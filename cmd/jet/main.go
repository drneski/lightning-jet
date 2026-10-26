// Command jet is Lightning Jet 2.0's command-line interface and daemon.
//
// This version observes a node read-only, records LND's HTLC and channel
// events as they happen, decides in shadow mode and records every decision.
// It changes nothing on the node.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
	"github.com/drneski/lightning-jet/v2/internal/engine"
	"github.com/drneski/lightning-jet/v2/internal/explain"
	"github.com/drneski/lightning-jet/v2/internal/health"
	"github.com/drneski/lightning-jet/v2/internal/ids"
	"github.com/drneski/lightning-jet/v2/internal/limits"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/observe"
	"github.com/drneski/lightning-jet/v2/internal/record"
	"github.com/drneski/lightning-jet/v2/internal/store"
	"github.com/drneski/lightning-jet/v2/internal/strategy/fee"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

// stuckAfter is how long a cycle may run before the watchdog stops vouching
// for Jet. Every LND call has its own 30-second deadline, so a healthy cycle
// is far shorter.
const stuckAfter = 15 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes one command and returns the process exit code: 0 on success,
// 1 on failure, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = cmdRun(ctx, args[1:], stderr)
	case "status":
		err = cmdStatus(ctx, args[1:], stdout, stderr)
	case "explain":
		err = cmdExplain(ctx, args[1:], stdout, stderr)
	case "health":
		err = cmdHealth(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "jet", version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "jet: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	var u usageError
	switch {
	case errors.As(err, &u):
		fmt.Fprintf(stderr, "jet %s: %v\n", args[0], err)
		return 2
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUnhealthy):
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "jet %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

type usageError struct{ error }

var errUnhealthy = errors.New("unhealthy")

func usage(w io.Writer) {
	fmt.Fprint(w, `Lightning Jet 2.0, in development. It observes a node read-only, records
LND's HTLC and channel events, decides in shadow mode and records every
decision. It changes nothing on the node.

Usage:
  jet <command> [flags]

Commands:
  run       Observe the node and record decisions, once or on an interval
  status    Summarize the most recent cycle and what is being recorded
  explain   Explain the latest decision about a channel
  health    Check that Jet is running and recording; exit 1 if not
  version   Print the version
  help      Show this help

Settings come from a config file (jet.conf; see contrib/jet.conf.example),
and flags override it. Run "jet <command> -h" for a command's flags.
`)
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	return nil
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("jet "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func cmdRun(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlags("run", stderr)
	c := addCommon(fs)
	address := fs.String("lnd", "", "LND's REST address, host:port (default localhost:8080)")
	cert := fs.String("tls-cert", "", "LND's tls.cert")
	macaroon := fs.String("macaroon", "", "LND's readonly.macaroon")
	interval := fs.Duration("interval", 0, "time between cycles (default 1h)")
	once := fs.Bool("once", false, "run one cycle and exit, without recording the event streams")
	feesMode := fs.String("fees-mode", "", "fees engine mode: shadow or off (default shadow)")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	// Flags given on the command line override the file.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "lnd":
			cfg.RESTAddress = *address
		case "tls-cert":
			cfg.TLSCert = *cert
		case "macaroon":
			cfg.Macaroon = *macaroon
		case "interval":
			cfg.Interval = *interval
		case "fees-mode":
			cfg.FeesMode = contracts.Mode(*feesMode)
		}
	})
	if err := cfg.Validate(); err != nil {
		return usageError{err}
	}
	if cfg.TLSCert == "" || cfg.Macaroon == "" {
		return usageError{errors.New("LND's tls.cert and readonly.macaroon are required: set tls_cert and macaroon in [lnd], or pass --tls-cert and --macaroon")}
	}
	if cfg.FeesMode == contracts.ModeOff {
		fmt.Fprintln(stderr, "jet run: the fees engine is off, so there is nothing to evaluate")
		return nil
	}

	client, err := lnd.NewClient(lnd.Config{Address: cfg.RESTAddress, TLSCertPath: cfg.TLSCert, MacaroonPath: cfg.Macaroon})
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, c.dbPath(cfg))
	if err != nil {
		return err
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(stderr, nil))
	gen := ids.New()
	now := time.Now
	lim := cfg.Limits
	lim.FeesMode = cfg.FeesMode
	strat := fee.DefaultConfig
	strat.Revisit = cfg.Interval
	e := &engine.Engine{
		Observer: &observe.Observer{LND: client, Now: now, NewID: gen.ID, Window: time.Duration(cfg.WindowDays) * 24 * time.Hour,
			CallTimeout: 30 * time.Second, Failures: st, GossipEvery: time.Duration(cfg.GossipRefreshHours) * time.Hour},
		Estimator:  &economics.Estimator{Horizon: 14 * 24 * time.Hour, Now: now, NewID: gen.ID},
		Strategy:   &fee.Strategy{Config: strat, Mode: cfg.FeesMode, Now: now, NewID: gen.ID},
		Authorizer: &limits.Authorizer{Config: lim, History: st, Now: now, NewID: gen.ID},
		Store:      st, Now: now, NewID: gen.ID, Log: log,
	}
	if *once {
		sum, err := e.Cycle(ctx)
		if err != nil {
			return err
		}
		log.Info("cycle recorded", "snapshot", sum.SnapshotID, "channels", sum.Channels, "intents", sum.Intents,
			"approved", sum.Approved, "rejected", sum.Rejected, "abstentions", sum.Abstentions)
		return nil
	}

	log.Info("jet running", "version", version, "lnd", cfg.RESTAddress, "interval", cfg.Interval.String(), "fees", cfg.FeesMode, "data", cfg.DataDir)
	rec := &record.Recorder{LND: client, Store: st, Now: now, Log: log,
		MinBackoff: time.Second, MaxBackoff: 5 * time.Minute, AliveEvery: time.Minute}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); rec.Run(ctx) }()
	if every := health.WatchdogInterval(); every > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); watchdog(ctx, e, every, log) }()
	}
	if err := health.Notify("READY=1"); err != nil {
		log.Warn("telling systemd Jet is ready", "err", err)
	}
	err = e.Run(ctx, cfg.Interval)
	_ = health.Notify("STOPPING=1")
	wg.Wait()
	return err
}

// watchdog pings systemd while the engine is healthy. If a cycle hangs, the
// pings stop and systemd restarts Jet.
func watchdog(ctx context.Context, e *engine.Engine, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.Stuck(time.Now(), stuckAfter) {
			log.Error("a cycle has run too long; withholding the watchdog ping so systemd restarts Jet")
			continue
		}
		if err := health.Notify("WATCHDOG=1"); err != nil {
			log.Warn("pinging systemd's watchdog", "err", err)
		}
	}
}

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("status", stderr)
	c := addCommon(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	st, err := openExisting(ctx, c.dbPath(cfg))
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.LatestStatus(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("no cycle recorded yet; run jet run first")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Last cycle   %s (snapshot %s)\n", s.TakenAt.Format("2006-01-02 15:04 MST"), s.SnapshotID)
	fmt.Fprintf(stdout, "Channels     %d\n", s.Channels)
	fmt.Fprintf(stdout, "Proposed     %d (%d approved, %d rejected), shadow mode: nothing was executed\n", s.Approved+s.Rejected, s.Approved, s.Rejected)
	total := 0
	reasons := make([]string, 0, len(s.Abstentions))
	for r, n := range s.Abstentions {
		total += n
		reasons = append(reasons, fmt.Sprintf("%s %d", strings.ReplaceAll(string(r), "_", " "), n))
	}
	sort.Strings(reasons)
	fmt.Fprintf(stdout, "Abstained    %d", total)
	if total > 0 {
		fmt.Fprintf(stdout, " (%s)", strings.Join(reasons, ", "))
	}
	fmt.Fprintln(stdout)

	now := time.Now().UTC()
	day := now.Add(-24 * time.Hour)
	fails, err := st.BalanceFailuresSince(ctx, day)
	if err != nil {
		return err
	}
	n := 0
	for _, f := range fails {
		n += f.Count
	}
	for _, stream := range []string{record.StreamHTLC, record.StreamChannels} {
		cov, err := st.Coverage(ctx, stream, day, now)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("Recording    %s stream: %s of the last 24 hours", stream, cov.Round(time.Minute))
		if stream == record.StreamHTLC {
			line += fmt.Sprintf(", %d forwards failed for lack of balance", n)
		}
		fmt.Fprintln(stdout, line)
	}

	if len(s.PerChannel) > 0 {
		fmt.Fprintln(stdout, "\nChannel              Local  Result")
	}
	for _, c := range s.PerChannel {
		local := "-"
		if c.CapacityMsat > 0 {
			local = fmt.Sprintf("%d%%", int64(c.LocalMsat)*100/int64(c.CapacityMsat))
		}
		result := "no decision recorded"
		switch {
		case c.Abstained != "":
			result = "abstained: " + strings.ReplaceAll(string(c.Abstained), "_", " ")
		case c.Proposal != nil:
			result = fmt.Sprintf("proposed %d to %d ppm: %s", c.Proposal.ObservedFeePPM, c.Proposal.FeePPM, c.Verdict)
		}
		fmt.Fprintf(stdout, "%-20s %5s  %s\n", c.ChanID, local, result)
	}
	return nil
}

func cmdExplain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("explain", stderr)
	c := addCommon(fs)
	asJSON := fs.Bool("json", false, "print the explanation as JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError{errors.New("expected one channel id")}
	}
	chanID, err := contracts.ParseChanID(fs.Arg(0))
	if err != nil {
		return usageError{err}
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	st, err := openExisting(ctx, c.dbPath(cfg))
	if err != nil {
		return err
	}
	defer st.Close()
	ev, err := st.LatestEvaluation(ctx, chanID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no decision recorded for channel %s", chanID)
	}
	if err != nil {
		return err
	}
	x, err := explain.Build(ev)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(x)
	}
	fmt.Fprint(stdout, x.Text())
	return nil
}

func cmdHealth(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("health", stderr)
	c := addCommon(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	st, err := openExisting(ctx, c.dbPath(cfg))
	if err != nil {
		return err
	}
	defer st.Close()
	msMeta := func(key string) time.Time {
		v, err := st.Meta(ctx, key)
		if err != nil {
			return time.Time{}
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.UnixMilli(n).UTC()
	}
	interval := cfg.Interval
	if v, err := st.Meta(ctx, health.KeyInterval); err == nil {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			interval = time.Duration(n) * time.Millisecond
		}
	}
	streams := map[string]time.Time{}
	for _, s := range []string{record.StreamHTLC, record.StreamChannels} {
		if at := msMeta(record.AliveKey(s)); !at.IsZero() {
			streams[s] = at
		}
	}
	healthy := true
	for _, ch := range health.Judge(time.Now().UTC(), msMeta(health.KeyCycleOK), interval, streams) {
		mark := "ok  "
		if !ch.OK {
			mark, healthy = "FAIL", false
		}
		fmt.Fprintf(stdout, "%s  %-16s %s\n", mark, ch.Name, ch.Detail)
	}
	if failedAt := msMeta(health.KeyCycleFail); !failedAt.IsZero() && failedAt.After(msMeta(health.KeyCycleOK)) {
		reason, _ := st.Meta(ctx, health.KeyCycleErr)
		fmt.Fprintf(stdout, "      the last cycle failed %s ago: %s\n", time.Since(failedAt).Round(time.Second), reason)
	}
	if !healthy {
		return errUnhealthy
	}
	return nil
}

// openExisting opens the database without creating one where none exists.
func openExisting(ctx context.Context, path string) (*store.Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no database at %s; run jet run first", path)
	}
	return store.Open(ctx, path)
}

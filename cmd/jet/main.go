// Command jet is Lightning Jet 2.0's command-line interface and daemon.
//
// This version observes a node read-only, decides in shadow mode and records
// every decision. It changes nothing on the node.
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
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
	"github.com/drneski/lightning-jet/v2/internal/engine"
	"github.com/drneski/lightning-jet/v2/internal/explain"
	"github.com/drneski/lightning-jet/v2/internal/ids"
	"github.com/drneski/lightning-jet/v2/internal/limits"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/observe"
	"github.com/drneski/lightning-jet/v2/internal/store"
	"github.com/drneski/lightning-jet/v2/internal/strategy/fee"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

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
	case err != nil:
		fmt.Fprintf(stderr, "jet %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

type usageError struct{ error }

func usage(w io.Writer) {
	fmt.Fprint(w, `Lightning Jet 2.0, in development. It observes a node read-only, decides in
shadow mode and records every decision. It changes nothing on the node.

Usage:
  jet <command> [flags]

Commands:
  run       Observe the node and record decisions, once or on an interval
  status    Summarize the most recent cycle
  explain   Explain the latest decision about a channel
  version   Print the version
  help      Show this help

Run "jet <command> -h" for a command's flags.
`)
}

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("jet "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for Jet's database")
	return fs, dataDir
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

func cmdRun(ctx context.Context, args []string, stderr io.Writer) error {
	fs, dataDir := newFlags("run", stderr)
	address := fs.String("lnd", "localhost:8080", "LND's REST address, host:port")
	cert := fs.String("tls-cert", "", "LND's tls.cert (required)")
	macaroon := fs.String("macaroon", "", "LND's readonly.macaroon (required)")
	interval := fs.Duration("interval", time.Hour, "time between cycles")
	once := fs.Bool("once", false, "run one cycle and exit")
	feesMode := fs.String("fees-mode", string(contracts.ModeShadow), "fees engine mode: shadow or off")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case *cert == "" || *macaroon == "":
		return usageError{errors.New("--tls-cert and --macaroon are required")}
	case *interval < time.Minute:
		return usageError{errors.New("--interval must be at least a minute")}
	case *feesMode == string(contracts.ModeLive):
		return usageError{engine.ErrLiveUnsupported}
	case *feesMode != string(contracts.ModeShadow) && *feesMode != string(contracts.ModeOff):
		return usageError{fmt.Errorf("--fees-mode must be shadow or off, not %q", *feesMode)}
	}
	if *feesMode == string(contracts.ModeOff) {
		fmt.Fprintln(stderr, "jet run: the fees engine is off, so there is nothing to evaluate")
		return nil
	}

	client, err := lnd.NewClient(lnd.Config{Address: *address, TLSCertPath: *cert, MacaroonPath: *macaroon})
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(*dataDir, "jet.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	gen := ids.New()
	now := time.Now
	mode := contracts.Mode(*feesMode)
	lim := limits.DefaultConfig
	lim.FeesMode = mode
	strat := fee.DefaultConfig
	strat.Revisit = *interval
	e := &engine.Engine{
		Observer:   &observe.Observer{LND: client, Now: now, NewID: gen.ID, Window: 30 * 24 * time.Hour, CallTimeout: 30 * time.Second},
		Estimator:  &economics.Estimator{Horizon: 14 * 24 * time.Hour, Now: now, NewID: gen.ID},
		Strategy:   &fee.Strategy{Config: strat, Mode: mode, Now: now, NewID: gen.ID},
		Authorizer: &limits.Authorizer{Config: lim, History: st, Now: now, NewID: gen.ID},
		Store:      st, Now: now, NewID: gen.ID,
		Log: slog.New(slog.NewTextHandler(stderr, nil)),
	}
	if *once {
		sum, err := e.Cycle(ctx)
		if err != nil {
			return err
		}
		e.Log.Info("cycle recorded", "snapshot", sum.SnapshotID, "channels", sum.Channels, "intents", sum.Intents,
			"approved", sum.Approved, "rejected", sum.Rejected, "abstentions", sum.Abstentions)
		return nil
	}
	e.Log.Info("jet running", "version", version, "lnd", *address, "interval", interval.String(), "fees", mode)
	return e.Run(ctx, *interval)
}

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, dataDir := newFlags("status", stderr)
	if err := parse(fs, args); err != nil {
		return err
	}
	st, err := openExisting(ctx, *dataDir)
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
	fs, dataDir := newFlags("explain", stderr)
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
	st, err := openExisting(ctx, *dataDir)
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

// openExisting opens the database without creating one where none exists.
func openExisting(ctx context.Context, dataDir string) (*store.Store, error) {
	path := filepath.Join(dataDir, "jet.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no database at %s; run jet run first", path)
	}
	return store.Open(ctx, path)
}

// defaultDataDir is where Jet keeps its database: $XDG_DATA_HOME/lightning-jet,
// or the platform's usual place for application data.
func defaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "lightning-jet")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "lightning-jet"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "lightning-jet")
	}
	return filepath.Join(home, ".local", "share", "lightning-jet")
}

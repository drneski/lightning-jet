package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
	"github.com/drneski/lightning-jet/v2/internal/health"
	"github.com/drneski/lightning-jet/v2/internal/ids"
	"github.com/drneski/lightning-jet/v2/internal/limits"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/lnd/lndtest"
	"github.com/drneski/lightning-jet/v2/internal/observe"
	"github.com/drneski/lightning-jet/v2/internal/store"
	"github.com/drneski/lightning-jet/v2/internal/strategy/fee"
	"github.com/drneski/lightning-jet/v2/internal/strategy/refill"
)

// Our node, and a peer for each channel.
const us = "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var peers = map[contracts.ChanID]string{
	fullBusy:  "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	balanced:  "03cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	fullQuiet: "03dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
}

// Synthetic channels, all funded long before the forwards window.
const (
	fullBusy  = contracts.ChanID(800000<<40 | 1) // 92% local, 12 forwards out: an experiment
	balanced  = contracts.ChanID(800000<<40 | 2) // 50% local: nothing to do
	fullQuiet = contracts.ChanID(800000<<40 | 3) // 90% local, 3 forwards out: too little evidence
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func fakeNode(now time.Time) *lndtest.Fake {
	policy := func(ppm int64) *lnd.RoutingPolicy {
		return &lnd.RoutingPolicy{FeeRateMilliMsat: ppm, TimeLockDelta: 80}
	}
	f := &lndtest.Fake{
		Info: lnd.Info{IdentityPubkey: us, BlockHeight: 900000, SyncedToChain: true, SyncedToGraph: true},
		Channels: []lnd.Channel{
			{ChanID: fullBusy, RemotePubkey: peers[fullBusy], Active: true, CapacitySat: 5_000_000, LocalSat: 4_600_000, RemoteSat: 390_000},
			{ChanID: balanced, RemotePubkey: peers[balanced], Active: true, CapacitySat: 2_000_000, LocalSat: 1_000_000, RemoteSat: 990_000},
			{ChanID: fullQuiet, RemotePubkey: peers[fullQuiet], Active: true, CapacitySat: 5_000_000, LocalSat: 4_500_000, RemoteSat: 490_000},
		},
		Edges: map[contracts.ChanID]lnd.Edge{},
	}
	for _, ch := range f.Channels {
		f.Edges[ch.ChanID] = lnd.Edge{ChanID: ch.ChanID, Node1Pub: us, Node2Pub: ch.RemotePubkey, Node1Policy: policy(500), Node2Policy: policy(400)}
	}
	add := func(ch contracts.ChanID, n int) {
		for i := 0; i < n; i++ {
			f.Forwards = append(f.Forwards, lnd.Forward{Time: now.Add(-time.Duration(i+1) * time.Hour), ChanIDIn: balanced, ChanIDOut: ch,
				AmtInMsat: 100_050_000, AmtOutMsat: 100_000_000, FeeMsat: 50_000})
		}
	}
	add(fullBusy, 12)
	add(fullQuiet, 3)
	return f
}

func newEngine(t *testing.T, reader lnd.Reader, c *clock) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gen := &ids.Generator{Now: c.now, Rand: rand.NewChaCha8([32]byte{1})}
	return &Engine{
		Observer:   &observe.Observer{LND: reader, Now: c.now, NewID: gen.ID, Window: 30 * 24 * time.Hour, CallTimeout: time.Second},
		Estimator:  &economics.Estimator{Horizon: 14 * 24 * time.Hour, Now: c.now, NewID: gen.ID},
		Strategy:   &fee.Strategy{Config: fee.DefaultConfig, Mode: contracts.ModeShadow, Now: c.now, NewID: gen.ID},
		Refills:    &refill.Strategy{Config: refill.DefaultConfig, Mode: contracts.ModeShadow, Now: c.now, NewID: gen.ID},
		Authorizer: &limits.Authorizer{Config: limits.DefaultConfig, History: st, Now: c.now, NewID: gen.ID},
		Store:      st, Now: c.now, NewID: gen.ID, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st
}

func TestCycleRecordsAnIntentOrAnAbstentionForEveryChannel(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	e, st := newEngine(t, fakeNode(c.t), c)
	ctx := context.Background()

	sum, err := e.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Channels != 3 || sum.Intents != 1 || sum.Approved != 1 || sum.Abstentions != 2 {
		t.Fatalf("summary = %+v", sum)
	}

	busy, err := st.LatestEvaluation(ctx, fullBusy)
	if err != nil {
		t.Fatal(err)
	}
	if busy.Intent == nil || busy.Intent.Parameters.Fee.FeePPM != 450 || busy.Decision.Verdict != contracts.Approved {
		t.Fatalf("busy channel: intent %+v, decision %+v", busy.Intent, busy.Decision)
	}
	if len(busy.Outcomes) != 1 || busy.Outcomes[0].State != contracts.NotExecuted || !strings.Contains(busy.Outcomes[0].Detail, "shadow") {
		t.Errorf("busy channel outcomes = %+v", busy.Outcomes)
	}
	if err := busy.Snapshot.Validate(); err != nil {
		t.Errorf("snapshot read back: %v", err)
	}
	// Each channel is estimated twice: what it earns, and what refilling it
	// costs.
	estimates, err := st.Estimates(ctx, sum.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, est := range estimates {
		kinds[est.Kind]++
	}
	if len(estimates) != 2*sum.Channels || kinds[economics.KindOutboundRevenue] != sum.Channels || kinds[economics.KindReplacementCost] != sum.Channels {
		t.Errorf("estimates by kind = %v for %d channels", kinds, sum.Channels)
	}
	// The intent rests on both.
	if len(busy.Intent.Rationale.EstimateIDs) != 2 {
		t.Errorf("busy channel's intent rests on %v", busy.Intent.Rationale.EstimateIDs)
	}

	for ch, want := range map[contracts.ChanID]contracts.AbstentionReason{balanced: contracts.NoProfitableAction, fullQuiet: contracts.InsufficientEvidence} {
		ev, err := st.LatestEvaluation(ctx, ch)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Abstention == nil || ev.Abstention.Reason != want {
			t.Errorf("channel %s: abstention %+v, want %s", ch, ev.Abstention, want)
		}
	}
}

func TestShadowApprovalsCountAgainstTheCooldown(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	e, st := newEngine(t, fakeNode(c.t), c)
	ctx := context.Background()
	if _, err := e.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Hour) // nothing was executed, so the same change is proposed again
	sum, err := e.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rejected != 1 || sum.Approved != 0 {
		t.Fatalf("second cycle = %+v", sum)
	}
	ev, err := st.LatestEvaluation(ctx, fullBusy)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Decision.Verdict != contracts.Rejected || !strings.Contains(ev.Decision.Reason, "cooldown") {
		t.Errorf("decision = %+v", ev.Decision)
	}
}

func TestLiveModeIsRefused(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	e, _ := newEngine(t, fakeNode(c.t), c)
	e.Strategy.Mode = contracts.ModeLive
	if _, err := e.Cycle(context.Background()); !errors.Is(err, ErrLiveUnsupported) {
		t.Errorf("err = %v, want ErrLiveUnsupported", err)
	}
	if err := e.Run(context.Background(), time.Hour); !errors.Is(err, ErrLiveUnsupported) {
		t.Errorf("Run err = %v, want ErrLiveUnsupported", err)
	}
}

func TestAFailedCycleRecordsNothing(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	f := fakeNode(c.t)
	f.Errors = map[string]error{"ListChannels": errors.New("lnd is down")}
	e, st := newEngine(t, f, c)
	if _, err := e.Cycle(context.Background()); err == nil {
		t.Fatal("a cycle without channels succeeded")
	}
	if _, err := st.LatestStatus(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("status after a failed cycle: err = %v, want ErrNotFound", err)
	}
}

func TestRunStopsWhenCanceled(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	e, st := newEngine(t, fakeNode(c.t), c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.Run(ctx, time.Hour) }()
	// Wait for the first cycle to be recorded, then stop.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := st.LatestStatus(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first cycle was never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		if v, err := st.Meta(context.Background(), health.KeyCycleOK); err == nil && v != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat was recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v, _ := st.Meta(context.Background(), health.KeyInterval); v != "3600000" {
		t.Errorf("interval heartbeat = %q", v)
	}
	if e.Stuck(c.now(), time.Minute) {
		t.Error("an idle engine reports itself stuck")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestStuckOnlyWhileACycleRunsTooLong(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	e, _ := newEngine(t, fakeNode(c.t), c)
	e.busySince.Store(c.t.UnixMilli())
	if e.Stuck(c.t.Add(time.Minute), 15*time.Minute) {
		t.Error("a one-minute cycle is reported stuck")
	}
	if !e.Stuck(c.t.Add(16*time.Minute), 15*time.Minute) {
		t.Error("a sixteen-minute cycle is not reported stuck")
	}
}

// A depleted channel gets a refill evaluation as well as a fee one, and the
// two are recorded apart. Without the gossip graph there is no way to price a
// refill, so here it is an abstention.
func TestADepletedChannelIsEvaluatedForARefill(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	f := fakeNode(c.t)
	f.Channels[0].LocalSat, f.Channels[0].RemoteSat = 100_000, 4_890_000 // fullBusy, now 2% local
	e, st := newEngine(t, f, c)
	ctx := context.Background()
	sum, err := e.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Refills != 1 || sum.RefillsApproved != 0 || sum.Intents != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	status, err := st.LatestStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Refills) != 1 || status.Refills[0].ChanID != fullBusy || status.Refills[0].Abstention == nil ||
		status.Refills[0].Abstention.Reason != contracts.InsufficientEvidence || status.Approved+status.Rejected != 1 {
		t.Errorf("status = %+v, refills %+v", status, status.Refills)
	}
}

// A second channel to a peer joins the first: one evaluation of the two, on
// their combined balance, recorded under the older channel with both listed.
func TestAPeersChannelsAreEvaluatedAsOne(t *testing.T) {
	c := &clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	f := fakeNode(c.t)
	second := contracts.ChanID(850000<<40 | 1)
	f.Channels = append(f.Channels, lnd.Channel{ChanID: second, RemotePubkey: peers[fullBusy], Active: true,
		CapacitySat: 5_000_000, LocalSat: 500_000, RemoteSat: 4_490_000})
	f.Edges[second] = lnd.Edge{ChanID: second, Node1Pub: us, Node2Pub: peers[fullBusy],
		Node1Policy: &lnd.RoutingPolicy{FeeRateMilliMsat: 500}, Node2Policy: &lnd.RoutingPolicy{FeeRateMilliMsat: 400}}
	e, st := newEngine(t, f, c)
	ctx := context.Background()
	sum, err := e.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 5.1M of 10M sats between them is inside the band.
	if sum.Channels != 4 || sum.Intents != 0 || sum.Abstentions != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	ev, err := st.LatestEvaluation(ctx, fullBusy)
	if err != nil {
		t.Fatal(err)
	}
	a := ev.Abstention
	if a == nil || !strings.Contains(a.Detail, "local balance 51% is inside") || len(a.ChanIDs) != 2 || a.ChanIDs[1] != second {
		t.Fatalf("abstention = %+v", a)
	}
	for _, est := range ev.Estimates {
		if len(est.ChanIDs) != 2 {
			t.Errorf("estimate %s covers %v", est.Kind, est.ChanIDs)
		}
	}
}

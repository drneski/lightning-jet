package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

var t0 = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "jet.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mp(m contracts.MilliSat) *contracts.MilliSat { return &m }

// sampleCycle is a synthetic cycle: channel 1 gets an approved shadow intent,
// channel 2 an abstention.
func sampleCycle(t *testing.T, suffix string, at time.Time, mode contracts.Mode) Cycle {
	t.Helper()
	snap := &contracts.Snapshot{ID: "s" + suffix, Schema: contracts.SchemaSnapshot, NodePubkey: "02aa", BlockHeight: 900000, TakenAt: at,
		Channels: []contracts.ChannelObservation{
			{ChanID: 1, Peer: "03p1", CapacityMsat: 5_000_000_000, LocalMsat: 4_600_000_000, DataQuality: contracts.DataQuality{Level: contracts.DataGood}},
			{ChanID: 2, Peer: "03p2", CapacityMsat: 2_000_000_000, LocalMsat: 1_000_000_000, DataQuality: contracts.DataQuality{Level: contracts.DataGood}},
		}}
	if err := snap.Seal(); err != nil {
		t.Fatal(err)
	}
	est := func(id string, ch contracts.ChanID) *contracts.Estimate {
		return &contracts.Estimate{ID: id, Schema: contracts.SchemaEstimate, ChanID: ch, Kind: "outbound_revenue", SnapshotID: snap.ID,
			Estimator: contracts.Component{Name: "flow", Version: "0.1.0"}, HorizonHours: 336,
			ExpectedMsat: mp(1000), LowMsat: mp(500), HighMsat: mp(1500), Interval: "p10-p90",
			EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceMedium, Basis: "12 forwards"},
			DataQuality:        contracts.DataQuality{Level: contracts.DataGood}, ComputedAt: at}
	}
	e1, e2 := est("e1"+suffix, 1), est("e2"+suffix, 2)
	intent := &contracts.Intent{ID: "i" + suffix, Schema: contracts.SchemaIntent, Action: contracts.ActionFeeUpdate,
		Target:     contracts.Target{ChanID: 1},
		Parameters: contracts.IntentParameters{Fee: &contracts.FeeParameters{FeePPM: 450, ObservedFeePPM: 500}},
		Rationale:  contracts.Rationale{EstimateIDs: []string{e1.ID}, Summary: "experiment"},
		Strategy:   contracts.Component{Name: "fee.balance-band", Version: "0.1.0"}, SnapshotID: snap.ID, Mode: mode,
		CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour)}
	decision := &contracts.Decision{ID: "d" + suffix, Schema: contracts.SchemaDecision, IntentID: intent.ID, SnapshotID: snap.ID,
		Verdict: contracts.Approved, Rules: []contracts.RuleResult{{Rule: "mode", Passed: true, Detail: "fees in shadow"}},
		PolicyVersion: "v1", Authorizer: "standalone", Mode: mode, DecidedAt: at}
	outcome := &contracts.Outcome{ID: "o" + suffix, Schema: contracts.SchemaOutcome, IntentID: intent.ID, State: contracts.NotExecuted, Detail: "shadow mode", At: at}
	abst := &contracts.Abstention{ID: "a" + suffix, Schema: contracts.SchemaAbstention, ChanID: 2,
		Strategy: contracts.Component{Name: "fee.balance-band", Version: "0.1.0"}, SnapshotID: snap.ID, EstimateIDs: []string{e2.ID},
		Reason: contracts.NoProfitableAction, Detail: "inside the band", WouldChangeIf: []string{"balance outside 20-80%"},
		RevisitAt: at.Add(time.Hour), CreatedAt: at}
	return Cycle{Snapshot: snap, Estimates: []*contracts.Estimate{e1, e2}, Intents: []*contracts.Intent{intent},
		Abstentions: []*contracts.Abstention{abst}, Decisions: []*contracts.Decision{decision}, Outcomes: []*contracts.Outcome{outcome}}
}

func TestSaveAndReadBack(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.SaveCycle(ctx, sampleCycle(t, "1", t0, contracts.ModeShadow)); err != nil {
		t.Fatal(err)
	}

	ev, err := s.LatestEvaluation(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Intent == nil || ev.Intent.ID != "i1" || ev.Abstention != nil {
		t.Fatalf("channel 1: got intent %v, abstention %v", ev.Intent, ev.Abstention)
	}
	if ev.Decision == nil || ev.Decision.Verdict != contracts.Approved || len(ev.Outcomes) != 1 || ev.Outcomes[0].State != contracts.NotExecuted {
		t.Errorf("channel 1: decision %+v, outcomes %+v", ev.Decision, ev.Outcomes)
	}
	if len(ev.Estimates) != 1 || ev.Estimates[0].ID != "e11" {
		t.Errorf("channel 1: estimates %+v", ev.Estimates)
	}
	if err := ev.Snapshot.Validate(); err != nil {
		t.Errorf("snapshot read back does not match its hash: %v", err)
	}

	ev2, err := s.LatestEvaluation(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Abstention == nil || ev2.Abstention.Reason != contracts.NoProfitableAction {
		t.Errorf("channel 2: got %+v", ev2.Abstention)
	}

	if _, err := s.LatestEvaluation(ctx, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown channel: err = %v, want ErrNotFound", err)
	}

	st, err := s.LatestStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.SnapshotID != "s1" || st.Channels != 2 || st.Approved != 1 || st.Rejected != 0 || st.Abstentions[contracts.NoProfitableAction] != 1 {
		t.Errorf("status = %+v", st)
	}
	if len(st.PerChannel) != 2 || st.PerChannel[0].Verdict != contracts.Approved || st.PerChannel[0].Proposal.FeePPM != 450 ||
		st.PerChannel[1].Abstained != contracts.NoProfitableAction {
		t.Errorf("per channel = %+v", st.PerChannel)
	}
}

func TestLatestEvaluationIsTheNewest(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	for i, at := range []time.Time{t0, t0.Add(time.Hour)} {
		if err := s.SaveCycle(ctx, sampleCycle(t, string(rune('a'+i)), at, contracts.ModeShadow)); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := s.LatestEvaluation(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Intent.ID != "ib" {
		t.Errorf("latest intent = %s, want ib", ev.Intent.ID)
	}
}

// A proposal's prior approvals are those in its mode in the 24 hours before
// it, not counting itself.
func TestLatestStatusCountsPriorApprovals(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	for i, at := range []time.Time{t0.Add(-25 * time.Hour), t0.Add(-time.Hour), t0} {
		if err := s.SaveCycle(ctx, sampleCycle(t, string(rune('a'+i)), at, contracts.ModeShadow)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveCycle(ctx, sampleCycle(t, "l", t0.Add(-2*time.Hour), contracts.ModeLive)); err != nil {
		t.Fatal(err)
	}
	st, err := s.LatestStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := st.PerChannel[0]; c.Mode != contracts.ModeShadow || c.PriorApprovals != 1 {
		t.Errorf("channel 1: mode %s, prior approvals %d; want shadow, 1", c.Mode, c.PriorApprovals)
	}
}

func TestApprovedFeeChangesKeepsLedgersApart(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.SaveCycle(ctx, sampleCycle(t, "s", t0, contracts.ModeShadow)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCycle(ctx, sampleCycle(t, "l", t0.Add(time.Hour), contracts.ModeLive)); err != nil {
		t.Fatal(err)
	}
	shadow, err := s.ApprovedFeeChanges(ctx, 1, contracts.ModeShadow, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(shadow) != 1 || !shadow[0].Equal(t0) {
		t.Errorf("shadow approvals = %v", shadow)
	}
	recent, err := s.ApprovedFeeChanges(ctx, 1, contracts.ModeShadow, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 0 {
		t.Errorf("approvals before since were returned: %v", recent)
	}
}

func TestAnInvalidCycleWritesNothing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	c := sampleCycle(t, "1", t0, contracts.ModeShadow)
	c.Abstentions[0].WouldChangeIf = nil // invalid: an abstention must say what would change it
	if err := s.SaveCycle(ctx, c); err == nil {
		t.Fatal("an invalid cycle was saved")
	}
	if _, err := s.LatestStatus(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("after a refused cycle, status err = %v, want ErrNotFound", err)
	}
}

func TestTamperedSnapshotIsRefused(t *testing.T) {
	s, _ := openTemp(t)
	c := sampleCycle(t, "1", t0, contracts.ModeShadow)
	c.Snapshot.Channels[0].LocalMsat++ // after sealing
	if err := s.SaveCycle(context.Background(), c); err == nil {
		t.Error("a snapshot that no longer matches its hash was saved")
	}
}

func TestReopenKeepsSchemaAndRefusesANewerOne(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if err := s.SaveCycle(ctx, sampleCycle(t, "1", t0, contracts.ModeShadow)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.LatestStatus(ctx); err != nil {
		t.Errorf("records lost on reopen: %v", err)
	}
	if _, err := s2.db.ExecContext(ctx, `UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Error("a database from a newer Jet was opened")
	}
}

func TestDirectoryIsOwnerOnly(t *testing.T) {
	_, path := openTemp(t)
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("data directory permissions = %o, want 700", perm)
	}
}

func chanID(v uint64) contracts.ChanID { return contracts.ChanID(v) }
func msat(v int64) contracts.MilliSat  { return contracts.MilliSat(v) }

// withRefill adds a refill evaluation to each channel of a sample cycle: an
// approved refill of channel 2 from channel 1, and an abstention for channel
// 1, recorded after its fee intent.
func withRefill(t *testing.T, c Cycle, at time.Time, mode contracts.Mode) Cycle {
	t.Helper()
	src, max := contracts.ChanID(1), contracts.MilliSat(500_000)
	in := &contracts.Intent{ID: "r" + c.Snapshot.ID, Schema: contracts.SchemaIntent, Action: contracts.ActionRebalance,
		Target:     contracts.Target{ChanID: 2, SourceChanID: &src},
		Parameters: contracts.IntentParameters{Rebalance: &contracts.RebalanceParameters{AmountMsat: 1_000_000_000}},
		Limits:     contracts.IntentLimits{MaxFeeMsat: &max},
		Rationale:  contracts.Rationale{EstimateIDs: []string{c.Estimates[1].ID}, Summary: "refill"},
		Strategy:   contracts.Component{Name: "refill.value", Version: "0.1.0"}, SnapshotID: c.Snapshot.ID, Mode: mode,
		CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
	d := &contracts.Decision{ID: "rd" + c.Snapshot.ID, Schema: contracts.SchemaDecision, IntentID: in.ID, SnapshotID: c.Snapshot.ID,
		Verdict: contracts.Approved, Rules: []contracts.RuleResult{{Rule: "mode", Passed: true, Detail: "refills in shadow"}},
		PolicyVersion: "v1", Authorizer: "standalone", Mode: mode, DecidedAt: at}
	ab := &contracts.Abstention{ID: "ra" + c.Snapshot.ID, Schema: contracts.SchemaAbstention, ChanID: 1, Action: contracts.ActionRebalance,
		Strategy: contracts.Component{Name: "refill.value", Version: "0.1.0"}, SnapshotID: c.Snapshot.ID, EstimateIDs: []string{c.Estimates[0].ID},
		Reason: contracts.NoProfitableAction, Detail: "full", WouldChangeIf: []string{"it drains"},
		RevisitAt: at.Add(time.Hour), CreatedAt: at.Add(time.Second)}
	c.Intents = append(c.Intents, in)
	c.Decisions = append(c.Decisions, d)
	c.Abstentions = append(c.Abstentions, ab)
	return c
}

// Refill evaluations stay out of the fee readers and come back on their own.
func TestRefillsAreKeptApartFromFees(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.SaveCycle(ctx, withRefill(t, sampleCycle(t, "1", t0, contracts.ModeShadow), t0, contracts.ModeShadow)); err != nil {
		t.Fatal(err)
	}
	ev, err := s.LatestEvaluation(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Intent == nil || ev.Intent.Action != contracts.ActionFeeUpdate || ev.Abstention != nil {
		t.Errorf("channel 1's latest fee evaluation: intent %+v, abstention %+v", ev.Intent, ev.Abstention)
	}
	st, err := s.LatestStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Approved != 1 || st.Abstentions[contracts.NoProfitableAction] != 1 || st.PerChannel[1].Abstained != contracts.NoProfitableAction ||
		st.PerChannel[0].Proposal == nil {
		t.Errorf("fee status = %+v", st)
	}
	if len(st.Refills) != 2 || st.Refills[0].ChanID != 1 || st.Refills[0].Abstention == nil ||
		st.Refills[1].ChanID != 2 || st.Refills[1].Intent == nil || st.Refills[1].Decision.Verdict != contracts.Approved {
		t.Errorf("refills = %+v", st.Refills)
	}
	approved, err := s.ApprovedRefills(ctx, contracts.ModeShadow, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].Target.ChanID != 2 || *approved[0].Limits.MaxFeeMsat != 500_000 || !approved[0].CreatedAt.Equal(t0) {
		t.Errorf("approved refills = %+v", approved)
	}
	if live, _ := s.ApprovedRefills(ctx, contracts.ModeLive, t0.Add(-time.Hour)); len(live) != 0 {
		t.Errorf("live refills = %+v", live)
	}
	if fees, _ := s.ApprovedFeeChanges(ctx, 2, contracts.ModeShadow, t0.Add(-time.Hour)); len(fees) != 0 {
		t.Errorf("a refill counted as a fee change: %v", fees)
	}
}

// A channel's refill evaluation is read by cycle: the intent or abstention,
// with its decision.
func TestRefillEvaluation(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.SaveCycle(ctx, withRefill(t, sampleCycle(t, "1", t0, contracts.ModeShadow), t0, contracts.ModeShadow)); err != nil {
		t.Fatal(err)
	}
	ev, err := s.RefillEvaluation(ctx, 2, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Intent == nil || ev.Intent.Action != contracts.ActionRebalance || ev.Decision == nil || ev.Decision.Verdict != contracts.Approved ||
		ev.Snapshot.ID != "s1" || len(ev.Estimates) != 1 {
		t.Errorf("channel 2's refill: %+v", ev)
	}
	ev, err = s.RefillEvaluation(ctx, 1, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Abstention == nil || ev.Abstention.Action != contracts.ActionRebalance || ev.Intent != nil {
		t.Errorf("channel 1's refill: %+v", ev)
	}
	if _, err := s.RefillEvaluation(ctx, 1, "elsewhen"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another cycle: err = %v, want ErrNotFound", err)
	}
}

// A peer's channels are one entry in the status, under the first of them,
// with their balances together.
func TestLatestStatusGroupsAPeersChannels(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	c := sampleCycle(t, "1", time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), contracts.ModeShadow)
	c.Snapshot.Channels[1].Peer = c.Snapshot.Channels[0].Peer
	if err := c.Snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	c.Abstentions, c.Estimates = nil, c.Estimates[:1]
	if err := s.SaveCycle(ctx, c); err != nil {
		t.Fatal(err)
	}
	st, err := s.LatestStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PerChannel) != 1 || st.Channels != 2 {
		t.Fatalf("status = %+v", st)
	}
	p := st.PerChannel[0]
	if p.ChanID != 1 || len(p.ChanIDs) != 2 || p.LocalMsat != 5_600_000_000 || p.CapacityMsat != 7_000_000_000 || p.Proposal == nil {
		t.Errorf("entry = %+v", p)
	}
}

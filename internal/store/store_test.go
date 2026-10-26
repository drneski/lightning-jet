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
			{ChanID: 1, CapacityMsat: 5_000_000_000, LocalMsat: 4_600_000_000, DataQuality: contracts.DataQuality{Level: contracts.DataGood}},
			{ChanID: 2, CapacityMsat: 2_000_000_000, LocalMsat: 1_000_000_000, DataQuality: contracts.DataQuality{Level: contracts.DataGood}},
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

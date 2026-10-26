package contracts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func TestChanIDJSON(t *testing.T) {
	id := ChanID(111111111111111111)
	b, err := json.Marshal(struct {
		C ChanID `json:"chan_id"`
	}{id})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"chan_id":"111111111111111111"}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}
	var back struct {
		C ChanID `json:"chan_id"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.C != id {
		t.Errorf("unmarshal = %v, %v; want %v", back.C, err, id)
	}
	if err := json.Unmarshal([]byte(`{"chan_id":111}`), &back); err == nil {
		t.Error("a channel id written as a JSON number was accepted")
	}
}

func TestChanIDFundingHeight(t *testing.T) {
	// Block 800000, transaction 12, output 1.
	id := ChanID(800000<<40 | 12<<16 | 1)
	if got := id.FundingHeight(); got != 800000 {
		t.Errorf("FundingHeight() = %d, want 800000", got)
	}
}

func TestLevels(t *testing.T) {
	if Worse(DataGood, DataDegraded) != DataDegraded || Worse(DataInsufficient, DataGood) != DataInsufficient {
		t.Error("Worse does not return the lower level")
	}
	if !ConfidenceHigh.AtLeast(ConfidenceMedium) || ConfidenceLow.AtLeast(ConfidenceMedium) || !ConfidenceNone.AtLeast(ConfidenceNone) {
		t.Error("AtLeast ranks levels wrongly")
	}
	q := DataQuality{Level: DataGood}
	q.Add(DataDegraded, "channel inactive")
	q.Add(DataGood, "noted")
	if q.Level != DataDegraded || len(q.Issues) != 2 {
		t.Errorf("Add gave %+v", q)
	}
}

func TestSnapshotSealDetectsTampering(t *testing.T) {
	s := &Snapshot{ID: "s1", Schema: SchemaSnapshot, NodePubkey: "02aa", TakenAt: t0,
		Channels: []ChannelObservation{{ChanID: 1, LocalMsat: 1000, DataQuality: DataQuality{Level: DataGood}}}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not sealed") {
		t.Errorf("unsealed snapshot: Validate() = %v", err)
	}
	if err := s.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("sealed snapshot: Validate() = %v", err)
	}
	s.Channels[0].LocalMsat = 2000
	if err := s.Validate(); err == nil {
		t.Error("a changed snapshot still validated")
	}
}

func ptr(m MilliSat) *MilliSat { return &m }

func validEstimate() Estimate {
	return Estimate{ID: "e1", Schema: SchemaEstimate, ChanID: 1, Kind: "outbound_revenue", SnapshotID: "s1",
		Estimator: Component{"flow", "0.1.0"}, HorizonHours: 336,
		ExpectedMsat: ptr(100), LowMsat: ptr(50), HighMsat: ptr(150), Interval: "p10-p90",
		EconomicConfidence: EconomicConfidence{ConfidenceMedium, "12 forwards"}, DataQuality: DataQuality{Level: DataGood}, ComputedAt: t0}
}

func TestEstimateValidate(t *testing.T) {
	e := validEstimate()
	if err := e.Validate(); err != nil {
		t.Fatalf("valid estimate: %v", err)
	}
	none := validEstimate()
	none.EconomicConfidence.Level = ConfidenceNone
	if err := none.Validate(); err == nil {
		t.Error("an estimate at confidence none with a value validated")
	}
	none.ExpectedMsat, none.LowMsat, none.HighMsat, none.Interval = nil, nil, nil, ""
	if err := none.Validate(); err != nil {
		t.Errorf("an estimate at confidence none without a value: %v", err)
	}
	outside := validEstimate()
	outside.ExpectedMsat = ptr(200)
	if err := outside.Validate(); err == nil {
		t.Error("an expected value outside its range validated")
	}
}

func validIntent() Intent {
	return Intent{ID: "i1", Schema: SchemaIntent, Action: ActionFeeUpdate, Target: Target{ChanID: 1},
		Parameters: IntentParameters{Fee: &FeeParameters{FeePPM: 450, ObservedFeePPM: 500}},
		Rationale:  Rationale{EstimateIDs: []string{"e1"}, Summary: "experiment"}, Strategy: Component{"fee.balance-band", "0.1.0"},
		SnapshotID: "s1", Mode: ModeShadow, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
}

func TestIntentValidate(t *testing.T) {
	i := validIntent()
	if err := i.Validate(); err != nil {
		t.Fatalf("valid intent: %v", err)
	}
	off := validIntent()
	off.Mode = ModeOff
	if err := off.Validate(); err == nil {
		t.Error("an intent in mode off validated")
	}
	noFee := validIntent()
	noFee.Parameters.Fee = nil
	if err := noFee.Validate(); err == nil {
		t.Error("a fee_update without fee parameters validated")
	}
	stale := validIntent()
	stale.ExpiresAt = stale.CreatedAt
	if err := stale.Validate(); err == nil {
		t.Error("an intent that expires when it is created validated")
	}
}

func TestAbstentionValidate(t *testing.T) {
	a := Abstention{ID: "a1", Schema: SchemaAbstention, ChanID: 1, Strategy: Component{"fee.balance-band", "0.1.0"},
		SnapshotID: "s1", Reason: InsufficientEvidence, Detail: "3 forwards", WouldChangeIf: []string{"10 forwards"},
		RevisitAt: t0.Add(time.Hour), CreatedAt: t0}
	if err := a.Validate(); err != nil {
		t.Fatalf("valid abstention: %v", err)
	}
	a.Reason = "bored"
	if err := a.Validate(); err == nil {
		t.Error("an abstention with an unknown reason validated")
	}
}

func TestDecisionVerdictMustMatchRules(t *testing.T) {
	d := Decision{ID: "d1", Schema: SchemaDecision, IntentID: "i1", Verdict: Approved, PolicyVersion: "v",
		Rules: []RuleResult{{Rule: "a", Passed: true}, {Rule: "b", Passed: false, Detail: "no"}}}
	if err := d.Validate(); err == nil {
		t.Error("an approval with a failed rule validated")
	}
	d.Verdict, d.Reason = Rejected, "b: no"
	if err := d.Validate(); err != nil {
		t.Errorf("a rejection with a failed rule: %v", err)
	}
}

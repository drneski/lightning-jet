package fee

import (
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/peer"
)

var now = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func strategy() *Strategy {
	n := 0
	return &Strategy{Config: DefaultConfig, Mode: contracts.ModeShadow, Now: func() time.Time { return now },
		NewID: func() string { n++; return "x" + string(rune('0'+n)) }}
}

func mp(m contracts.MilliSat) *contracts.MilliSat { return &m }

func channel(localPct int64, ppm int64) contracts.ChannelObservation {
	return contracts.ChannelObservation{ChanID: 1, CapacityMsat: 5_000_000_000, LocalMsat: contracts.MilliSat(50_000_000 * localPct),
		OurPolicy: &contracts.Policy{FeePPM: ppm}, DataQuality: contracts.DataQuality{Level: contracts.DataGood},
		Forwards: contracts.Forwards{WindowHours: 720}}
}

func estimate(level contracts.ConfidenceLevel) *contracts.Estimate {
	e := &contracts.Estimate{ID: "e1", Kind: "outbound_revenue", HorizonHours: 336, Interval: "p10-p90",
		EconomicConfidence: contracts.EconomicConfidence{Level: level, Basis: "n outbound forwards in 720 hours"}}
	if level != contracts.ConfidenceNone {
		e.ExpectedMsat, e.LowMsat, e.HighMsat = mp(2_800_000), mp(1_764_000), mp(3_836_000)
	}
	return e
}

var snap = &contracts.Snapshot{ID: "s1"}

func TestInsideTheBandIsNoProfitableAction(t *testing.T) {
	intent, abst := strategy().Evaluate(snap, peer.New(channel(54, 500)), estimate(contracts.ConfidenceHigh), nil)
	if intent != nil || abst == nil || abst.Reason != contracts.NoProfitableAction || abst.Action != contracts.ActionFeeUpdate {
		t.Fatalf("got intent %v, abstention %+v", intent, abst)
	}
	if err := abst.Validate(); err != nil {
		t.Errorf("abstention does not validate: %v", err)
	}
}

func TestOutsideTheBandWithoutEvidenceAbstains(t *testing.T) {
	_, abst := strategy().Evaluate(snap, peer.New(channel(90, 500)), estimate(contracts.ConfidenceLow), nil)
	if abst == nil || abst.Reason != contracts.InsufficientEvidence {
		t.Fatalf("got %+v", abst)
	}
	if !strings.Contains(abst.Detail, "needs medium") || !strings.Contains(abst.WouldChangeIf[0], "at least 10 outbound forwards") {
		t.Errorf("detail %q, would change if %q", abst.Detail, abst.WouldChangeIf)
	}
	if !abst.RevisitAt.Equal(now.Add(time.Hour)) {
		t.Errorf("revisit at %v", abst.RevisitAt)
	}
}

func TestUntrustedInputsAreInsufficientData(t *testing.T) {
	ch := channel(90, 500)
	ch.DataQuality = contracts.DataQuality{Level: contracts.DataInsufficient, Issues: []string{"forwarding history unavailable: timeout"}}
	_, abst := strategy().Evaluate(snap, peer.New(ch), estimate(contracts.ConfidenceNone), nil)
	if abst == nil || abst.Reason != contracts.InsufficientData || !strings.Contains(abst.Detail, "forwarding history unavailable") {
		t.Fatalf("got %+v", abst)
	}
}

func TestFullChannelProposesALowerFee(t *testing.T) {
	intent, abst := strategy().Evaluate(snap, peer.New(channel(92, 500)), estimate(contracts.ConfidenceMedium), nil)
	if abst != nil {
		t.Fatalf("abstained: %+v", abst)
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("intent does not validate: %v", err)
	}
	f := intent.Parameters.Fee
	if f.FeePPM != 450 || f.ObservedFeePPM != 500 || intent.Mode != contracts.ModeShadow {
		t.Errorf("fee %+v, mode %s", f, intent.Mode)
	}
	if !intent.ExpiresAt.Equal(now.Add(24*time.Hour)) || intent.Rationale.EstimateIDs[0] != "e1" {
		t.Errorf("intent = %+v", intent)
	}
	if !strings.Contains(intent.Rationale.Summary, "from 500 to 450 ppm") || !strings.Contains(intent.Rationale.Summary, "2800 sats expected over 14 days") {
		t.Errorf("summary = %q", intent.Rationale.Summary)
	}
}

func TestDrainedChannelProposesAHigherFee(t *testing.T) {
	intent, _ := strategy().Evaluate(snap, peer.New(channel(10, 500)), estimate(contracts.ConfidenceHigh), nil)
	if intent == nil || intent.Parameters.Fee.FeePPM != 550 {
		t.Fatalf("got %+v", intent)
	}
}

func TestStepIsAtLeastOnePPM(t *testing.T) {
	intent, _ := strategy().Evaluate(snap, peer.New(channel(92, 5)), estimate(contracts.ConfidenceMedium), nil)
	if intent == nil || intent.Parameters.Fee.FeePPM != 4 {
		t.Fatalf("got %+v", intent)
	}
}

func TestNothingBelowZero(t *testing.T) {
	_, abst := strategy().Evaluate(snap, peer.New(channel(92, 0)), estimate(contracts.ConfidenceMedium), nil)
	if abst == nil || abst.Reason != contracts.NoProfitableAction || !strings.Contains(abst.Detail, "already 0 ppm") {
		t.Fatalf("got %+v", abst)
	}
}

func refill(low int64) *contracts.Estimate {
	exp, high := low+2, low+500
	return &contracts.Estimate{ID: "r1", Kind: "replacement_cost", HorizonHours: 336, Interval: "advertised fees",
		LowPPM: &low, ExpectedPPM: &exp, HighPPM: &high, EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceLow}}
}

// A depleted channel priced below the least a refill costs is raised by the
// catch-up step; one priced above it by the usual step.
func TestBelowTheRefillCostCatchesUp(t *testing.T) {
	intent, _ := strategy().Evaluate(snap, peer.New(channel(2, 300)), estimate(contracts.ConfidenceMedium), refill(600))
	if intent == nil || intent.Parameters.Fee.FeePPM != 345 {
		t.Fatalf("got %+v", intent)
	}
	if !strings.Contains(intent.Rationale.Summary, "under the least a refill costs (600 ppm") ||
		!strings.Contains(intent.Rationale.Summary, "Refill: 602 ppm expected (600 to 1100, advertised fees).") {
		t.Errorf("summary = %q", intent.Rationale.Summary)
	}
	if len(intent.Rationale.EstimateIDs) != 2 || intent.Rationale.EstimateIDs[1] != "r1" {
		t.Errorf("estimate ids = %v", intent.Rationale.EstimateIDs)
	}
	if err := intent.Validate(); err != nil {
		t.Errorf("intent does not validate: %v", err)
	}

	intent, _ = strategy().Evaluate(snap, peer.New(channel(2, 800)), estimate(contracts.ConfidenceMedium), refill(600))
	if intent == nil || intent.Parameters.Fee.FeePPM != 880 {
		t.Errorf("above the refill cost: got %+v", intent)
	}
	// A full channel isn't raised, whatever a refill costs.
	intent, _ = strategy().Evaluate(snap, peer.New(channel(92, 400)), estimate(contracts.ConfidenceMedium), refill(600))
	if intent == nil || intent.Parameters.Fee.FeePPM != 360 {
		t.Errorf("full channel: got %+v", intent)
	}
}

// A peer's channels get one decision, on their combined balance, and the
// proposal lists them all.
func TestAPeersChannelsAreDecidedTogether(t *testing.T) {
	a, b := channel(10, 500), channel(10, 500)
	b.ChanID = 2
	b.LocalMsat = 4_000_000_000 // 40% between them: inside the band
	_, abst := strategy().Evaluate(snap, peer.New(a, b), estimate(contracts.ConfidenceHigh), nil)
	if abst == nil || !strings.Contains(abst.Detail, "local balance 45% is inside") || len(abst.ChanIDs) != 2 {
		t.Fatalf("got %+v", abst)
	}
	b.LocalMsat = 500_000_000
	intent, _ := strategy().Evaluate(snap, peer.New(a, b), estimate(contracts.ConfidenceHigh), nil)
	if intent == nil || intent.Parameters.Fee.FeePPM != 550 || len(intent.Target.ChanIDs) != 2 ||
		!strings.Contains(intent.Rationale.Summary, "applies to all 2 of the peer's channels") {
		t.Fatalf("got %+v", intent)
	}
	if err := intent.Validate(); err != nil {
		t.Errorf("intent does not validate: %v", err)
	}
}

// Channels to one peer that charge different fees are set to the highest,
// whatever their balance or evidence.
func TestDifferentFeesAreAligned(t *testing.T) {
	a, b := channel(54, 1000), channel(54, 900)
	b.ChanID = 2
	intent, abst := strategy().Evaluate(snap, peer.New(a, b), estimate(contracts.ConfidenceNone), nil)
	if intent == nil {
		t.Fatalf("abstained: %+v", abst)
	}
	f := intent.Parameters.Fee
	if f.FeePPM != 1000 || f.ObservedFeePPM != 1000 || !strings.HasPrefix(intent.Rationale.Summary, "Align: ") ||
		!strings.Contains(intent.Rationale.Summary, "0x0x2 at 900 ppm") {
		t.Errorf("intent = %+v, %q", f, intent.Rationale.Summary)
	}
	if err := intent.Validate(); err != nil {
		t.Errorf("intent does not validate: %v", err)
	}
}

package fee

import (
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
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
	intent, abst := strategy().Evaluate(snap, channel(54, 500), estimate(contracts.ConfidenceHigh))
	if intent != nil || abst == nil || abst.Reason != contracts.NoProfitableAction {
		t.Fatalf("got intent %v, abstention %+v", intent, abst)
	}
	if err := abst.Validate(); err != nil {
		t.Errorf("abstention does not validate: %v", err)
	}
}

func TestOutsideTheBandWithoutEvidenceAbstains(t *testing.T) {
	_, abst := strategy().Evaluate(snap, channel(90, 500), estimate(contracts.ConfidenceLow))
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
	_, abst := strategy().Evaluate(snap, ch, estimate(contracts.ConfidenceNone))
	if abst == nil || abst.Reason != contracts.InsufficientData || !strings.Contains(abst.Detail, "forwarding history unavailable") {
		t.Fatalf("got %+v", abst)
	}
}

func TestFullChannelProposesALowerFee(t *testing.T) {
	intent, abst := strategy().Evaluate(snap, channel(92, 500), estimate(contracts.ConfidenceMedium))
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
	intent, _ := strategy().Evaluate(snap, channel(10, 500), estimate(contracts.ConfidenceHigh))
	if intent == nil || intent.Parameters.Fee.FeePPM != 550 {
		t.Fatalf("got %+v", intent)
	}
}

func TestStepIsAtLeastOnePPM(t *testing.T) {
	intent, _ := strategy().Evaluate(snap, channel(92, 5), estimate(contracts.ConfidenceMedium))
	if intent == nil || intent.Parameters.Fee.FeePPM != 4 {
		t.Fatalf("got %+v", intent)
	}
}

func TestNothingBelowZero(t *testing.T) {
	_, abst := strategy().Evaluate(snap, channel(92, 0), estimate(contracts.ConfidenceMedium))
	if abst == nil || abst.Reason != contracts.NoProfitableAction || !strings.Contains(abst.Detail, "already 0 ppm") {
		t.Fatalf("got %+v", abst)
	}
}

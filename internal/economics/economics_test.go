package economics

import (
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

var now = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func estimator() *Estimator {
	return &Estimator{Horizon: 14 * 24 * time.Hour, Now: func() time.Time { return now }, NewID: func() string { return "e1" }}
}

func snapshot() *contracts.Snapshot {
	return &contracts.Snapshot{ID: "s1", BlockHeight: 900000}
}

// oldChannel is funded long enough ago that its whole 30-day window counts.
const oldChannel = contracts.ChanID(800000 << 40)

func channel(out int, feeMsat contracts.MilliSat) contracts.ChannelObservation {
	return contracts.ChannelObservation{ChanID: oldChannel, DataQuality: contracts.DataQuality{Level: contracts.DataGood},
		Forwards: contracts.Forwards{WindowHours: 720, OutCount: out, OutFeeMsat: feeMsat}}
}

func TestConfidenceThresholds(t *testing.T) {
	for n, want := range map[int]contracts.ConfidenceLevel{0: "none", 2: "none", 3: "low", 9: "low", 10: "medium", 29: "medium", 30: "high"} {
		if got := ConfidenceFor(n); got != want {
			t.Errorf("ConfidenceFor(%d) = %s, want %s", n, got, want)
		}
	}
	if ForwardsFor(contracts.ConfidenceMedium) != 10 {
		t.Errorf("ForwardsFor(medium) = %d, want 10", ForwardsFor(contracts.ConfidenceMedium))
	}
}

func TestForecastScalesToTheHorizon(t *testing.T) {
	// 12 forwards earned 6,000 sats over 30 days, so 14 days expects 2,800.
	est := estimator().OutboundRevenue(snapshot(), channel(12, 6_000_000))
	if err := est.Validate(); err != nil {
		t.Fatalf("estimate does not validate: %v", err)
	}
	if est.EconomicConfidence.Level != contracts.ConfidenceMedium {
		t.Errorf("confidence = %+v", est.EconomicConfidence)
	}
	if *est.ExpectedMsat != 2_800_000 {
		t.Errorf("expected = %d msat, want 2800000", *est.ExpectedMsat)
	}
	// ±1.2816/sqrt(12) = ±37.0% (370 thousandths).
	if *est.LowMsat != 1_764_000 || *est.HighMsat != 3_836_000 || est.Interval != "p10-p90" {
		t.Errorf("range = %d to %d (%s)", *est.LowMsat, *est.HighMsat, est.Interval)
	}
	if est.Kind != KindOutboundRevenue || est.Estimator.Version != EstimatorVersion || len(est.Evidence) != 1 || len(est.Assumptions) == 0 {
		t.Errorf("estimate = %+v", est)
	}
}

func TestTooFewForwardsMeansNoForecast(t *testing.T) {
	est := estimator().OutboundRevenue(snapshot(), channel(2, 1_000_000))
	if err := est.Validate(); err != nil {
		t.Fatalf("estimate does not validate: %v", err)
	}
	if est.EconomicConfidence.Level != contracts.ConfidenceNone || est.ExpectedMsat != nil {
		t.Errorf("2 forwards gave confidence %s and expected %v", est.EconomicConfidence.Level, est.ExpectedMsat)
	}
}

func TestInsufficientDataMeansNoForecast(t *testing.T) {
	ch := channel(40, 9_000_000)
	ch.DataQuality = contracts.DataQuality{Level: contracts.DataInsufficient, Issues: []string{"our policy unknown"}}
	est := estimator().OutboundRevenue(snapshot(), ch)
	if est.EconomicConfidence.Level != contracts.ConfidenceNone || est.DataQuality.Level != contracts.DataInsufficient {
		t.Errorf("confidence %s, data quality %s", est.EconomicConfidence.Level, est.DataQuality.Level)
	}
}

func TestYoungChannelUsesItsLifeAsTheWindow(t *testing.T) {
	// Funded 1,440 blocks (about 240 hours, 10 days) ago.
	ch := channel(10, 1_000_000)
	ch.ChanID = contracts.ChanID(uint64(900000-1440) << 40)
	est := estimator().OutboundRevenue(snapshot(), ch)
	if est.Evidence[0].WindowHours != 240 {
		t.Errorf("window = %d hours, want 240", est.Evidence[0].WindowHours)
	}
	// 1,000 sats in 240 hours, scaled to 336.
	if *est.ExpectedMsat != 1_400_000 {
		t.Errorf("expected = %d msat, want 1400000", *est.ExpectedMsat)
	}
}

func TestBrandNewChannelHasNoForecast(t *testing.T) {
	ch := channel(50, 1_000_000)
	ch.ChanID = contracts.ChanID(uint64(900000-60) << 40) // about 10 hours old
	est := estimator().OutboundRevenue(snapshot(), ch)
	if est.EconomicConfidence.Level != contracts.ConfidenceNone {
		t.Errorf("a 10-hour-old channel has confidence %s", est.EconomicConfidence.Level)
	}
}

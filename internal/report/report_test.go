package report

import (
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

func mp(m contracts.MilliSat) *contracts.MilliSat { return &m }

// A synthetic node: a busy channel that earned 3,650 sats in 30 days on 10M
// sats of capacity, and a quiet one with no forecast.
func TestBuild(t *testing.T) {
	at := time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)
	window := contracts.Forwards{WindowHours: 720}
	busy, quiet := window, window
	busy.OutCount, busy.OutAmountMsat, busy.OutFeeMsat = 20, 9_000_000_000, 3_650_000
	snap := &contracts.Snapshot{ID: "s1", TakenAt: at, Channels: []contracts.ChannelObservation{
		{ChanID: 1, Active: true, CapacityMsat: 10_000_000_000, LocalMsat: 100_000_000, OurPolicy: &contracts.Policy{FeePPM: 400}, Forwards: busy,
			Failures:    &contracts.Failures{WindowHours: 720, CoverageHours: 240, BalanceFailures: 8, BalanceFailuresMsat: 1_200_000_000},
			Competition: &contracts.Competition{Channels: 364, FeePPMMedian: 500}},
		{ChanID: 2, Active: false, CapacityMsat: 2_000_000_000, LocalMsat: 1_960_000_000, Forwards: quiet,
			Failures: &contracts.Failures{WindowHours: 720, CoverageHours: 240}},
	}}
	estimates := []*contracts.Estimate{
		{ChanID: 1, Kind: economics.KindOutboundRevenue, HorizonHours: 336, ExpectedMsat: mp(1_400_000), LowMsat: mp(500_000), HighMsat: mp(2_900_000),
			Interval: "p10-p90", EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceMedium}},
		{ChanID: 2, Kind: economics.KindOutboundRevenue, HorizonHours: 336},
	}
	status := &store.Status{PerChannel: []store.ChannelStatus{
		{ChanID: 1, Proposal: &contracts.FeeParameters{ObservedFeePPM: 400, FeePPM: 440}, Verdict: contracts.Approved},
		{ChanID: 2, Abstained: contracts.AbstentionReason("insufficient_evidence")},
	}}

	r := Build(snap, estimates, status)
	tt := r.Totals
	if tt.Channels != 2 || tt.Active != 1 || tt.OutFeeMsat != 3_650_000 || tt.BalanceFailures != 8 || tt.CoverageHours != 240 ||
		tt.Forecasts != 1 || tt.ExpectedMsat != 1_400_000 || tt.HorizonHours != 336 {
		t.Errorf("totals = %+v", tt)
	}
	// 3,650 sats on 10M sats over 30 days is 0.0365% a month: 0.444% a year.
	if y := r.Channels[0].YieldPct; y < 0.443 || y > 0.445 {
		t.Errorf("yield = %v", y)
	}
	if r.Channels[1].Forecast != nil || r.Channels[1].FeePPM != nil {
		t.Errorf("quiet channel = %+v", r.Channels[1])
	}

	text := r.Text()
	for _, want := range []string{
		"the 30 days before it (snapshot s1)",
		"2 channels, 1 active: 12,000,000 sats, 17% local",
		"3,650 sats in fees from 20 forwards out of 9,000,000 sats; 0.37% a year on capacity",
		"8 forwards for lack of balance, 1,200,000 sats attempted, recorded for 240 of 720 hours",
		"1,400 sats over the next 14 days, from the 1 of 2 channels with a forecast",
		"proposed 400 to 440 ppm: approved",
		"inactive; abstained: insufficient evidence",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
}

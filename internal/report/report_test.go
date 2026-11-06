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
		{ChanID: contracts.ChanID(uint64(800000)<<40 | 7<<16 | 1), Peer: "02aaaaaaaaaaaaaaaa", PeerAlias: "Big\x1b[31m Peer\u202e", Active: true, CapacityMsat: 10_000_000_000, LocalMsat: 100_000_000, OurPolicy: &contracts.Policy{FeePPM: 400}, Forwards: busy,
			Failures:    &contracts.Failures{WindowHours: 720, CoverageHours: 240, BalanceFailures: 8, BalanceFailuresMsat: 1_200_000_000},
			Competition: &contracts.Competition{Channels: 364, FeePPMMedian: 500}},
		{ChanID: 2, Peer: "03bbbbbbbbbbbbbbbb", Active: false, CapacityMsat: 2_000_000_000, LocalMsat: 1_960_000_000, Forwards: quiet,
			Failures: &contracts.Failures{WindowHours: 720, CoverageHours: 240}},
	}}
	estimates := []*contracts.Estimate{
		{ChanID: contracts.ChanID(uint64(800000)<<40 | 7<<16 | 1), Kind: economics.KindOutboundRevenue, HorizonHours: 336, ExpectedMsat: mp(1_400_000), LowMsat: mp(500_000), HighMsat: mp(2_900_000),
			Interval: "p10-p90", EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceMedium}},
		{ChanID: 2, Kind: economics.KindOutboundRevenue, HorizonHours: 336},
	}
	status := &store.Status{PerChannel: []store.ChannelStatus{
		{ChanID: contracts.ChanID(uint64(800000)<<40 | 7<<16 | 1), Proposal: &contracts.FeeParameters{ObservedFeePPM: 400, FeePPM: 440},
			Verdict: contracts.Rejected, Blocked: []string{"cooldown", "daily_changes"}, Mode: contracts.ModeShadow, PriorApprovals: 2},
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
		"Lightning Jet report for the 30 days to 2026-11-06 17:00 UTC",
		"2 channels, 1 active. 12M sats of capacity, 17% of it on our side.",
		"3,650 sats in fees from 20 forwards (9M sats routed): 0.37% a year on capacity.",
		"8 forwards failed for lack of balance on our side, 1.2M sats in all.",
		"1,400 sats expected, from the one channel with enough history to forecast.",
		"Big[31m Peer   800000x7x1  ░░░░░░░░░░   1%      400     500   3,650       8     1,400  would raise to 440 ppm; already approved twice in shadow in the last 24h",
		"inactive; too few forwards to judge",
		"Big[31m Peer earns 100% of the fees but is nearly empty on our side (1%)",
		"1 channel forwarded nothing out in 30 days while holding 2M sats on our side:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\u202e") {
		t.Error("an alias put a control character on the terminal")
	}
	// The busier channel comes first.
	if strings.Index(text, "800000x7x1") > strings.Index(text, "0x0x2") {
		t.Error("channels are not ordered by earnings")
	}

	raw := strings.Split(r.Raw(), "\n")
	if !strings.HasPrefix(raw[0], "chan_id\tscid\tpeer\tpeer_alias") || len(strings.Split(raw[1], "\t")) != len(rawColumns) {
		t.Errorf("raw = %q", raw[:2])
	}
	if !strings.Contains(raw[1], "\t3650000\t") || !strings.HasSuffix(raw[1], "\tcooldown,daily_changes\tshadow\t2") {
		t.Errorf("raw row = %q", raw[1])
	}
}

// A proposal held by a limit that doesn't count history is held, in shadow
// or not; one held by its history in live mode is held too.
func TestHeld(t *testing.T) {
	for _, tc := range []struct {
		c    Channel
		want string
	}{
		{Channel{Blocked: []string{"cooldown", "fee_ppm_bounds"}, Mode: contracts.ModeShadow, PriorApprovals: 1},
			"already approved once in shadow in the last 24h; held: fee bounds"},
		{Channel{Blocked: []string{"fee_step"}, Mode: contracts.ModeShadow}, "held: step limit"},
		{Channel{Blocked: []string{"cooldown"}, Mode: contracts.ModeLive, PriorApprovals: 1}, "held: cooldown"},
	} {
		if got := held(tc.c); got != tc.want {
			t.Errorf("held(%v) = %q, want %q", tc.c.Blocked, got, tc.want)
		}
	}
}

// Package report summarizes each channel's economics from the latest cycle's
// records: what it earned over the observation window and what that yields on
// its capacity, the forwards that failed for lack of balance, what the
// estimator expects next, what competing channels charge, and what Jet
// decided. It reads Jet's records only, never LND.
package report

import (
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

// Report is the economics of every channel as of one cycle.
type Report struct {
	SnapshotID  string    `json:"snapshot_id"`
	TakenAt     time.Time `json:"taken_at"`
	WindowHours int64     `json:"window_hours"`
	Totals      Totals    `json:"totals"`
	Channels    []Channel `json:"channels"`
}

// Channel is one channel's economics.
type Channel struct {
	ChanID contracts.ChanID `json:"chan_id"`
	// SCID is the channel id as block x transaction x output.
	SCID string `json:"scid"`
	Peer string `json:"peer"`
	// PeerAlias is the alias the peer announced, as recorded: untrusted text.
	PeerAlias    string             `json:"peer_alias,omitempty"`
	Active       bool               `json:"active"`
	CapacityMsat contracts.MilliSat `json:"capacity_msat"`
	LocalMsat    contracts.MilliSat `json:"local_msat"`
	// FeePPM is our outbound fee rate: what we charge to forward out through
	// the channel.
	FeePPM   *int64             `json:"out_fee_ppm,omitempty"`
	Forwards contracts.Forwards `json:"forwards"`
	// YieldPct is the fees earned over the window, as a yearly percentage of
	// the channel's capacity.
	YieldPct    float64                `json:"yield_pct_per_year"`
	Failures    *contracts.Failures    `json:"failures,omitempty"`
	Competition *contracts.Competition `json:"competition,omitempty"`
	Forecast    *Forecast              `json:"forecast,omitempty"`
	// Result is the cycle's decision in jet status's words. Abstained, or
	// Proposal with its Verdict and the rules it failed, say the same in parts.
	Result    string                     `json:"result"`
	Abstained contracts.AbstentionReason `json:"abstained,omitempty"`
	Proposal  *contracts.FeeParameters   `json:"proposal,omitempty"`
	Verdict   contracts.Verdict          `json:"verdict,omitempty"`
	Blocked   []string                   `json:"blocked,omitempty"`
	// Mode is the mode a proposal was decided in, and PriorApprovals the fee
	// changes to the channel approved in that mode in the 24 hours before.
	Mode           contracts.Mode `json:"mode,omitempty"`
	PriorApprovals int            `json:"prior_approvals_24h,omitempty"`
}

// Forecast is the estimator's outbound revenue forecast for a channel.
type Forecast struct {
	HorizonHours int64                     `json:"horizon_hours"`
	ExpectedMsat contracts.MilliSat        `json:"expected_msat"`
	LowMsat      contracts.MilliSat        `json:"low_msat"`
	HighMsat     contracts.MilliSat        `json:"high_msat"`
	Interval     string                    `json:"interval"`
	Confidence   contracts.ConfidenceLevel `json:"confidence"`
}

// Totals add up the channels. Forecasts are summed only over the channels
// that have one.
type Totals struct {
	Channels            int                `json:"channels"`
	Active              int                `json:"active"`
	CapacityMsat        contracts.MilliSat `json:"capacity_msat"`
	LocalMsat           contracts.MilliSat `json:"local_msat"`
	OutCount            int                `json:"out_count"`
	OutAmountMsat       contracts.MilliSat `json:"out_amount_msat"`
	OutFeeMsat          contracts.MilliSat `json:"out_fee_msat"`
	YieldPct            float64            `json:"yield_pct_per_year"`
	BalanceFailures     int                `json:"balance_failures"`
	BalanceFailuresMsat contracts.MilliSat `json:"balance_failures_msat"`
	CoverageHours       int64              `json:"coverage_hours"`
	Forecasts           int                `json:"forecasts"`
	HorizonHours        int64              `json:"horizon_hours"`
	ExpectedMsat        contracts.MilliSat `json:"expected_msat"`
}

// Build assembles a report from one cycle: its snapshot, the estimates made
// from it, and the cycle's per-channel results.
func Build(snap *contracts.Snapshot, estimates []*contracts.Estimate, status *store.Status) *Report {
	results := map[contracts.ChanID]store.ChannelStatus{}
	for _, c := range status.PerChannel {
		results[c.ChanID] = c
	}
	forecasts := map[contracts.ChanID]*Forecast{}
	var horizon int64
	for _, e := range estimates {
		if e.Kind != economics.KindOutboundRevenue {
			continue
		}
		horizon = e.HorizonHours
		if e.ExpectedMsat == nil {
			continue
		}
		forecasts[e.ChanID] = &Forecast{HorizonHours: e.HorizonHours, ExpectedMsat: *e.ExpectedMsat, LowMsat: deref(e.LowMsat),
			HighMsat: deref(e.HighMsat), Interval: e.Interval, Confidence: e.EconomicConfidence.Level}
	}

	r := &Report{SnapshotID: snap.ID, TakenAt: snap.TakenAt}
	t := &r.Totals
	t.HorizonHours = horizon
	for _, c := range snap.Channels {
		res := results[c.ChanID]
		ch := Channel{ChanID: c.ChanID, SCID: c.ChanID.SCID(), Peer: c.Peer, PeerAlias: c.PeerAlias, Active: c.Active,
			CapacityMsat: c.CapacityMsat, LocalMsat: c.LocalMsat, Forwards: c.Forwards,
			YieldPct: yield(c.Forwards.OutFeeMsat, c.CapacityMsat, c.Forwards.WindowHours),
			Failures: c.Failures, Competition: c.Competition, Forecast: forecasts[c.ChanID],
			Result: res.Result(), Abstained: res.Abstained, Proposal: res.Proposal, Verdict: res.Verdict, Blocked: res.Blocked,
			Mode: res.Mode, PriorApprovals: res.PriorApprovals}
		if c.OurPolicy != nil {
			ppm := c.OurPolicy.FeePPM
			ch.FeePPM = &ppm
		}
		r.Channels = append(r.Channels, ch)
		r.WindowHours = max(r.WindowHours, c.Forwards.WindowHours)

		t.Channels++
		if c.Active {
			t.Active++
		}
		t.CapacityMsat += c.CapacityMsat
		t.LocalMsat += c.LocalMsat
		t.OutCount += c.Forwards.OutCount
		t.OutAmountMsat += c.Forwards.OutAmountMsat
		t.OutFeeMsat += c.Forwards.OutFeeMsat
		if f := c.Failures; f != nil {
			t.BalanceFailures += f.BalanceFailures
			t.BalanceFailuresMsat += f.BalanceFailuresMsat
			t.CoverageHours = max(t.CoverageHours, f.CoverageHours)
		}
		if f := ch.Forecast; f != nil {
			t.Forecasts++
			t.ExpectedMsat += f.ExpectedMsat
		}
	}
	t.YieldPct = yield(t.OutFeeMsat, t.CapacityMsat, r.WindowHours)
	return r
}

// yield is fees earned over a window, as a yearly percentage of capacity.
func yield(fees, capacity contracts.MilliSat, windowHours int64) float64 {
	if capacity <= 0 || windowHours <= 0 {
		return 0
	}
	return float64(fees) / float64(capacity) * (365 * 24 / float64(windowHours)) * 100
}

func deref(m *contracts.MilliSat) contracts.MilliSat {
	if m == nil {
		return 0
	}
	return *m
}

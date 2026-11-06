// Package report summarizes each channel's economics from the latest cycle's
// records: what it earned over the observation window and what that yields on
// its capacity, the forwards that failed for lack of balance, what the
// estimator expects next, what competing channels charge, and what Jet
// decided. It reads Jet's records only, never LND.
package report

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
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
	ChanID       contracts.ChanID   `json:"chan_id"`
	Peer         string             `json:"peer"`
	Active       bool               `json:"active"`
	CapacityMsat contracts.MilliSat `json:"capacity_msat"`
	LocalMsat    contracts.MilliSat `json:"local_msat"`
	FeePPM       *int64             `json:"fee_ppm,omitempty"`
	Forwards     contracts.Forwards `json:"forwards"`
	// YieldPct is the fees earned over the window, as a yearly percentage of
	// the channel's capacity.
	YieldPct    float64                `json:"yield_pct_per_year"`
	Failures    *contracts.Failures    `json:"failures,omitempty"`
	Competition *contracts.Competition `json:"competition,omitempty"`
	Forecast    *Forecast              `json:"forecast,omitempty"`
	Result      string                 `json:"result"`
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
	results := map[contracts.ChanID]string{}
	for _, c := range status.PerChannel {
		results[c.ChanID] = c.Result()
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
		ch := Channel{ChanID: c.ChanID, Peer: c.Peer, Active: c.Active, CapacityMsat: c.CapacityMsat, LocalMsat: c.LocalMsat,
			Forwards: c.Forwards, YieldPct: yield(c.Forwards.OutFeeMsat, c.CapacityMsat, c.Forwards.WindowHours),
			Failures: c.Failures, Competition: c.Competition, Forecast: forecasts[c.ChanID], Result: results[c.ChanID]}
		if c.OurPolicy != nil {
			ppm := c.OurPolicy.FeePPM
			ch.FeePPM = &ppm
		}
		if ch.Result == "" {
			ch.Result = "no decision recorded"
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

// Text renders the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	t := r.Totals
	days := r.WindowHours / 24
	fmt.Fprintf(&b, "Report       %s, the %d days before it (snapshot %s)\n", r.TakenAt.Format("2006-01-02 15:04 MST"), days, r.SnapshotID)
	fmt.Fprintf(&b, "Node         %d channels, %d active: %s sats, %d%% local\n", t.Channels, t.Active, commas(t.CapacityMsat.Sats()), pct(t.LocalMsat, t.CapacityMsat))
	fmt.Fprintf(&b, "Earned       %s sats in fees from %d forwards out of %s sats; %s a year on capacity\n",
		commas(t.OutFeeMsat.Sats()), t.OutCount, commas(t.OutAmountMsat.Sats()), yieldText(t.YieldPct))
	if t.CoverageHours > 0 || t.BalanceFailures > 0 {
		fmt.Fprintf(&b, "Failed       %d forwards for lack of balance, %s sats attempted, recorded for %d of %d hours (attempts, not payments)\n",
			t.BalanceFailures, commas(t.BalanceFailuresMsat.Sats()), t.CoverageHours, r.WindowHours)
	}
	if t.Forecasts == 0 {
		fmt.Fprintf(&b, "Expected     no channel has enough forwards for a forecast\n")
	} else {
		fmt.Fprintf(&b, "Expected     %s sats over the next %d days, from the %d of %d channels with a forecast\n",
			commas(t.ExpectedMsat.Sats()), t.HorizonHours/24, t.Forecasts, t.Channels)
	}

	if len(r.Channels) == 0 {
		return b.String()
	}
	b.WriteString("\n")
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "Channel\tLocal\tFee ppm\tOut\tEarned\tYield/yr\tFailed\tExpected\tCompeting\t\t")
	for _, c := range r.Channels {
		fee, failed, expected, competing := "-", "-", "-", "-"
		if c.FeePPM != nil {
			fee = strconv.FormatInt(*c.FeePPM, 10)
		}
		if c.Failures != nil {
			failed = strconv.Itoa(c.Failures.BalanceFailures)
		}
		if c.Forecast != nil {
			expected = commas(c.Forecast.ExpectedMsat.Sats())
		}
		if c.Competition != nil && c.Competition.Channels > 0 {
			competing = strconv.FormatInt(c.Competition.FeePPMMedian, 10)
		}
		result := c.Result
		if !c.Active {
			result = "inactive; " + result
		}
		fmt.Fprintf(w, "%s\t%d%%\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t\t%s\n", c.ChanID, pct(c.LocalMsat, c.CapacityMsat), fee,
			c.Forwards.OutCount, commas(c.Forwards.OutFeeMsat.Sats()), yieldText(c.YieldPct), failed, expected, competing, result)
	}
	w.Flush()
	fmt.Fprintf(&b, "\nEarned, Out and Failed cover the %d days before the cycle. Expected is the next %d days, in sats.\n",
		days, t.HorizonHours/24)
	b.WriteString("Competing is the median fee of the other channels into the same peer: a prior, not a price.\n")
	return b.String()
}

func yieldText(pctPerYear float64) string {
	return strconv.FormatFloat(pctPerYear, 'f', 2, 64) + "%"
}

func pct(part, whole contracts.MilliSat) int64 {
	if whole <= 0 {
		return 0
	}
	return int64(part) * 100 / int64(whole)
}

// commas formats n with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

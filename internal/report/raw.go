package report

import (
	"strconv"
	"strings"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// rawColumns are Raw's columns. Amounts are in msat, as Jet records them.
var rawColumns = []string{
	"chan_id", "scid", "peer", "peer_alias", "active", "capacity_msat", "local_msat", "remote_msat", "out_fee_ppm",
	"out_forwards", "out_msat", "out_fee_msat", "in_forwards", "in_msat", "window_hours", "yield_pct_per_year",
	"failed_forwards", "failed_msat", "coverage_hours",
	"expected_msat", "low_msat", "high_msat", "interval", "horizon_hours", "confidence",
	"competing_channels", "competing_p25_ppm", "competing_median_ppm", "competing_p75_ppm",
	"refill_expected_ppm", "refill_low_ppm", "refill_high_ppm",
	"result", "blocked", "mode", "prior_approvals_24h",
}

// Raw renders the report as tab-separated values, one row per channel with
// every exact value, for spreadsheets and scripts. An empty cell is a value
// that wasn't observed or estimated.
func (r *Report) Raw() string {
	var b strings.Builder
	b.WriteString(strings.Join(rawColumns, "\t") + "\n")
	for _, c := range r.Channels {
		f, row := c.Forwards, make([]string, 0, len(rawColumns))
		add := func(v ...string) { row = append(row, v...) }
		add(c.ChanID.String(), c.SCID, c.Peer, clean(c.PeerAlias), strconv.FormatBool(c.Active),
			msat(c.CapacityMsat), msat(c.LocalMsat), msat(c.RemoteMsat), optInt(c.FeePPM),
			strconv.Itoa(f.OutCount), msat(f.OutAmountMsat), msat(f.OutFeeMsat), strconv.Itoa(f.InCount), msat(f.InAmountMsat),
			strconv.FormatInt(f.WindowHours, 10), strconv.FormatFloat(c.YieldPct, 'f', 4, 64))
		if fl := c.Failures; fl != nil {
			add(strconv.Itoa(fl.BalanceFailures), msat(fl.BalanceFailuresMsat), strconv.FormatInt(fl.CoverageHours, 10))
		} else {
			add("", "", "")
		}
		if fc := c.Forecast; fc != nil {
			add(msat(fc.ExpectedMsat), msat(fc.LowMsat), msat(fc.HighMsat), fc.Interval, strconv.FormatInt(fc.HorizonHours, 10), string(fc.Confidence))
		} else {
			add("", "", "", "", "", "")
		}
		if cp := c.Competition; cp != nil {
			add(strconv.Itoa(cp.Channels), strconv.FormatInt(cp.FeePPMP25, 10), strconv.FormatInt(cp.FeePPMMedian, 10), strconv.FormatInt(cp.FeePPMP75, 10))
		} else {
			add("", "", "", "")
		}
		if rf := c.Refill; rf != nil {
			add(strconv.FormatInt(rf.ExpectedPPM, 10), strconv.FormatInt(rf.LowPPM, 10), strconv.FormatInt(rf.HighPPM, 10))
		} else {
			add("", "", "")
		}
		add(c.Result, strings.Join(c.Blocked, ","))
		if c.Proposal != nil {
			add(string(c.Mode), strconv.Itoa(c.PriorApprovals))
		} else {
			add("", "")
		}
		b.WriteString(strings.Join(row, "\t") + "\n")
	}
	return b.String()
}

func msat(m contracts.MilliSat) string { return strconv.FormatInt(int64(m), 10) }

func optInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

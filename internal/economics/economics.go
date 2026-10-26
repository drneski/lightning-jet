// Package economics estimates what a channel's liquidity earns. Every estimate
// carries its economic confidence and the data quality of its inputs, kept
// apart.
package economics

import (
	"fmt"
	"math"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// The estimator's identity, recorded on every estimate it makes.
const (
	EstimatorName    = "flow"
	EstimatorVersion = "0.1.0"
)

// KindOutboundRevenue is the routing revenue a channel's outbound liquidity is
// expected to earn over the horizon if nothing changes: the baseline that a
// change is measured against.
const KindOutboundRevenue = "outbound_revenue"

// Forwards needed for each confidence level. Revenue is forecast from a count
// of forwards; with n forwards the relative error is about 1/sqrt(n), so 30
// gives a range of about ±23% (high), 10 about ±41% (medium), 3 about ±74%
// (low). Fewer than 3 gives no defensible forecast.
var thresholds = []struct {
	level contracts.ConfidenceLevel
	min   int
}{
	{contracts.ConfidenceHigh, 30},
	{contracts.ConfidenceMedium, 10},
	{contracts.ConfidenceLow, 3},
}

// ConfidenceFor returns the confidence of a forecast from n forwards.
func ConfidenceFor(n int) contracts.ConfidenceLevel {
	for _, t := range thresholds {
		if n >= t.min {
			return t.level
		}
	}
	return contracts.ConfidenceNone
}

// ForwardsFor returns the forwards a forecast needs to reach a confidence
// level.
func ForwardsFor(level contracts.ConfidenceLevel) int {
	for _, t := range thresholds {
		if t.level == level {
			return t.min
		}
	}
	return 0
}

// Estimator makes estimates.
type Estimator struct {
	Horizon time.Duration
	Now     func() time.Time
	NewID   func() string
}

// minAgeHours is the youngest a channel can be and still have a forecast.
const minAgeHours = 24

// OutboundRevenue estimates a channel's routing revenue over the horizon at
// its current policy, from the forwards out of it in the snapshot's window.
func (e *Estimator) OutboundRevenue(snap *contracts.Snapshot, ch contracts.ChannelObservation) *contracts.Estimate {
	horizonHours := int64(e.Horizon / time.Hour)
	window := ch.Forwards.WindowHours
	ageHours, ageKnown := channelAgeHours(snap.BlockHeight, ch.ChanID)
	if ageKnown && ageHours < window {
		window = ageHours
	}
	n := ch.Forwards.OutCount

	est := &contracts.Estimate{
		ID: e.NewID(), Schema: contracts.SchemaEstimate, ChanID: ch.ChanID, Kind: KindOutboundRevenue,
		SnapshotID: snap.ID, Estimator: contracts.Component{Name: EstimatorName, Version: EstimatorVersion},
		HorizonHours: horizonHours, DataQuality: ch.DataQuality, ComputedAt: contracts.Timestamp(e.Now()),
		Evidence: []contracts.Evidence{{
			Kind: "forwards_out", WindowHours: window, Count: n, Source: "lnd.forwardinghistory",
			Summary: fmt.Sprintf("%d forwards out, %d sats earned", n, ch.Forwards.OutFeeMsat.Sats()),
		}},
		Assumptions: []string{
			fmt.Sprintf("the next %d days resemble the last %d", horizonHours/24, max(window/24, 1)),
			"our fee policy stays as it is",
			"revenue per forward stays near its observed average",
		},
	}

	basis := fmt.Sprintf("%d outbound forwards in %d hours", n, window)
	if ageKnown && ageHours < ch.Forwards.WindowHours {
		basis += ", the channel's whole life so far"
	}
	level := ConfidenceFor(n)
	switch {
	case ch.DataQuality.Level == contracts.DataInsufficient:
		level, basis = contracts.ConfidenceNone, "the inputs are insufficient for a forecast"
	case ageKnown && ageHours < minAgeHours:
		level, basis = contracts.ConfidenceNone, fmt.Sprintf("the channel is about %d hours old", ageHours)
	case window <= 0:
		level, basis = contracts.ConfidenceNone, "no window of history to forecast from"
	}
	est.EconomicConfidence = contracts.EconomicConfidence{Level: level, Basis: basis}
	if level == contracts.ConfidenceNone {
		return est
	}

	expected := contracts.MilliSat(int64(ch.Forwards.OutFeeMsat) * horizonHours / window)
	// The p10-p90 range of a count-based forecast: ±1.2816/sqrt(n), in
	// thousandths. Floating point touches only the count, never an amount.
	k := int64(math.Round(1281.55 / math.Sqrt(float64(n))))
	low := contracts.MilliSat(int64(expected) * max(0, 1000-k) / 1000)
	high := contracts.MilliSat(int64(expected) * (1000 + k) / 1000)
	est.ExpectedMsat, est.LowMsat, est.HighMsat, est.Interval = &expected, &low, &high, "p10-p90"
	return est
}

// channelAgeHours estimates a channel's age from the blocks since its funding
// transaction confirmed, at ten minutes a block.
func channelAgeHours(tip uint32, id contracts.ChanID) (int64, bool) {
	funded := id.FundingHeight()
	if funded == 0 || funded > tip {
		return 0, false
	}
	return int64(tip-funded) / 6, true
}

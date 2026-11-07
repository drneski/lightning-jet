// Package economics estimates what a channel's liquidity earns. Every estimate
// carries its economic confidence and the data quality of its inputs, kept
// apart.
package economics

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// The estimator's identity, recorded on every estimate it makes.
const (
	EstimatorName    = "flow"
	EstimatorVersion = "0.4.0"
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

	if f := ch.Failures; f != nil {
		est.Evidence = append(est.Evidence, contracts.Evidence{
			Kind: "failed_for_balance", WindowHours: f.WindowHours, Count: f.BalanceFailures, Source: "jet.htlc_stream",
			Summary: fmt.Sprintf("%d forwards failed for lack of balance, %d sats attempted, recorded for %d of %d hours; attempts, not payments",
				f.BalanceFailures, f.BalanceFailuresMsat.Sats(), f.CoverageHours, f.WindowHours),
		})
	}
	if c := ch.Competition; c != nil && c.Channels > 0 {
		est.Evidence = append(est.Evidence, contracts.Evidence{
			Kind: "competing_fees", Count: c.Channels, Source: "lnd.graph.node", Prior: true,
			Summary: fmt.Sprintf("%d other channels into the peer advertise %d-%d ppm (median %d)",
				c.Channels, c.FeePPMP25, c.FeePPMP75, c.FeePPMMedian),
		})
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

// KindReplacementCost is what putting liquidity back on our side of a channel
// costs: a circular refill's fees, as a rate in ppm of the amount moved.
const KindReplacementCost = "replacement_cost"

// Intervals of a replacement cost estimate: what its range rests on.
const (
	IntervalMeasured   = "measured refills"
	IntervalAdvertised = "advertised fees"
	IntervalAttempts   = "advertised fees and refill attempts"
)

// ReplacementCost estimates what refilling the channel costs, from the refills
// into it in LND's payment history when any succeeded: their median, and
// their range. Without one, it is a prior from advertised fees. Every refill
// ends with the hop from the peer to us, paid at the peer's fee, so that fee
// is the low: no refill costs less. The expected cost adds the cheapest way
// into the peer: through another of our peers when one has a channel into
// it, the two-hop refill, and otherwise the cheapest quarter of the channels
// into it. The high adds the median of the channels into it instead, for
// when the cheap ways in lack the liquidity.
//
// Refill attempts that failed correct the prior. The cheapest route that
// carried an attempt all the way back to us, as a probe does, is a price
// that had the liquidity, and becomes the expected cost. Without one, the
// costliest route that ran out of liquidity raises the expected cost to at
// least its price: the cheaper ways in were tried and came up dry.
func (e *Estimator) ReplacementCost(snap *contracts.Snapshot, ch contracts.ChannelObservation) *contracts.Estimate {
	est := &contracts.Estimate{
		ID: e.NewID(), Schema: contracts.SchemaEstimate, ChanID: ch.ChanID, Kind: KindReplacementCost,
		SnapshotID: snap.ID, Estimator: contracts.Component{Name: EstimatorName, Version: EstimatorVersion},
		HorizonHours: int64(e.Horizon / time.Hour), DataQuality: ch.DataQuality, ComputedAt: contracts.Timestamp(e.Now()),
		Evidence: []contracts.Evidence{},
		Assumptions: []string{
			"fees stay as advertised",
			"a route into the peer has the liquidity to carry the refill",
			"base fees are left out: on a refill of 100,000 sats, a 1 sat base fee adds 10 ppm",
		},
	}
	none := func(basis string) *contracts.Estimate {
		est.EconomicConfidence = contracts.EconomicConfidence{Level: contracts.ConfidenceNone, Basis: basis}
		return est
	}
	set := func(low, expected, high int64, interval string, level contracts.ConfidenceLevel, basis string) *contracts.Estimate {
		est.LowPPM, est.ExpectedPPM, est.HighPPM, est.Interval = &low, &expected, &high, interval
		est.EconomicConfidence = contracts.EconomicConfidence{Level: level, Basis: basis}
		return est
	}
	p := ch.PeerPolicy
	switch {
	case ch.DataQuality.Level == contracts.DataInsufficient:
		return none("the inputs are insufficient for an estimate")
	case p == nil:
		return none("the peer's fee to us is unknown")
	case p.Disabled:
		return none("the peer has disabled forwarding to us, so no refill can come back through it")
	}
	est.Evidence = append(est.Evidence, contracts.Evidence{
		Kind: "peer_fee", Count: 1, Source: "lnd.getchaninfo",
		Summary: fmt.Sprintf("the peer charges %d ppm and %d msat base to forward to us, the last hop of any refill", p.FeePPM, p.BaseMsat),
	})

	var succeeded, reached, dry []int64
	var window int64
	if r := ch.Refills; r != nil {
		window = r.WindowHours
		for _, a := range r.Attempts {
			switch a.Result {
			case contracts.AttemptSucceeded:
				succeeded = append(succeeded, a.PPM())
			case contracts.AttemptReached:
				reached = append(reached, a.PPM())
			case contracts.AttemptNoLiquidity:
				dry = append(dry, a.PPM())
			}
		}
	}
	attempts := func(kind string, ppms []int64, one, many string) {
		at := fmt.Sprintf("at %d to %d ppm", slices.Min(ppms), slices.Max(ppms))
		if slices.Min(ppms) == slices.Max(ppms) {
			at = fmt.Sprintf("at %d ppm", ppms[0])
		}
		est.Evidence = append(est.Evidence, contracts.Evidence{
			Kind: kind, WindowHours: window, Count: len(ppms), Source: "lnd.listpayments",
			Summary: fmt.Sprintf("%d %s, %s", len(ppms), plural(len(ppms), one, many), at),
		})
	}

	if n := len(succeeded); n > 0 {
		slices.Sort(succeeded)
		attempts("refills_succeeded", succeeded, "refill into the channel succeeded", "refills into the channel succeeded")
		est.Assumptions = []string{"a refill costs about what the recent ones into the channel did", "a refill of another size pays about the same rate"}
		median := succeeded[(n-1)/2]
		return set(succeeded[0], median, succeeded[n-1], IntervalMeasured, refillConfidence(n),
			fmt.Sprintf("%d %s into the channel in %d days, median %d ppm", n, plural(n, "refill", "refills"), max(window/24, 1), median))
	}

	low := p.FeePPM
	var expected, high int64
	var wayBasis string
	c := ch.Competition
	prior := c != nil && c.Channels > 0
	if prior {
		est.Evidence = append(est.Evidence, contracts.Evidence{
			Kind: "competing_fees", Count: c.Channels, Source: "lnd.graph.node", Prior: true,
			Summary: fmt.Sprintf("%d other channels into the peer advertise %d-%d ppm (median %d)",
				c.Channels, c.FeePPMP25, c.FeePPMP75, c.FeePPMMedian),
		})
		var way int64
		way, wayBasis = c.FeePPMP25, "the cheapest quarter of the channels into the peer"
		if n := c.Neighbor; n != nil {
			way, wayBasis = n.FeePPM, "the cheapest channel into the peer run by another of our peers, a two-hop refill"
			via := "another of our peers"
			for _, o := range snap.Channels {
				if o.Peer == n.Pubkey {
					via = "the peer of our channel " + o.ChanID.SCID()
					break
				}
			}
			est.Evidence = append(est.Evidence, contracts.Evidence{
				Kind: "neighbor_fee", Count: 1, Source: "lnd.graph.node", Prior: true,
				Summary: fmt.Sprintf("%s charges %d ppm to forward into the peer", via, n.FeePPM),
			})
		}
		expected, high = low+way, low+max(way, c.FeePPMMedian)
	}
	if len(reached) > 0 {
		attempts("refill_routes_reached", reached, "attempted refill reached us", "attempted refills reached us")
	}
	if len(dry) > 0 {
		attempts("refill_attempts_dry", dry, "attempted refill ran out of liquidity past our channel",
			"attempted refills ran out of liquidity past our channel")
	}

	switch {
	case len(reached) > 0:
		cheapest := slices.Min(reached)
		expected = max(low, cheapest)
		est.Assumptions = []string{"the cheapest route that reached us still has the liquidity", "a refill of another size pays about the same rate"}
		return set(low, expected, max(high, expected), IntervalAttempts, contracts.ConfidenceLow,
			fmt.Sprintf("no refill into this channel succeeded, but %d attempted %s reached us, the cheapest at %d ppm; "+
				"the peer's fee to us is the least one can cost", len(reached), plural(len(reached), "route", "routes"), cheapest))
	case !prior && c == nil:
		return none(fmt.Sprintf("the channels into the peer are unknown; a refill costs at least %d ppm, the peer's fee to us", p.FeePPM))
	case !prior:
		return none("no other channel leads into the peer, so a circular refill has no way in")
	case len(dry) > 0:
		costliest := slices.Max(dry)
		expected = max(expected, costliest)
		return set(low, expected, max(high, expected), IntervalAttempts, contracts.ConfidenceLow,
			fmt.Sprintf("advertised fees, the peer's fee to us plus %s, raised by %d refill %s that ran out of liquidity, "+
				"the costliest at %d ppm", wayBasis, len(dry), plural(len(dry), "attempt", "attempts"), costliest))
	}
	return set(low, expected, high, IntervalAdvertised, contracts.ConfidenceLow,
		"advertised fees only, no refill into this channel measured: the peer's fee to us plus "+wayBasis)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// refillConfidence is how far a median of n measured refills can be
// trusted: a few refills say little about the next one.
func refillConfidence(n int) contracts.ConfidenceLevel {
	switch {
	case n >= 10:
		return contracts.ConfidenceHigh
	case n >= 3:
		return contracts.ConfidenceMedium
	}
	return contracts.ConfidenceLow
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

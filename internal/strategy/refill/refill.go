// Package refill is Jet's first refill strategy, refill.value. For a depleted
// channel with enough traffic to value, it sizes a refill to the demand it
// expects, picks a source with liquidity to spare and little demand of its
// own, and proposes the refill only when what it would earn at the current
// fee, at the low end of the forecast's range, is more than it would cost.
// Otherwise it abstains, and says at what fee or what cost a refill would
// pay. It proposes and never decides: authorization does that.
//
// It works per peer, like the fee strategy: a peer's channels are refilled as
// one, since a refill comes back through whichever of them the peer forwards
// it over, and are never the source of a refill of each other.
package refill

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
)

// The strategy's identity, recorded on everything it proposes.
const (
	StrategyName    = "refill.value"
	StrategyVersion = "0.4.0"
)

// Config is the strategy's settings.
type Config struct {
	// LowPct is the share of capacity on our side below which a channel is a
	// refill target.
	LowPct int64
	// TargetPct is the share a refill brings our side up to at most, and the
	// share a source keeps at least.
	TargetPct int64
	// MinConfidence is the economic confidence the target's outbound revenue
	// needs before a refill can be valued.
	MinConfidence contracts.ConfidenceLevel
	// MinAmountMsat is the smallest refill worth proposing.
	MinAmountMsat contracts.MilliSat
	// IntentTTL is how long a proposal stays valid; Revisit is when an
	// abstention is due to be evaluated again.
	IntentTTL, Revisit time.Duration
}

// DefaultConfig is the configuration Jet runs with.
var DefaultConfig = Config{
	LowPct: 20, TargetPct: 50, MinConfidence: contracts.ConfidenceMedium,
	MinAmountMsat: 100_000_000, IntentTTL: time.Hour, Revisit: time.Hour,
}

// Strategy evaluates refills.
type Strategy struct {
	Config
	Mode  contracts.Mode
	Now   func() time.Time
	NewID func() string
}

// Channel is what the strategy knows of one peer's channels in a cycle: what
// was observed, as one, and their outbound revenue and replacement cost
// estimates. Channels are the peer's channels when it has several.
type Channel struct {
	Obs      contracts.ChannelObservation
	Channels []contracts.ChannelObservation
	Revenue  *contracts.Estimate
	Refill   *contracts.Estimate
}

// ids are the peer's channels for a record's chan_ids: nil for one.
func (c *Channel) ids() []contracts.ChanID {
	if len(c.Channels) < 2 {
		return nil
	}
	ids := make([]contracts.ChanID, len(c.Channels))
	for i, o := range c.Channels {
		ids[i] = o.ChanID
	}
	return ids
}

// fullest is the channel with the most on our side, which a refill leaves
// through: a payment goes out through one channel.
func (c *Channel) fullest() contracts.ChannelObservation {
	best := c.Obs
	for i, o := range c.Channels {
		if i == 0 || o.LocalMsat > best.LocalMsat {
			best = o
		}
	}
	return best
}

var component = contracts.Component{Name: StrategyName, Version: StrategyVersion}

// Target reports whether a refill of the channel is evaluated at all: it is
// below LowPct on our side.
func (s *Strategy) Target(ch contracts.ChannelObservation) bool {
	return ch.CapacityMsat > 0 && int64(ch.LocalMsat)*100/int64(ch.CapacityMsat) < s.LowPct
}

// Evaluate returns exactly one of an intent or an abstention for refilling
// target, choosing a source among all, the cycle's channels.
func (s *Strategy) Evaluate(snap *contracts.Snapshot, target Channel, all []Channel) (*contracts.Intent, *contracts.Abstention) {
	now := contracts.Timestamp(s.Now())
	obs, rev, rc := target.Obs, target.Revenue, target.Refill
	ids := []string{rev.ID}
	if rc != nil {
		ids = append(ids, rc.ID)
	}
	abstain := func(reason contracts.AbstentionReason, detail string, wouldChangeIf ...string) (*contracts.Intent, *contracts.Abstention) {
		return nil, &contracts.Abstention{
			ID: s.NewID(), Schema: contracts.SchemaAbstention, ChanID: obs.ChanID, ChanIDs: target.ids(), Strategy: component, Action: contracts.ActionRebalance,
			SnapshotID: snap.ID, EstimateIDs: ids, Reason: reason, Detail: detail,
			WouldChangeIf: wouldChangeIf, RevisitAt: now.Add(s.Revisit), CreatedAt: now,
		}
	}
	days := max(rev.HorizonHours/24, 1)

	if obs.DataQuality.Level == contracts.DataInsufficient || obs.OurPolicy == nil || obs.CapacityMsat <= 0 {
		issues := strings.Join(obs.DataQuality.Issues, "; ")
		if issues == "" {
			issues = "capacity or our policy unknown"
		}
		return abstain(contracts.InsufficientData, "the inputs can't be trusted: "+issues, "the missing or failed inputs become available")
	}
	if c := rev.EconomicConfidence; !c.Level.AtLeast(s.MinConfidence) || rev.LowMsat == nil || rev.ExpectedMsat == nil {
		return abstain(contracts.InsufficientEvidence,
			fmt.Sprintf("%s has economic confidence %s (%s); valuing a refill needs %s", rev.Kind, c.Level, c.Basis, s.MinConfidence),
			fmt.Sprintf("at least %d outbound forwards through this channel within %d days",
				economics.ForwardsFor(s.MinConfidence), max(obs.Forwards.WindowHours/24, 1)))
	}
	if rc == nil || rc.ExpectedPPM == nil || rc.HighPPM == nil {
		basis := "no replacement cost estimate"
		if rc != nil {
			basis = rc.EconomicConfidence.Basis
		}
		return abstain(contracts.InsufficientEvidence, "there is no way to price a refill: "+basis,
			"what a refill costs can be estimated")
	}

	// Size: up to TargetPct on our side, and no more than the demand the
	// channel's own forwards show over the horizon.
	window := obs.Forwards.WindowHours
	for _, ev := range rev.Evidence {
		if ev.Kind == "forwards_out" && ev.WindowHours > 0 {
			window = ev.WindowHours
		}
	}
	var demand contracts.MilliSat
	if window > 0 {
		demand = contracts.MilliSat(int64(obs.Forwards.OutAmountMsat) * rev.HorizonHours / window)
	}
	gap := contracts.MilliSat(int64(obs.CapacityMsat)*s.TargetPct/100) - obs.LocalMsat
	amount := min(gap, demand)
	if amount < s.MinAmountMsat {
		return abstain(contracts.NoProfitableAction,
			fmt.Sprintf("a refill would move %s sats: the channel's forwards out show %s sats of demand over %d days, and it is %s sats short of %d%% on our side; refills under %s sats aren't proposed",
				sats(max(amount, 0)), sats(demand), days, sats(max(gap, 0)), s.TargetPct, sats(s.MinAmountMsat)),
			fmt.Sprintf("demand over %d days grows to at least %s sats", days, sats(s.MinAmountMsat)))
	}

	// Source: the neighbor, the cheapest way into the peer, when it has
	// liquidity to spare; otherwise the peer with the most to spare. Either
	// way it keeps TargetPct on our side, its fullest channel can carry the
	// whole amount, and it doesn't route enough to be valued itself. A
	// channel to the target's own peer would only move sats between the two.
	var neighbor string
	if c := obs.Competition; c != nil && c.Neighbor != nil {
		neighbor = c.Neighbor.Pubkey
	}
	var source *Channel
	var spare contracts.MilliSat
	viaNeighbor := false
	for i := range all {
		c := &all[i]
		o := c.Obs
		if o.Peer == obs.Peer || !o.Active || o.DataQuality.Level == contracts.DataInsufficient || c.fullest().LocalMsat < amount {
			continue
		}
		if c.Revenue != nil && c.Revenue.EconomicConfidence.Level.AtLeast(s.MinConfidence) {
			continue // it has demand of its own
		}
		left := o.LocalMsat - amount - contracts.MilliSat(int64(o.CapacityMsat)*s.TargetPct/100)
		if left < 0 {
			continue
		}
		isNeighbor := neighbor != "" && o.Peer == neighbor
		if source == nil || isNeighbor && !viaNeighbor || isNeighbor == viaNeighbor && left > spare {
			source, spare, viaNeighbor = c, left, isNeighbor
		}
	}
	if source == nil {
		return abstain(contracts.NoProfitableAction,
			fmt.Sprintf("no channel has %s sats to spare while keeping %d%% on our side, with little demand of its own", sats(amount), s.TargetPct),
			"a channel with liquidity to spare and little demand of its own")
	}
	if source.Revenue != nil {
		ids = append(ids, source.Revenue.ID)
	}
	// Priced by what refills into it have cost when there are any; otherwise
	// by the expected cost through the neighbor, and the high end from
	// anywhere else.
	costPPM, how := *rc.HighPPM, "the high end of its replacement cost, with no cheap way in from that source"
	switch {
	case rc.Interval == economics.IntervalMeasured:
		costPPM, how = *rc.ExpectedPPM, "the median of the refills into it"
	case viaNeighbor && rc.Interval == economics.IntervalAttempts:
		costPPM, how = *rc.ExpectedPPM, "its expected replacement cost, from refill attempts"
	case viaNeighbor:
		costPPM, how = *rc.ExpectedPPM, "its expected replacement cost, through the cheapest way in"
	}

	// Value: the amount routed out at our current fee. The forecast's fees are
	// whatever the window's fees were, which a fee change since leaves behind;
	// its range, from the count of forwards, says how sure the volume is, so
	// the low end keeps the same share of the expected value.
	cost := contracts.MilliSat(int64(amount) * costPPM / 1_000_000)
	fee := obs.OurPolicy.FeePPM
	expected := contracts.MilliSat(int64(amount) * fee / 1_000_000)
	var low contracts.MilliSat
	if *rev.ExpectedMsat > 0 {
		low = contracts.MilliSat(int64(expected) * int64(*rev.LowMsat) / int64(*rev.ExpectedMsat))
	}
	what := fmt.Sprintf("a refill of %s sats from %s would cost about %s sats (%d ppm, %s) and earn %s to %s sats over %d days at %d ppm, "+
		"if forwards out keep their volume at that fee",
		sats(amount), source.fullest().ChanID.SCID(), sats(cost), costPPM, how, sats(low), sats(expected), days, fee)
	if low <= cost {
		wouldChange := []string{}
		if low > 0 {
			wouldChange = append(wouldChange,
				fmt.Sprintf("our fee rises above about %d ppm", fee*int64(cost)/int64(low)+1),
				fmt.Sprintf("a refill costs under about %d ppm", costPPM*int64(low)/int64(cost)))
		} else {
			wouldChange = append(wouldChange, "the forecast's low end rises above zero")
		}
		return abstain(contracts.NoProfitableAction, what+": at the forecast's low end, less than it costs", wouldChange...)
	}

	src := source.fullest().ChanID
	maxFee := low
	return &contracts.Intent{
		ID: s.NewID(), Schema: contracts.SchemaIntent, Action: contracts.ActionRebalance,
		Target:     contracts.Target{ChanID: obs.ChanID, ChanIDs: target.ids(), SourceChanID: &src},
		Parameters: contracts.IntentParameters{Rebalance: &contracts.RebalanceParameters{AmountMsat: amount}},
		Limits:     contracts.IntentLimits{MaxFeeMsat: &maxFee},
		Rationale: contracts.Rationale{EstimateIDs: ids, Summary: fmt.Sprintf(
			"Refill: %s, so it may pay at most %s sats, what it earns at the forecast's low end.", what, sats(maxFee))},
		Strategy:   component,
		SnapshotID: snap.ID, Mode: s.Mode, CreatedAt: now, ExpiresAt: now.Add(s.IntentTTL),
	}, nil
}

// sats writes an amount in whole sats with thousands separators.
func sats(m contracts.MilliSat) string {
	n := strconv.FormatInt(m.Sats(), 10)
	neg := strings.HasPrefix(n, "-")
	n = strings.TrimPrefix(n, "-")
	for i := len(n) - 3; i > 0; i -= 3 {
		n = n[:i] + "," + n[i:]
	}
	if neg {
		n = "-" + n
	}
	return n
}

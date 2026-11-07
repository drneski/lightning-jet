// Package peer groups a snapshot's channels by peer, so Jet decides about a
// peer's channels together. LND forwards a payment over any channel to the
// peer that can carry it and whose policy the payment satisfies, picking at
// random among them, and LND senders plan with the highest fee and time lock
// among a peer's channels. So a peer's channels behave as one: their forwards
// and failures split between them by chance, and a fee set on one alone
// either goes unused or keeps payments off the other's balance.
package peer

import (
	"sort"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Group is one peer's channels, in id order, and Combined, the channels as
// one: what the strategies evaluate.
type Group struct {
	Channels []contracts.ChannelObservation
	Combined contracts.ChannelObservation
}

// IDs returns the group's channel ids when the peer has several, for a
// record's chan_ids, and nil when it has one.
func (g Group) IDs() []contracts.ChanID {
	if len(g.Channels) < 2 {
		return nil
	}
	ids := make([]contracts.ChanID, len(g.Channels))
	for i, c := range g.Channels {
		ids[i] = c.ChanID
	}
	return ids
}

// Groups returns the channels grouped by peer, each group in id order and
// the groups in the order their peers first appear: for a snapshot's
// channels, which are in id order, the order of each group's first channel.
func Groups(channels []contracts.ChannelObservation) []Group {
	var order []string
	byPeer := map[string][]contracts.ChannelObservation{}
	for _, c := range channels {
		if _, ok := byPeer[c.Peer]; !ok {
			order = append(order, c.Peer)
		}
		byPeer[c.Peer] = append(byPeer[c.Peer], c)
	}
	var out []Group
	for _, p := range order {
		out = append(out, New(byPeer[p]...))
	}
	return out
}

// New groups channels to one peer.
func New(channels ...contracts.ChannelObservation) Group {
	chs := append([]contracts.ChannelObservation(nil), channels...)
	sort.Slice(chs, func(i, j int) bool { return chs[i].ChanID < chs[j].ChanID })
	return Group{Channels: chs, Combined: combine(chs)}
}

// Find returns the group that has the channel.
func Find(groups []Group, id contracts.ChanID) (Group, bool) {
	for _, g := range groups {
		for _, c := range g.Channels {
			if c.ChanID == id {
				return g, true
			}
		}
	}
	return Group{}, false
}

// combine makes one observation of a peer's channels, which are in id order.
// It takes the first channel's id, the oldest, and adds up balances, forwards
// and failures. Our policy is the enabled one with the highest fee, which is
// what LND senders plan with. The peer's is its enabled one with the lowest
// fee: a refill can name the channel it comes back through, so the cheapest
// is what one costs at least. Data quality is the worst of the channels',
// each issue naming its channel.
func combine(chs []contracts.ChannelObservation) contracts.ChannelObservation {
	if len(chs) == 1 {
		return chs[0]
	}
	c := contracts.ChannelObservation{ChanID: chs[0].ChanID, Peer: chs[0].Peer,
		DataQuality: contracts.DataQuality{Level: contracts.DataGood}}
	var ours, theirs []*contracts.Policy
	for _, o := range chs {
		if c.PeerAlias == "" {
			c.PeerAlias = o.PeerAlias
		}
		c.Active = c.Active || o.Active
		c.CapacityMsat += o.CapacityMsat
		c.LocalMsat += o.LocalMsat
		c.RemoteMsat += o.RemoteMsat
		ours, theirs = append(ours, o.OurPolicy), append(theirs, o.PeerPolicy)

		f := &c.Forwards
		f.WindowHours = max(f.WindowHours, o.Forwards.WindowHours)
		f.OutCount += o.Forwards.OutCount
		f.OutAmountMsat += o.Forwards.OutAmountMsat
		f.OutFeeMsat += o.Forwards.OutFeeMsat
		f.InCount += o.Forwards.InCount
		f.InAmountMsat += o.Forwards.InAmountMsat

		if o.Failures != nil {
			if c.Failures == nil {
				c.Failures = &contracts.Failures{}
			}
			f := c.Failures
			f.WindowHours = max(f.WindowHours, o.Failures.WindowHours)
			f.CoverageHours = max(f.CoverageHours, o.Failures.CoverageHours)
			f.BalanceFailures += o.Failures.BalanceFailures
			f.BalanceFailuresMsat += o.Failures.BalanceFailuresMsat
		}
		if c.Competition == nil {
			c.Competition = o.Competition
		}
		c.DataQuality.Level = contracts.Worse(c.DataQuality.Level, o.DataQuality.Level)
		for _, issue := range o.DataQuality.Issues {
			c.DataQuality.Issues = append(c.DataQuality.Issues, o.ChanID.SCID()+": "+issue)
		}
	}
	c.OurPolicy, c.PeerPolicy = pick(ours, true), pick(theirs, false)
	return c
}

// pick returns a copy of the enabled policy with the highest fee rate, or the
// lowest, the base fee breaking a tie the same way; or the first known one if
// none is enabled.
func pick(ps []*contracts.Policy, highest bool) *contracts.Policy {
	var best, first *contracts.Policy
	better := func(p, q *contracts.Policy) bool {
		if p.FeePPM != q.FeePPM {
			return p.FeePPM > q.FeePPM == highest
		}
		return p.BaseMsat != q.BaseMsat && p.BaseMsat > q.BaseMsat == highest
	}
	for _, p := range ps {
		if p == nil {
			continue
		}
		if first == nil {
			first = p
		}
		if !p.Disabled && (best == nil || better(p, best)) {
			best = p
		}
	}
	if best == nil {
		best = first
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

// FeesDiffer reports whether the group's channels charge different fees: a
// different rate or base fee, where both are known.
func (g Group) FeesDiffer() bool {
	var seen *contracts.Policy
	for _, c := range g.Channels {
		p := c.OurPolicy
		if p == nil {
			continue
		}
		if seen != nil && (p.FeePPM != seen.FeePPM || p.BaseMsat != seen.BaseMsat) {
			return true
		}
		seen = p
	}
	return false
}

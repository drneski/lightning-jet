// Package observe assembles a snapshot of the node from LND: its channels,
// both sides' fee policies, recent forwards, the forwards that failed for lack
// of balance as Jet recorded them, and what competing channels advertise, with
// data quality noted for each channel. It records what it read; the only
// arithmetic is totals and percentiles of those readings.
package observe

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/record"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

// FailureSource is the local record of failed HTLCs.
type FailureSource interface {
	BalanceFailuresSince(ctx context.Context, since time.Time) (map[contracts.ChanID]store.BalanceFailures, error)
	Coverage(ctx context.Context, stream string, since, now time.Time) (time.Duration, error)
}

// Observer takes snapshots.
type Observer struct {
	LND   lnd.Reader
	Now   func() time.Time
	NewID func() string
	// Window is how far back forwards and failures are summarized.
	Window time.Duration
	// CallTimeout bounds each call to LND.
	CallTimeout time.Duration
	// Failures is the recorded HTLC stream. Without it, snapshots have no
	// failure summary.
	Failures FailureSource
	// GossipEvery is how often each peer's competing channels are read again.
	// Zero leaves competition out.
	GossipEvery time.Duration

	gossip map[string]*contracts.Competition
}

// Snapshot reads the node and returns a sealed snapshot. It fails only if LND
// can't say who the node is or what channels it has. Anything else missing is
// recorded as a data quality issue on the channels it affects.
func (o *Observer) Snapshot(ctx context.Context) (*contracts.Snapshot, error) {
	now := contracts.Timestamp(o.Now())
	var sources []contracts.Source
	read := func(name string) {
		sources = append(sources, contracts.Source{Name: name, ReadAt: contracts.Timestamp(o.Now())})
	}

	info, err := callLND(ctx, o.CallTimeout, o.LND.GetInfo)
	if err != nil {
		return nil, fmt.Errorf("observe: reading node info: %w", err)
	}
	read("lnd.getinfo")
	channels, err := callLND(ctx, o.CallTimeout, o.LND.ListChannels)
	if err != nil {
		return nil, fmt.Errorf("observe: listing channels: %w", err)
	}
	read("lnd.listchannels")
	sort.Slice(channels, func(i, j int) bool { return channels[i].ChanID < channels[j].ChanID })

	start := now.Add(-o.Window)
	forwards, fwErr := callLND(ctx, o.CallTimeout, func(c context.Context) ([]lnd.Forward, error) {
		return o.LND.ForwardingHistory(c, start, now)
	})
	if fwErr == nil {
		read("lnd.forwardinghistory")
	}
	out, in := summarize(forwards)

	var failures map[contracts.ChanID]store.BalanceFailures
	var coverage time.Duration
	var failErr error
	if o.Failures != nil {
		failures, failErr = o.Failures.BalanceFailuresSince(ctx, start)
		if failErr == nil {
			coverage, failErr = o.Failures.Coverage(ctx, record.StreamHTLC, start, now)
		}
		if failErr == nil {
			read("jet.htlc_stream")
		}
	}
	gossipRead := false

	snap := &contracts.Snapshot{
		ID: o.NewID(), Schema: contracts.SchemaSnapshot, NodePubkey: info.IdentityPubkey,
		BlockHeight: info.BlockHeight, TakenAt: now,
	}
	for _, ch := range channels {
		obs := contracts.ChannelObservation{
			ChanID: ch.ChanID, Peer: ch.RemotePubkey, Active: ch.Active,
			CapacityMsat: contracts.MilliSat(ch.CapacitySat * 1000),
			LocalMsat:    contracts.MilliSat(ch.LocalSat * 1000),
			RemoteMsat:   contracts.MilliSat(ch.RemoteSat * 1000),
			DataQuality:  contracts.DataQuality{Level: contracts.DataGood},
		}
		dq := &obs.DataQuality
		if !info.SyncedToChain {
			dq.Add(contracts.DataDegraded, "node not synced to the chain")
		}
		if !info.SyncedToGraph {
			dq.Add(contracts.DataDegraded, "node not synced to the graph, so the peer's policy may be stale")
		}
		if !ch.Active {
			dq.Add(contracts.DataDegraded, "channel inactive")
		}

		edge, err := callLND(ctx, o.CallTimeout, func(c context.Context) (lnd.Edge, error) { return o.LND.GetChanInfo(c, ch.ChanID) })
		switch {
		case err != nil:
			dq.Add(contracts.DataInsufficient, "channel policies unavailable: "+err.Error())
		case edge.Node1Pub == info.IdentityPubkey:
			obs.OurPolicy, obs.PeerPolicy = policy(edge.Node1Policy), policy(edge.Node2Policy)
		case edge.Node2Pub == info.IdentityPubkey:
			obs.OurPolicy, obs.PeerPolicy = policy(edge.Node2Policy), policy(edge.Node1Policy)
		default:
			dq.Add(contracts.DataInsufficient, "the graph's record of this channel does not include this node")
		}
		if err == nil && obs.OurPolicy == nil {
			dq.Add(contracts.DataInsufficient, "our policy unknown")
		}
		if err == nil && obs.PeerPolicy == nil {
			dq.Add(contracts.DataDegraded, "the peer's policy has not been gossiped yet")
		}

		switch {
		case o.Failures == nil:
		case failErr != nil:
			dq.Add(contracts.DataDegraded, "failed-HTLC record unavailable: "+failErr.Error())
		default:
			f := failures[ch.ChanID]
			obs.Failures = &contracts.Failures{
				WindowHours: int64(o.Window / time.Hour), CoverageHours: int64(coverage / time.Hour),
				BalanceFailures: f.Count, BalanceFailuresMsat: f.AmountMsat,
			}
		}

		if o.GossipEvery > 0 {
			comp, fresh, err := o.competition(ctx, ch.RemotePubkey, info.IdentityPubkey, now)
			switch {
			case err != nil:
				// A missing prior doesn't lower the quality of what was observed.
				dq.Add(contracts.DataGood, "competing channels unavailable: "+err.Error())
			default:
				obs.Competition = comp
				gossipRead = gossipRead || fresh
			}
		}

		if fwErr != nil {
			dq.Add(contracts.DataInsufficient, "forwarding history unavailable: "+fwErr.Error())
		} else {
			f := out[ch.ChanID]
			f.InCount, f.InAmountMsat = in[ch.ChanID].InCount, in[ch.ChanID].InAmountMsat
			f.WindowHours = int64(o.Window / time.Hour)
			obs.Forwards = f
		}
		snap.Channels = append(snap.Channels, obs)
	}
	read("lnd.getchaninfo")
	if gossipRead {
		read("lnd.graph.node")
	}
	snap.Sources = sources
	if err := snap.Seal(); err != nil {
		return nil, fmt.Errorf("observe: sealing snapshot: %w", err)
	}
	return snap, nil
}

// competition summarizes what the other channels into a peer advertise: for
// each, the policy of the node on its far side, which is what that node charges
// to forward into the peer. Our own channels are left out. Readings are cached
// for GossipEvery; fresh reports whether this call read the graph.
func (o *Observer) competition(ctx context.Context, peer, us string, now time.Time) (*contracts.Competition, bool, error) {
	if c, ok := o.gossip[peer]; ok && now.Sub(c.ReadAt) < o.GossipEvery {
		return c, false, nil
	}
	edges, err := callLND(ctx, o.CallTimeout, func(c context.Context) ([]lnd.Edge, error) { return o.LND.NodeChannels(c, peer) })
	if err != nil {
		return nil, false, err
	}
	var ppms, bases []int64
	for _, e := range edges {
		var p *lnd.RoutingPolicy
		switch {
		case e.Node1Pub == us || e.Node2Pub == us:
			continue
		case e.Node1Pub == peer:
			p = e.Node2Policy
		case e.Node2Pub == peer:
			p = e.Node1Policy
		}
		if p == nil || p.Disabled {
			continue
		}
		ppms = append(ppms, p.FeeRateMilliMsat)
		bases = append(bases, p.FeeBaseMsat)
	}
	c := &contracts.Competition{ReadAt: now, Channels: len(ppms)}
	if len(ppms) > 0 {
		c.FeePPMP25, c.FeePPMMedian, c.FeePPMP75 = percentile(ppms, 25), percentile(ppms, 50), percentile(ppms, 75)
		c.BaseMsatMedian = contracts.MilliSat(percentile(bases, 50))
	}
	if o.gossip == nil {
		o.gossip = map[string]*contracts.Competition{}
	}
	o.gossip[peer] = c
	return c, true, nil
}

// percentile returns the value at rank (n-1)*p/100, rounded down, of vs, which
// it sorts: always one of the readings, never an interpolation.
func percentile(vs []int64, p int) int64 {
	sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
	return vs[(len(vs)-1)*p/100]
}

func callLND[T any](ctx context.Context, timeout time.Duration, f func(context.Context) (T, error)) (T, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return f(c)
}

func policy(p *lnd.RoutingPolicy) *contracts.Policy {
	if p == nil {
		return nil
	}
	return &contracts.Policy{
		BaseMsat: contracts.MilliSat(p.FeeBaseMsat), FeePPM: p.FeeRateMilliMsat, TimeLockDelta: p.TimeLockDelta,
		MinHTLCMsat: contracts.MilliSat(p.MinHTLCMsat), MaxHTLCMsat: contracts.MilliSat(p.MaxHTLCMsat),
		Disabled: p.Disabled, LastUpdate: contracts.Timestamp(p.LastUpdate),
	}
}

// summarize totals forwards out of and into each channel.
func summarize(fws []lnd.Forward) (out, in map[contracts.ChanID]contracts.Forwards) {
	out, in = map[contracts.ChanID]contracts.Forwards{}, map[contracts.ChanID]contracts.Forwards{}
	for _, fw := range fws {
		o := out[fw.ChanIDOut]
		o.OutCount++
		o.OutAmountMsat += contracts.MilliSat(fw.AmtOutMsat)
		o.OutFeeMsat += contracts.MilliSat(fw.FeeMsat)
		out[fw.ChanIDOut] = o
		i := in[fw.ChanIDIn]
		i.InCount++
		i.InAmountMsat += contracts.MilliSat(fw.AmtInMsat)
		in[fw.ChanIDIn] = i
	}
	return out, in
}

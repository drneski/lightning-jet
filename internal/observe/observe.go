// Package observe assembles a snapshot of the node from LND: its channels,
// both sides' fee policies and recent forwards, with data quality noted for
// each channel. It records what LND reported and derives nothing.
package observe

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
)

// Observer takes snapshots.
type Observer struct {
	LND   lnd.Reader
	Now   func() time.Time
	NewID func() string
	// Window is how far back forwards are summarized.
	Window time.Duration
	// CallTimeout bounds each call to LND.
	CallTimeout time.Duration
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
	snap.Sources = sources
	if err := snap.Seal(); err != nil {
		return nil, fmt.Errorf("observe: sealing snapshot: %w", err)
	}
	return snap, nil
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

package observe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/lnd/lndtest"
)

const (
	us   = "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	peer = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var now = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func fakeNode() *lndtest.Fake {
	ourPolicy := &lnd.RoutingPolicy{FeeBaseMsat: 0, FeeRateMilliMsat: 500, TimeLockDelta: 80}
	peerPolicy := &lnd.RoutingPolicy{FeeBaseMsat: 1000, FeeRateMilliMsat: 400, TimeLockDelta: 40}
	return &lndtest.Fake{
		Info: lnd.Info{IdentityPubkey: us, BlockHeight: 900000, SyncedToChain: true, SyncedToGraph: true},
		Channels: []lnd.Channel{
			{ChanID: 2, RemotePubkey: peer, Active: false, CapacitySat: 2_000_000, LocalSat: 1_000_000, RemoteSat: 990_000},
			{ChanID: 1, RemotePubkey: peer, Active: true, CapacitySat: 5_000_000, LocalSat: 4_500_000, RemoteSat: 480_000},
		},
		Edges: map[contracts.ChanID]lnd.Edge{
			// We are node1 on channel 1 and node2 on channel 2.
			1: {ChanID: 1, Node1Pub: us, Node2Pub: peer, Node1Policy: ourPolicy, Node2Policy: peerPolicy},
			2: {ChanID: 2, Node1Pub: peer, Node2Pub: us, Node1Policy: nil, Node2Policy: ourPolicy},
		},
		Forwards: []lnd.Forward{
			{Time: now.Add(-time.Hour), ChanIDIn: 2, ChanIDOut: 1, AmtInMsat: 100_500_000, AmtOutMsat: 100_000_000, FeeMsat: 50_000},
			{Time: now.Add(-48 * time.Hour), ChanIDIn: 2, ChanIDOut: 1, AmtInMsat: 200_100_000, AmtOutMsat: 200_000_000, FeeMsat: 100_000},
			{Time: now.Add(-40 * 24 * time.Hour), ChanIDIn: 2, ChanIDOut: 1, AmtInMsat: 1, AmtOutMsat: 1, FeeMsat: 1}, // outside the window
		},
	}
}

func observer(f lnd.Reader) *Observer {
	n := 0
	return &Observer{LND: f, Now: func() time.Time { return now }, NewID: func() string { n++; return "id" + string(rune('0'+n)) },
		Window: 30 * 24 * time.Hour, CallTimeout: time.Second}
}

func TestSnapshot(t *testing.T) {
	snap, err := observer(fakeNode()).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("snapshot does not validate: %v", err)
	}
	if snap.NodePubkey != us || snap.BlockHeight != 900000 || len(snap.Channels) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Channels[0].ChanID != 1 {
		t.Errorf("channels are not in id order: first is %s", snap.Channels[0].ChanID)
	}

	c1, _ := snap.Channel(1)
	if c1.LocalMsat != 4_500_000_000 || c1.CapacityMsat != 5_000_000_000 {
		t.Errorf("channel 1 balances = %+v", c1)
	}
	if c1.OurPolicy == nil || c1.OurPolicy.FeePPM != 500 || c1.PeerPolicy == nil || c1.PeerPolicy.BaseMsat != 1000 {
		t.Errorf("channel 1 policies: ours %+v, peer's %+v", c1.OurPolicy, c1.PeerPolicy)
	}
	if f := c1.Forwards; f.OutCount != 2 || f.OutFeeMsat != 150_000 || f.OutAmountMsat != 300_000_000 || f.WindowHours != 720 {
		t.Errorf("channel 1 forwards = %+v", f)
	}
	if c1.DataQuality.Level != contracts.DataGood {
		t.Errorf("channel 1 data quality = %+v", c1.DataQuality)
	}

	c2, _ := snap.Channel(2)
	if c2.OurPolicy == nil || c2.OurPolicy.FeePPM != 500 || c2.PeerPolicy != nil {
		t.Errorf("channel 2, where we are node2: ours %+v, peer's %+v", c2.OurPolicy, c2.PeerPolicy)
	}
	if c2.Forwards.InCount != 2 || c2.Forwards.InAmountMsat != 300_600_000 {
		t.Errorf("channel 2 forwards = %+v", c2.Forwards)
	}
	if c2.DataQuality.Level != contracts.DataDegraded || !hasIssue(c2.DataQuality, "inactive") || !hasIssue(c2.DataQuality, "gossiped") {
		t.Errorf("channel 2 data quality = %+v", c2.DataQuality)
	}
}

func TestMissingForwardsMakeDataInsufficient(t *testing.T) {
	f := fakeNode()
	f.Errors = map[string]error{"ForwardingHistory": errors.New("timeout")}
	snap, err := observer(f).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("a missing forwarding history failed the snapshot: %v", err)
	}
	for _, c := range snap.Channels {
		if c.DataQuality.Level != contracts.DataInsufficient || !hasIssue(c.DataQuality, "forwarding history unavailable") {
			t.Errorf("channel %s data quality = %+v", c.ChanID, c.DataQuality)
		}
	}
}

func TestUnknownPolicyMakesDataInsufficient(t *testing.T) {
	f := fakeNode()
	f.Errors = map[string]error{"GetChanInfo": errors.New("edge not found")}
	snap, err := observer(f).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := snap.Channel(1)
	if c1.DataQuality.Level != contracts.DataInsufficient || !hasIssue(c1.DataQuality, "edge not found") {
		t.Errorf("data quality = %+v", c1.DataQuality)
	}
}

func TestNodeNotSyncedDegradesEveryChannel(t *testing.T) {
	f := fakeNode()
	f.Info.SyncedToChain = false
	snap, err := observer(f).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := snap.Channel(1)
	if c1.DataQuality.Level != contracts.DataDegraded || !hasIssue(c1.DataQuality, "not synced to the chain") {
		t.Errorf("data quality = %+v", c1.DataQuality)
	}
}

func TestNoNodeInfoFailsTheSnapshot(t *testing.T) {
	f := fakeNode()
	f.Errors = map[string]error{"GetInfo": errors.New("connection refused")}
	if _, err := observer(f).Snapshot(context.Background()); err == nil {
		t.Error("a snapshot was taken without knowing the node")
	}
}

func hasIssue(q contracts.DataQuality, sub string) bool {
	for _, i := range q.Issues {
		if strings.Contains(i, sub) {
			return true
		}
	}
	return false
}

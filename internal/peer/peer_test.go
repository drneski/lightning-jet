package peer

import (
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

func ch(id contracts.ChanID, peer string, local contracts.MilliSat, ppm int64) contracts.ChannelObservation {
	return contracts.ChannelObservation{ChanID: id, Peer: peer, Active: true, CapacityMsat: 5_000_000_000, LocalMsat: local,
		RemoteMsat: 5_000_000_000 - local, OurPolicy: &contracts.Policy{FeePPM: ppm, BaseMsat: 1000},
		PeerPolicy:  &contracts.Policy{FeePPM: 50},
		DataQuality: contracts.DataQuality{Level: contracts.DataGood},
		Forwards:    contracts.Forwards{WindowHours: 720, OutCount: 3, OutAmountMsat: 300_000_000, OutFeeMsat: 30_000}}
}

func TestGroupsByPeerInIDOrder(t *testing.T) {
	gs := Groups([]contracts.ChannelObservation{ch(1, "02aa", 0, 100), ch(2, "02bb", 0, 100), ch(3, "02aa", 0, 100)})
	if len(gs) != 2 || gs[0].Combined.ChanID != 1 || len(gs[0].Channels) != 2 || gs[1].Combined.ChanID != 2 {
		t.Fatalf("groups = %+v", gs)
	}
	if ids := gs[0].IDs(); len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Errorf("ids = %v", ids)
	}
	if ids := gs[1].IDs(); ids != nil {
		t.Errorf("a peer with one channel lists ids %v", ids)
	}
	if g, ok := Find(gs, 3); !ok || g.Combined.ChanID != 1 {
		t.Errorf("Find(3) = %+v, %v", g, ok)
	}
}

// One channel is its own combination, unchanged.
func TestOneChannelIsUnchanged(t *testing.T) {
	c := ch(1, "02aa", 100_000_000, 400)
	c.DataQuality.Add(contracts.DataDegraded, "channel inactive")
	g := New(c)
	if g.Combined.DataQuality.Issues[0] != "channel inactive" || g.Combined.OurPolicy != c.OurPolicy {
		t.Errorf("combined = %+v", g.Combined)
	}
}

func TestCombineAddsUpAndPicksEachSidesPolicy(t *testing.T) {
	a, b := ch(1, "02aa", 100_000_000, 1000), ch(2, "02aa", 4_900_000_000, 900)
	b.Forwards.InCount, b.Forwards.InAmountMsat = 2, 50_000_000
	a.Failures = &contracts.Failures{WindowHours: 720, CoverageHours: 300, BalanceFailures: 4, BalanceFailuresMsat: 800_000_000}
	b.Failures = &contracts.Failures{WindowHours: 720, CoverageHours: 300, BalanceFailures: 1, BalanceFailuresMsat: 100_000_000}
	b.PeerPolicy.FeePPM = 900
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a.Refills = &contracts.Refills{WindowHours: 720, Attempts: []contracts.RefillAttempt{{At: day, Result: contracts.AttemptSucceeded}}}
	b.Refills = &contracts.Refills{WindowHours: 720, Omitted: 2, Attempts: []contracts.RefillAttempt{{At: day.Add(time.Hour), Result: contracts.AttemptNoLiquidity}}}
	b.Active = false
	b.DataQuality.Add(contracts.DataDegraded, "channel inactive")
	c := New(b, a).Combined

	if c.ChanID != 1 || c.CapacityMsat != 10_000_000_000 || c.LocalMsat != 5_000_000_000 || c.RemoteMsat != 5_000_000_000 || !c.Active {
		t.Errorf("balances = %+v", c)
	}
	if c.Forwards.OutCount != 6 || c.Forwards.OutFeeMsat != 60_000 || c.Forwards.InCount != 2 || c.Forwards.WindowHours != 720 {
		t.Errorf("forwards = %+v", c.Forwards)
	}
	if f := c.Failures; f.BalanceFailures != 5 || f.BalanceFailuresMsat != 900_000_000 || f.CoverageHours != 300 {
		t.Errorf("failures = %+v", f)
	}
	// Ours is the highest, what senders plan with; the peer's the lowest, the
	// cheapest way back in.
	if c.OurPolicy.FeePPM != 1000 || c.PeerPolicy.FeePPM != 50 {
		t.Errorf("policies = %+v, %+v", c.OurPolicy, c.PeerPolicy)
	}
	if r := c.Refills; r == nil || len(r.Attempts) != 2 || r.Attempts[0].Result != contracts.AttemptNoLiquidity || r.Omitted != 2 {
		t.Errorf("refills = %+v, want both channels', newest first", r)
	}
	if c.OurPolicy == a.OurPolicy {
		t.Error("the combined policy is the channel's own, not a copy")
	}
	if c.DataQuality.Level != contracts.DataDegraded || len(c.DataQuality.Issues) != 1 ||
		!strings.HasPrefix(c.DataQuality.Issues[0], "0x0x2: ") {
		t.Errorf("data quality = %+v", c.DataQuality)
	}
}

// A disabled policy doesn't count: senders don't route through it.
func TestADisabledPolicyIsPassedOver(t *testing.T) {
	a, b := ch(1, "02aa", 0, 500), ch(2, "02aa", 0, 2000)
	b.OurPolicy.Disabled = true
	if got := New(a, b).Combined.OurPolicy.FeePPM; got != 500 {
		t.Errorf("fee = %d, want 500", got)
	}
	a.OurPolicy.Disabled = true
	if got := New(a, b).Combined.OurPolicy; got == nil || got.FeePPM != 500 {
		t.Errorf("with every policy disabled: %+v, want the first", got)
	}
}

func TestTiesGoToTheBaseFee(t *testing.T) {
	a, b := ch(1, "02aa", 0, 1000), ch(2, "02aa", 0, 1000)
	b.OurPolicy.BaseMsat, b.PeerPolicy.BaseMsat = 0, 0
	a.PeerPolicy.BaseMsat = 1000
	c := New(a, b).Combined
	if c.OurPolicy.BaseMsat != 1000 || c.PeerPolicy.BaseMsat != 0 {
		t.Errorf("base fees = %d ours, %d theirs", c.OurPolicy.BaseMsat, c.PeerPolicy.BaseMsat)
	}
}

func TestFeesDiffer(t *testing.T) {
	a, b := ch(1, "02aa", 0, 1000), ch(2, "02aa", 0, 1000)
	if New(a, b).FeesDiffer() {
		t.Error("equal fees differ")
	}
	b.OurPolicy = &contracts.Policy{FeePPM: 1000, BaseMsat: 0}
	if !New(a, b).FeesDiffer() {
		t.Error("different base fees don't differ")
	}
}

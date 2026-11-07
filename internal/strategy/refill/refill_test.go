package refill

import (
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
)

var now = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

func strategy() *Strategy {
	n := 0
	return &Strategy{Config: DefaultConfig, Mode: contracts.ModeShadow, Now: func() time.Time { return now },
		NewID: func() string { n++; return "x" + string(rune('0'+n)) }}
}

func mp(m contracts.MilliSat) *contracts.MilliSat { return &m }
func ip(v int64) *int64                           { return &v }

var snap = &contracts.Snapshot{ID: "s1"}

// A synthetic node. The target is 2% full at 300 ppm and forwarded 3M sats
// out in 30 days, so 1.4M sats of demand over 14 days, earning 300 to 420
// sats. Channel 2's peer is the neighbor, the cheapest way into the target's
// peer; channel 3 has liquidity to spare but no cheap way in.
func node(feePPM int64, low, expected contracts.MilliSat) (Channel, []Channel) {
	good := contracts.DataQuality{Level: contracts.DataGood}
	target := Channel{
		Obs: contracts.ChannelObservation{ChanID: 1, Peer: "02aa", Active: true, CapacityMsat: 5_000_000_000, LocalMsat: 100_000_000,
			OurPolicy: &contracts.Policy{FeePPM: feePPM}, DataQuality: good,
			Forwards:    contracts.Forwards{WindowHours: 720, OutCount: 20, OutAmountMsat: 3_000_000_000},
			Competition: &contracts.Competition{Channels: 50, Neighbor: &contracts.Neighbor{Pubkey: "02nb", FeePPM: 5}}},
		Revenue: &contracts.Estimate{ID: "rev1", Kind: "outbound_revenue", HorizonHours: 336, ExpectedMsat: mp(expected), LowMsat: mp(low),
			HighMsat: mp(expected * 2), Interval: "p10-p90", EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceMedium},
			Evidence: []contracts.Evidence{{Kind: "forwards_out", WindowHours: 720, Count: 20}}},
		Refill: &contracts.Estimate{ID: "rc1", Kind: "replacement_cost", HorizonHours: 336, LowPPM: ip(500), ExpectedPPM: ip(505), HighPPM: ip(900),
			Interval: "advertised fees", EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceLow}},
	}
	idle := func(id contracts.ChanID, peer string, local contracts.MilliSat) Channel {
		return Channel{Obs: contracts.ChannelObservation{ChanID: id, Peer: peer, Active: true, CapacityMsat: 5_000_000_000, LocalMsat: local,
			OurPolicy: &contracts.Policy{FeePPM: 100}, DataQuality: good},
			Revenue: &contracts.Estimate{ID: "rev" + id.String(), Kind: "outbound_revenue", HorizonHours: 336,
				EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceNone}}}
	}
	return target, []Channel{target, idle(2, "02nb", 4_900_000_000), idle(3, "02cc", 4_950_000_000)}
}

func TestARefillThatDoesntPayIsAnAbstention(t *testing.T) {
	target, all := node(300, 300_000, 420_000)
	intent, abst := strategy().Evaluate(snap, target, all)
	if intent != nil || abst == nil || abst.Reason != contracts.NoProfitableAction || abst.Action != contracts.ActionRebalance {
		t.Fatalf("got intent %+v, abstention %+v", intent, abst)
	}
	// 1.4M sats through the neighbor at 505 ppm costs 707 sats, against 300
	// to 420 earned.
	if !strings.Contains(abst.Detail, "a refill of 1,400,000 sats from 0x0x2 would cost about 707 sats (505 ppm") ||
		!strings.Contains(abst.Detail, "earn 300 to 420 sats over 14 days at 300 ppm") {
		t.Errorf("detail = %q", abst.Detail)
	}
	if len(abst.WouldChangeIf) != 2 || abst.WouldChangeIf[0] != "our fee rises above about 708 ppm" ||
		abst.WouldChangeIf[1] != "a refill costs under about 214 ppm" {
		t.Errorf("would change if %q", abst.WouldChangeIf)
	}
	if err := abst.Validate(); err != nil {
		t.Errorf("abstention does not validate: %v", err)
	}
}

func TestARefillThatPaysIsProposed(t *testing.T) {
	target, all := node(1000, 1_000_000, 1_400_000)
	intent, abst := strategy().Evaluate(snap, target, all)
	if intent == nil {
		t.Fatalf("abstained: %+v", abst)
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("intent does not validate: %v", err)
	}
	if intent.Action != contracts.ActionRebalance || *intent.Target.SourceChanID != 2 || intent.Parameters.Rebalance.AmountMsat != 1_400_000_000 ||
		*intent.Limits.MaxFeeMsat != 1_000_000 || !intent.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("intent = %+v", intent)
	}
	if !strings.Contains(intent.Rationale.Summary, "may pay at most 1,000 sats") || len(intent.Rationale.EstimateIDs) != 3 {
		t.Errorf("rationale = %+v", intent.Rationale)
	}
}

// A refill is valued at the current fee, not the fees of the window. Here
// the forwards were at 300 ppm, earning 300 to 420 sats, and the fee is now
// 1,000: the same volume earns 1,000 to 1,400, more than the 707 it costs.
func TestARefillIsValuedAtTheCurrentFee(t *testing.T) {
	target, all := node(1000, 300_000, 420_000)
	intent, abst := strategy().Evaluate(snap, target, all)
	if intent == nil {
		t.Fatalf("abstained: %+v", abst)
	}
	if *intent.Limits.MaxFeeMsat != 1_000_000 || !strings.Contains(intent.Rationale.Summary, "earn 1,000 to 1,400 sats over 14 days at 1000 ppm, if forwards out keep their volume") {
		t.Errorf("intent = %+v", intent)
	}
}

// Without the neighbor, the source is the channel with the most to spare,
// priced at the high end of the replacement cost: 1.4M sats at 900 ppm is
// 1,260 sats, more than the 1,000 earned at the low end.
func TestWithoutTheNeighborTheHighEndIsTheCost(t *testing.T) {
	target, all := node(1000, 1_000_000, 1_400_000)
	all[1].Obs.LocalMsat = 2_000_000_000 // the neighbor has nothing to spare
	_, abst := strategy().Evaluate(snap, target, all)
	if abst == nil || !strings.Contains(abst.Detail, "from 0x0x3 would cost about 1,260 sats (900 ppm, the high end") {
		t.Fatalf("got %+v", abst)
	}
}

// A channel that routes enough to be valued isn't a source.
func TestASourceWithDemandOfItsOwnIsPassedOver(t *testing.T) {
	target, all := node(1000, 1_000_000, 1_400_000)
	all[1].Revenue.EconomicConfidence.Level = contracts.ConfidenceMedium
	intent, abst := strategy().Evaluate(snap, target, all)
	if intent != nil || abst == nil || !strings.Contains(abst.Detail, "from 0x0x3") {
		t.Errorf("got intent %+v, abstention %+v", intent, abst)
	}
}

func TestReasonsToAbstain(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(target *Channel, all []Channel)
		reason contracts.AbstentionReason
		detail string
	}{
		"too few forwards": {func(t *Channel, _ []Channel) { t.Revenue.EconomicConfidence.Level = contracts.ConfidenceLow },
			contracts.InsufficientEvidence, "valuing a refill needs medium"},
		"no way to price it": {func(t *Channel, _ []Channel) { t.Refill.ExpectedPPM, t.Refill.HighPPM, t.Refill.LowPPM = nil, nil, nil },
			contracts.InsufficientEvidence, "no way to price a refill"},
		"too little demand": {func(t *Channel, _ []Channel) { t.Obs.Forwards.OutAmountMsat = 100_000_000 },
			contracts.NoProfitableAction, "refills under 100,000 sats aren't proposed"},
		"nothing to spare": {func(_ *Channel, all []Channel) {
			all[1].Obs.LocalMsat, all[2].Obs.LocalMsat = 3_000_000_000, 3_000_000_000
		},
			contracts.NoProfitableAction, "no channel has 1,400,000 sats to spare"},
		"untrusted inputs": {func(t *Channel, _ []Channel) {
			t.Obs.DataQuality = contracts.DataQuality{Level: contracts.DataInsufficient, Issues: []string{"forwarding history unavailable"}}
		}, contracts.InsufficientData, "forwarding history unavailable"},
	} {
		target, all := node(1000, 1_000_000, 1_400_000)
		tc.change(&target, all)
		all[0] = target
		intent, abst := strategy().Evaluate(snap, target, all)
		if intent != nil || abst == nil || abst.Reason != tc.reason || !strings.Contains(abst.Detail, tc.detail) {
			t.Errorf("%s: intent %+v, abstention %+v", name, intent, abst)
			continue
		}
		if err := abst.Validate(); err != nil {
			t.Errorf("%s: abstention does not validate: %v", name, err)
		}
	}
}

func TestOnlyDepletedChannelsAreTargets(t *testing.T) {
	s := strategy()
	for local, want := range map[contracts.MilliSat]bool{100_000_000: true, 999_000_000: true, 1_000_000_000: false, 4_000_000_000: false} {
		if got := s.Target(contracts.ChannelObservation{CapacityMsat: 5_000_000_000, LocalMsat: local}); got != want {
			t.Errorf("local %d msat of 5M sats: target = %v", local, got)
		}
	}
}

// Another channel to the target's peer is never the source: the refill would
// only move sats from one of the peer's channels to the other.
func TestTheTargetsOwnPeerIsNoSource(t *testing.T) {
	target, all := node(1000, 1_000_000, 1_400_000)
	all[1].Obs.LocalMsat = 2_000_000_000 // the neighbor has nothing to spare
	all[2].Obs.LocalMsat = 2_000_000_000 // nor has channel 3
	sibling := all[2]
	sibling.Obs.ChanID, sibling.Obs.Peer, sibling.Obs.LocalMsat = 4, target.Obs.Peer, 4_950_000_000
	_, abst := strategy().Evaluate(snap, target, append(all, sibling))
	if abst == nil || !strings.Contains(abst.Detail, "no channel has 1,400,000 sats to spare") {
		t.Fatalf("got %+v", abst)
	}
}

// A peer with several channels is refilled as one, and a source with several
// leaves through its fullest.
func TestAPeersChannelsAreRefilledAsOne(t *testing.T) {
	target, all := node(1000, 1_000_000, 1_400_000)
	second := target.Obs
	second.ChanID, second.LocalMsat = 6, 0
	target.Channels = []contracts.ChannelObservation{target.Obs, second}
	target.Obs.CapacityMsat *= 2 // 100k of 10M sats: still depleted

	// The neighbor has two channels, 2M and 4.95M sats on our side.
	a, b := all[1].Obs, all[1].Obs
	a.LocalMsat, b.ChanID, b.LocalMsat = 2_000_000_000, 5, 4_950_000_000
	nb := all[1]
	nb.Obs.CapacityMsat, nb.Obs.LocalMsat, nb.Channels = 10_000_000_000, a.LocalMsat+b.LocalMsat, []contracts.ChannelObservation{a, b}

	intent, abst := strategy().Evaluate(snap, target, []Channel{target, nb, all[2]})
	if intent == nil {
		t.Fatalf("abstained: %+v", abst)
	}
	if *intent.Target.SourceChanID != 5 || !strings.Contains(intent.Rationale.Summary, "from 0x0x5") {
		t.Errorf("source = %v, summary %q; want the fullest channel", *intent.Target.SourceChanID, intent.Rationale.Summary)
	}
	if ids := intent.Target.ChanIDs; len(ids) != 2 || ids[0] != 1 || ids[1] != 6 {
		t.Errorf("chan_ids = %v", ids)
	}
	if err := intent.Validate(); err != nil {
		t.Errorf("intent does not validate: %v", err)
	}
}

// Measured refills price a refill from any source at their median.
func TestMeasuredRefillsPriceEverySource(t *testing.T) {
	target, all := node(1000, 1_000_000, 1_400_000)
	all[1].Obs.LocalMsat = 2_000_000_000 // the neighbor has nothing to spare
	target.Refill.Interval, target.Refill.ExpectedPPM = economics.IntervalMeasured, ip(600)
	intent, abst := strategy().Evaluate(snap, target, all)
	if intent == nil {
		t.Fatalf("abstained: %+v", abst)
	}
	if !strings.Contains(intent.Rationale.Summary, "from 0x0x3 would cost about 840 sats (600 ppm, the median of the refills into it)") {
		t.Errorf("summary = %q", intent.Rationale.Summary)
	}
}

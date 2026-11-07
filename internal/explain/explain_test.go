package explain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

var at = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

const peerKey = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func mp(m contracts.MilliSat) *contracts.MilliSat { return &m }

func snapshot() *contracts.Snapshot {
	s := &contracts.Snapshot{ID: "01a17581-b830-7c3b-98d2-e6a4b5c6d7e8", Schema: contracts.SchemaSnapshot, NodePubkey: "02aa", TakenAt: at,
		Channels: []contracts.ChannelObservation{{
			ChanID: 111111111111111111, Peer: peerKey, Active: true, CapacityMsat: 5_000_000_000, LocalMsat: 4_500_000_000,
			OurPolicy: &contracts.Policy{FeePPM: 500}, PeerPolicy: &contracts.Policy{FeePPM: 400},
			Forwards:    contracts.Forwards{WindowHours: 720, OutCount: 3, OutFeeMsat: 150_000, InCount: 1},
			DataQuality: contracts.DataQuality{Level: contracts.DataGood},
		}}}
	s.Seal()
	return s
}

func estimate(level contracts.ConfidenceLevel) *contracts.Estimate {
	e := &contracts.Estimate{ID: "01a17581-bc18-7e4c-99f1-a3b2c4d5e6f7", Schema: contracts.SchemaEstimate, ChanID: 111111111111111111,
		Kind: "outbound_revenue", HorizonHours: 336, Interval: "p10-p90",
		EconomicConfidence: contracts.EconomicConfidence{Level: level, Basis: "3 outbound forwards in 720 hours"}}
	if level != contracts.ConfidenceNone {
		e.ExpectedMsat, e.LowMsat, e.HighMsat = mp(70_000), mp(18_000), mp(121_000)
	}
	return e
}

func abstentionEvaluation() *store.Evaluation {
	return &store.Evaluation{Snapshot: snapshot(), Estimates: []*contracts.Estimate{estimate(contracts.ConfidenceLow)},
		Abstention: &contracts.Abstention{ID: "01a17581-c000-7d1a-99b6-e2f0c1d3e4a5", Schema: contracts.SchemaAbstention,
			ChanID: 111111111111111111, Strategy: contracts.Component{Name: "fee.balance-band", Version: "0.1.0"},
			Reason: contracts.InsufficientEvidence, Detail: "outbound_revenue has economic confidence low (3 outbound forwards in 720 hours); this strategy needs medium",
			WouldChangeIf: []string{"at least 10 outbound forwards through this channel within 30 days"},
			RevisitAt:     at.Add(time.Hour), CreatedAt: at}}
}

func TestAbstentionText(t *testing.T) {
	x, err := Build(abstentionEvaluation())
	if err != nil {
		t.Fatal(err)
	}
	got := x.Text()
	want := `Channel 111111111111111111 with 03bbbbbbbbbbbbbb…

Observed    2026-10-26 00:00 UTC. Local 4,500,000 sats of 5,000,000 (90%).
            Our fee: base 0 msat, 500 ppm. Peer's fee to us: base 0 msat, 400 ppm.
            30 days: 3 forwards out, earning 150 sats; 1 in.
            Data quality: good.
Estimated   Outbound revenue, 14 days: 70 sats expected, 18 to 121 (p10-p90).
            Economic confidence: low (3 outbound forwards in 720 hours).
Proposed    Nothing (insufficient evidence): outbound_revenue has economic confidence low (3 outbound forwards in 720 hours); this strategy needs medium.
Decided     No intent, so no authorization.
Next        Evaluated again 2026-10-26 01:00 UTC.
            Would act if at least 10 outbound forwards through this channel within 30 days.
Refill      Not evaluated in this cycle: Jet evaluates refills of depleted channels only.
`
	if got != want {
		t.Errorf("text:\n%s\nwant:\n%s", got, want)
	}
}

func TestIntentText(t *testing.T) {
	ev := &store.Evaluation{Snapshot: snapshot(), Estimates: []*contracts.Estimate{estimate(contracts.ConfidenceMedium)},
		Intent: &contracts.Intent{ID: "i1", Action: contracts.ActionFeeUpdate, Target: contracts.Target{ChanID: 111111111111111111},
			Parameters: contracts.IntentParameters{Fee: &contracts.FeeParameters{FeePPM: 450, ObservedFeePPM: 500}},
			Rationale:  contracts.Rationale{Summary: "Experiment: lower the fee."}, ExpiresAt: at.Add(24 * time.Hour)},
		Decision: &contracts.Decision{Verdict: contracts.Rejected, Mode: contracts.ModeShadow, Rules: []contracts.RuleResult{
			{Rule: "mode", Passed: true, Detail: "the fees engine is shadow"},
			{Rule: "cooldown", Passed: false, Detail: "a change was approved 1h0m0s ago; the cooldown is 12 hours"}}},
		Outcomes: []*contracts.Outcome{{State: contracts.NotExecuted, Detail: "rejected: cooldown"}}}
	low, exp, high := int64(600), int64(605), int64(1000)
	ev.Estimates = append(ev.Estimates, &contracts.Estimate{Kind: "replacement_cost", HorizonHours: 336, Interval: "advertised fees",
		LowPPM: &low, ExpectedPPM: &exp, HighPPM: &high, EconomicConfidence: contracts.EconomicConfidence{Level: contracts.ConfidenceLow,
			Basis: "advertised fees only"}})
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	text := x.Text()
	for _, want := range []string{
		"            Replacement cost, 14 days: 605 ppm expected, 600 to 1,000 (advertised fees).",
		"Proposed    Change the fee from 500 to 450 ppm, base 0 msat unchanged, by 2026-10-27 00:00 UTC.",
		"Decided     Rejected in shadow mode: 1 of 2 rules passed.",
		"FAIL  cooldown: a change was approved 1h0m0s ago",
		"Next        Rejected: cooldown; evaluated again in the next cycle.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	if x.Next.Outcome != contracts.NotExecuted {
		t.Errorf("next outcome = %s", x.Next.Outcome)
	}
}

func TestJSONHasTheFiveParts(t *testing.T) {
	x, err := Build(abstentionEvaluation())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"chan_id", "observed", "estimated", "proposed", "next"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON is missing %q", k)
		}
	}
	if _, ok := m["decided"]; ok {
		t.Error("an abstention has a decided part")
	}
}

func TestBuildNeedsTheChannelInItsSnapshot(t *testing.T) {
	ev := abstentionEvaluation()
	ev.Abstention.ChanID = 42
	if _, err := Build(ev); err == nil {
		t.Error("an abstention for a channel missing from its snapshot was explained")
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 4500000: "4,500,000", -1234567: "-1,234,567"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFailuresAndPriorsAreShown(t *testing.T) {
	ev := abstentionEvaluation()
	ch := &ev.Snapshot.Channels[0]
	ch.Failures = &contracts.Failures{WindowHours: 720, CoverageHours: 48, BalanceFailures: 5, BalanceFailuresMsat: 1_250_000_000}
	ev.Estimates[0].Evidence = []contracts.Evidence{
		{Kind: "competing_fees", Prior: true, Count: 40, Summary: "40 other channels into the peer advertise 150-600 ppm (median 300)"},
	}
	// A second estimate resting on the same prior doesn't repeat it.
	second := *ev.Estimates[0]
	ev.Estimates = append(ev.Estimates, &second)
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	text := x.Text()
	for _, want := range []string{
		"Failed for lack of balance: 5 forwards, 1,250,000 sats attempted (recorded for 48 of 720 hours; attempts, not payments).",
		"Prior: 40 other channels into the peer advertise 150-600 ppm (median 300) (a prior, not a price).",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	if x.Observed.Failures == nil {
		t.Error("the JSON form has no failures")
	}
	if n := strings.Count(text, "Prior: 40 other channels"); n != 1 {
		t.Errorf("the shared prior is shown %d times", n)
	}
}

func refillAbstention(ev *store.Evaluation) *store.Evaluation {
	return &store.Evaluation{Snapshot: ev.Snapshot, Abstention: &contracts.Abstention{ChanID: 111111111111111111, Action: contracts.ActionRebalance,
		Reason: contracts.NoProfitableAction, Detail: "a refill of 1,000,000 sats from 0x0x2 would cost about 605 sats",
		WouldChangeIf: []string{"our fee rises above about 700 ppm"}, RevisitAt: at.Add(time.Hour)}}
}

func TestRefillAbstentionText(t *testing.T) {
	ev := abstentionEvaluation()
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.AddRefill(refillAbstention(ev)); err != nil {
		t.Fatal(err)
	}
	text := x.Text()
	for _, want := range []string{
		"Refill      Nothing (no profitable action): a refill of 1,000,000 sats from 0x0x2 would cost about 605 sats.",
		"            Next: evaluated again 2026-10-26 01:00 UTC.",
		"            Would refill if our fee rises above about 700 ppm.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	if x.Refill == nil || x.Refill.Next.RevisitAt == nil || x.Refill.Decided != nil {
		t.Errorf("refill = %+v", x.Refill)
	}
}

func TestRefillIntentText(t *testing.T) {
	ev := abstentionEvaluation()
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	src, max := contracts.ChanID(800000<<40|2<<16), contracts.MilliSat(800_000)
	refill := &store.Evaluation{Snapshot: ev.Snapshot,
		Intent: &contracts.Intent{ID: "r1", Action: contracts.ActionRebalance, Target: contracts.Target{ChanID: 111111111111111111, SourceChanID: &src},
			Parameters: contracts.IntentParameters{Rebalance: &contracts.RebalanceParameters{AmountMsat: 1_000_000_000}},
			Limits:     contracts.IntentLimits{MaxFeeMsat: &max}, Rationale: contracts.Rationale{Summary: "Refill: worth it."},
			ExpiresAt: at.Add(time.Hour)},
		Decision: &contracts.Decision{Verdict: contracts.Rejected, Mode: contracts.ModeShadow, Rules: []contracts.RuleResult{
			{Rule: "mode", Passed: true, Detail: "the rebalance engine is shadow"},
			{Rule: "rebalance_budget", Passed: false, Detail: "700 sats reserved by refills approved in the last 24 hours, plus this one's 800, against 1000 a day"}}},
		Outcomes: []*contracts.Outcome{{State: contracts.NotExecuted, Detail: "rejected: rebalance_budget"}}}
	if err := x.AddRefill(refill); err != nil {
		t.Fatal(err)
	}
	text := x.Text()
	for _, want := range []string{
		"Refill      Move 1,000,000 sats in from 800000x2x0, paying at most 800 sats, by 2026-10-26 01:00 UTC.",
		"            Refill: worth it.",
		"            Decided: Rejected in shadow mode: 1 of 2 rules passed.",
		"            FAIL  rebalance_budget: 700 sats reserved",
		"            Next: rejected: rebalance_budget; evaluated again in the next cycle.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	if x.Refill.Next.Outcome != contracts.NotExecuted {
		t.Errorf("refill next = %+v", x.Refill.Next)
	}
}

// A refill evaluation from another cycle or of another channel isn't added.
func TestAddRefillChecksItsCycleAndChannel(t *testing.T) {
	ev := abstentionEvaluation()
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	other := refillAbstention(ev)
	other.Abstention.ChanID = 2
	if err := x.AddRefill(other); err == nil {
		t.Error("another channel's refill was added")
	}
	later := refillAbstention(ev)
	s := *ev.Snapshot
	s.ID = "later"
	later.Snapshot = &s
	if err := x.AddRefill(later); err == nil {
		t.Error("another cycle's refill was added")
	}
	if x.Refill != nil {
		t.Errorf("refill = %+v", x.Refill)
	}
}

// An evaluation of a peer's channels together shows them as one, each channel
// under it, from the same snapshot.
func TestAPeersChannelsAreExplainedTogether(t *testing.T) {
	ev := abstentionEvaluation()
	second := ev.Snapshot.Channels[0]
	second.ChanID, second.LocalMsat = 222222222222222222, 500_000_000
	second.OurPolicy, second.PeerPolicy = &contracts.Policy{FeePPM: 500}, &contracts.Policy{FeePPM: 900}
	ev.Snapshot.Channels = append(ev.Snapshot.Channels, second)
	ev.Snapshot.Seal()
	ev.Abstention.ChanIDs = []contracts.ChanID{111111111111111111, 222222222222222222}
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	if x.Observed.CapacityMsat != 10_000_000_000 || x.Observed.LocalMsat != 5_000_000_000 || len(x.Observed.Channels) != 2 ||
		x.Observed.PeerPolicy.FeePPM != 400 {
		t.Errorf("observed = %+v", x.Observed)
	}
	text := x.Text()
	for _, want := range []string{
		"Channels 111111111111111111 and 222222222222222222 with 03bbbbbbbbbbbbbb…, decided together",
		"Local 5,000,000 sats of 10,000,000 (50%).",
		"202109x15665344x58254: local 500,000 sats of 5,000,000 (10%), our fee base 0 msat, 500 ppm.",
		"Our fee: base 0 msat, 500 ppm, the highest, which LND senders plan with. Peer's fee to us: base 0 msat, 400 ppm, the lowest.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	ev.Abstention.ChanIDs[1] = 333
	if _, err := Build(ev); err == nil {
		t.Error("a channel missing from the snapshot was explained")
	}
}

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

const peer = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func mp(m contracts.MilliSat) *contracts.MilliSat { return &m }

func snapshot() *contracts.Snapshot {
	s := &contracts.Snapshot{ID: "01a17581-b830-7c3b-98d2-e6a4b5c6d7e8", Schema: contracts.SchemaSnapshot, NodePubkey: "02aa", TakenAt: at,
		Channels: []contracts.ChannelObservation{{
			ChanID: 111111111111111111, Peer: peer, Active: true, CapacityMsat: 5_000_000_000, LocalMsat: 4_500_000_000,
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
	x, err := Build(ev)
	if err != nil {
		t.Fatal(err)
	}
	text := x.Text()
	for _, want := range []string{
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
}

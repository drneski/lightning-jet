package limits

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

type history []time.Time

func (h history) ApprovedFeeChanges(_ context.Context, _ contracts.ChanID, _ contracts.Mode, since time.Time) ([]time.Time, error) {
	var out []time.Time
	for _, t := range h {
		if !t.Before(since) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (history) ApprovedRefills(context.Context, contracts.Mode, time.Time) ([]*contracts.Intent, error) {
	return nil, nil
}

// refills is a history of approved refills, and no fee changes.
type refills []*contracts.Intent

func (refills) ApprovedFeeChanges(context.Context, contracts.ChanID, contracts.Mode, time.Time) ([]time.Time, error) {
	return nil, nil
}

func (r refills) ApprovedRefills(_ context.Context, _ contracts.Mode, since time.Time) ([]*contracts.Intent, error) {
	var out []*contracts.Intent
	for _, in := range r {
		if !in.CreatedAt.Before(since) {
			out = append(out, in)
		}
	}
	return out, nil
}

func authorizer(h history) *Authorizer {
	return &Authorizer{Config: DefaultConfig, History: h, Now: func() time.Time { return now }, NewID: func() string { return "d1" }}
}

func intent(observed, proposed int64) *contracts.Intent {
	return &contracts.Intent{ID: "i1", Schema: contracts.SchemaIntent, Action: contracts.ActionFeeUpdate, Target: contracts.Target{ChanID: 7},
		Parameters: contracts.IntentParameters{Fee: &contracts.FeeParameters{FeePPM: proposed, ObservedFeePPM: observed}},
		SnapshotID: "s1", Mode: contracts.ModeShadow, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
}

func authorize(t *testing.T, a *Authorizer, i *contracts.Intent) *contracts.Decision {
	t.Helper()
	d, err := a.Authorize(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApproved(t *testing.T) {
	d := authorize(t, authorizer(nil), intent(500, 450))
	if d.Verdict != contracts.Approved || d.Reason != "" || len(d.Rules) != 7 {
		t.Fatalf("decision = %+v", d)
	}
	if d.Authorizer != AuthorizerStandalone || d.Mode != contracts.ModeShadow || d.ReservationMsat != 0 || !strings.HasPrefix(d.PolicyVersion, "sha256:") {
		t.Errorf("decision = %+v", d)
	}
}

func TestEachRuleCanReject(t *testing.T) {
	cases := map[string]func(*Authorizer, *contracts.Intent){
		"mode":           func(a *Authorizer, i *contracts.Intent) { i.Mode = contracts.ModeLive },
		"expiry":         func(a *Authorizer, i *contracts.Intent) { i.ExpiresAt = now },
		"freeze":         func(a *Authorizer, i *contracts.Intent) { a.Freeze = []contracts.ChanID{7} },
		"fee_ppm_bounds": func(a *Authorizer, i *contracts.Intent) { a.FeePPMMax = 400 },
		"fee_step":       func(a *Authorizer, i *contracts.Intent) { i.Parameters.Fee.FeePPM = 400 },
		"cooldown":       func(a *Authorizer, i *contracts.Intent) { a.History = history{now.Add(-3 * time.Hour)} },
		"daily_changes": func(a *Authorizer, i *contracts.Intent) {
			a.CooldownHours, a.History = 1, history{now.Add(-2 * time.Hour), now.Add(-20 * time.Hour)}
		},
	}
	for rule, spoil := range cases {
		t.Run(rule, func(t *testing.T) {
			a, i := authorizer(nil), intent(500, 450)
			spoil(a, i)
			d := authorize(t, a, i)
			if d.Verdict != contracts.Rejected || !strings.Contains(d.Reason, rule+":") {
				t.Errorf("verdict %s, reason %q", d.Verdict, d.Reason)
			}
			for _, r := range d.Rules {
				if (r.Rule == rule) == r.Passed {
					t.Errorf("rule %s passed=%v", r.Rule, r.Passed)
				}
			}
		})
	}
}

func TestModeOffRejectsEverything(t *testing.T) {
	a := authorizer(nil)
	a.FeesMode = contracts.ModeOff
	if d := authorize(t, a, intent(500, 450)); d.Verdict != contracts.Rejected {
		t.Errorf("fees off, verdict %s", d.Verdict)
	}
}

func TestStepFromZeroAllowsOnePPM(t *testing.T) {
	a := authorizer(nil)
	a.FeePPMMin = 0
	if d := authorize(t, a, intent(0, 1)); d.Verdict != contracts.Approved {
		t.Errorf("0 to 1 ppm: %s, %s", d.Verdict, d.Reason)
	}
	if d := authorize(t, a, intent(0, 2)); d.Verdict != contracts.Rejected {
		t.Error("0 to 2 ppm was approved")
	}
}

func TestVersionTracksTheConfig(t *testing.T) {
	a, b := DefaultConfig, DefaultConfig
	if a.Version() != b.Version() {
		t.Error("equal configs have different versions")
	}
	b.MaxStepPct = 20
	if a.Version() == b.Version() {
		t.Error("a changed config kept its version")
	}
	a.Freeze, b = []contracts.ChanID{2, 1}, DefaultConfig
	b.Freeze = []contracts.ChanID{1, 2}
	if a.Version() != b.Version() {
		t.Error("freeze order changed the version")
	}
}

func TestUnknownActionIsAnError(t *testing.T) {
	i := intent(500, 450)
	i.Action = contracts.ActionRebalance
	if _, err := authorizer(nil).Authorize(context.Background(), i); err == nil {
		t.Error("a rebalance was authorized without rules for it")
	}
}

func refill(target contracts.ChanID, amountSats, maxFeeSats int64, at time.Time) *contracts.Intent {
	src, fee := contracts.ChanID(9), contracts.MilliSat(maxFeeSats*1000)
	return &contracts.Intent{ID: "r1", Schema: contracts.SchemaIntent, Action: contracts.ActionRebalance,
		Target:     contracts.Target{ChanID: target, SourceChanID: &src},
		Parameters: contracts.IntentParameters{Rebalance: &contracts.RebalanceParameters{AmountMsat: contracts.MilliSat(amountSats * 1000)}},
		Limits:     contracts.IntentLimits{MaxFeeMsat: &fee}, SnapshotID: "s1", Mode: contracts.ModeShadow, CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
}

func failedRules(d *contracts.Decision) []string {
	var out []string
	for _, r := range d.Rules {
		if !r.Passed {
			out = append(out, r.Rule)
		}
	}
	return out
}

func TestRefillRules(t *testing.T) {
	a := &Authorizer{Config: DefaultConfig, Now: func() time.Time { return now }, NewID: func() string { return "d1" }}

	// 500 sats on 1M sats is 500 ppm, within 1,000; nothing else approved.
	a.History = refills{}
	if d := authorize(t, a, refill(7, 1_000_000, 500, now)); d.Verdict != contracts.Approved || len(d.Rules) != 6 {
		t.Errorf("refill: %s, failed %v", d.Verdict, failedRules(d))
	}

	for name, tc := range map[string]struct {
		history refills
		intent  *contracts.Intent
		config  func(c *Config)
		want    string
	}{
		"over the ppm limit": {intent: refill(7, 100_000, 150, now), want: "rebalance_ppm"},
		// 700 sats reserved today, by a refill of another channel, plus 500.
		"over the daily budget": {history: refills{refill(8, 1_000_000, 700, now.Add(-time.Hour))}, intent: refill(7, 1_000_000, 500, now),
			want: "rebalance_budget"},
		"in the cooldown": {history: refills{refill(7, 100_000, 10, now.Add(-11*time.Hour))}, intent: refill(7, 1_000_000, 500, now),
			want: "rebalance_cooldown"},
		"frozen source": {intent: refill(7, 1_000_000, 500, now), config: func(c *Config) { c.Freeze = []contracts.ChanID{9} }, want: "freeze"},
		"frozen sibling": {intent: func() *contracts.Intent {
			i := refill(7, 1_000_000, 500, now)
			i.Target.ChanIDs = []contracts.ChanID{7, 8}
			return i
		}(),
			config: func(c *Config) { c.Freeze = []contracts.ChanID{8} }, want: "freeze"},
		"engine off": {intent: refill(7, 1_000_000, 500, now), config: func(c *Config) { c.RebalanceMode = contracts.ModeOff }, want: "mode"},
		"expired":    {intent: refill(7, 1_000_000, 500, now.Add(-2*time.Hour)), want: "expiry"},
	} {
		b := *a
		b.History = tc.history
		if tc.config != nil {
			tc.config(&b.Config)
		}
		d := authorize(t, &b, tc.intent)
		if failed := failedRules(d); d.Verdict != contracts.Rejected || len(failed) != 1 || failed[0] != tc.want {
			t.Errorf("%s: %s, failed %v", name, d.Verdict, failed)
		}
	}

	// A refill approved more than a day and a cooldown ago no longer counts.
	b := *a
	b.History = refills{refill(7, 1_000_000, 900, now.Add(-25*time.Hour))}
	if d := authorize(t, &b, refill(7, 1_000_000, 500, now)); d.Verdict != contracts.Approved {
		t.Errorf("after a day: failed %v", failedRules(d))
	}
}

// Freezing any of a peer's channels freezes the fee of all of them.
func TestFreezingOneOfAPeersChannelsFreezesItsFee(t *testing.T) {
	a := authorizer(history{})
	a.Freeze = []contracts.ChanID{8}
	i := intent(1000, 1100)
	if d := authorize(t, a, i); d.Verdict != contracts.Approved {
		t.Fatalf("a channel of another peer was frozen: %v", failedRules(d))
	}
	i.Target.ChanIDs = []contracts.ChanID{7, 8}
	d := authorize(t, a, i)
	if failed := failedRules(d); len(failed) != 1 || failed[0] != "freeze" {
		t.Fatalf("failed %v", failed)
	}
	for _, r := range d.Rules {
		if r.Rule == "freeze" && r.Detail != "frozen: 8" {
			t.Errorf("detail %q", r.Detail)
		}
	}
}

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

// Package limits is Jet's deterministic authorization. Every intent passes it
// before anything happens, the same way in shadow and live mode. A strategy or
// model never sets or relaxes a limit.
package limits

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Authorizer names are recorded on every decision.
const AuthorizerStandalone = "standalone"

// Config is the limits that bind fee changes.
type Config struct {
	FeesMode            contracts.Mode     `json:"fees_mode"`
	FeePPMMin           int64              `json:"fee_ppm_min"`
	FeePPMMax           int64              `json:"fee_ppm_max"`
	MaxStepPct          int64              `json:"max_step_pct"`
	CooldownHours       int64              `json:"cooldown_hours"`
	MaxFeeChangesPerDay int                `json:"max_fee_changes_per_day"`
	Freeze              []contracts.ChanID `json:"freeze"`
}

// DefaultConfig is the limits the slice runs with.
var DefaultConfig = Config{
	FeesMode: contracts.ModeShadow, FeePPMMin: 1, FeePPMMax: 3000,
	MaxStepPct: 15, CooldownHours: 12, MaxFeeChangesPerDay: 2,
}

// Version returns a hash of the configuration, recorded on every decision so
// a verdict can be traced to the exact limits it applied.
func (c Config) Version() string {
	c.Freeze = slices.Clone(c.Freeze)
	slices.Sort(c.Freeze)
	b, err := json.Marshal(c)
	if err != nil {
		panic("limits: encoding config: " + err.Error()) // a plain struct always encodes
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// History says what authorization approved before.
type History interface {
	// ApprovedFeeChanges returns when fee changes to a channel were approved
	// in a mode since a time, newest first.
	ApprovedFeeChanges(ctx context.Context, chanID contracts.ChanID, mode contracts.Mode, since time.Time) ([]time.Time, error)
}

// Authorizer judges intents.
type Authorizer struct {
	Config
	History History
	Now     func() time.Time
	NewID   func() string
}

// Authorize evaluates every rule for the intent and returns the decision.
// Each rule's result is recorded, not just the failures.
func (a *Authorizer) Authorize(ctx context.Context, intent *contracts.Intent) (*contracts.Decision, error) {
	if intent.Action != contracts.ActionFeeUpdate || intent.Parameters.Fee == nil {
		return nil, fmt.Errorf("limits: no rules for action %q", intent.Action)
	}
	now := contracts.Timestamp(a.Now())
	fee := intent.Parameters.Fee
	ch := intent.Target.ChanID

	lookback := max(time.Duration(a.CooldownHours)*time.Hour, 24*time.Hour)
	approved, err := a.History.ApprovedFeeChanges(ctx, ch, intent.Mode, now.Add(-lookback))
	if err != nil {
		return nil, fmt.Errorf("limits: reading history: %w", err)
	}

	var rules []contracts.RuleResult
	rule := func(name string, passed bool, detail string, args ...any) {
		rules = append(rules, contracts.RuleResult{Rule: name, Passed: passed, Detail: fmt.Sprintf(detail, args...)})
	}

	rule("mode", a.FeesMode != contracts.ModeOff && intent.Mode == a.FeesMode,
		"the fees engine is %s; the intent was proposed in %s", a.FeesMode, intent.Mode)
	rule("expiry", now.Before(intent.ExpiresAt), "the intent expires at %s", intent.ExpiresAt.Format(time.RFC3339))
	rule("freeze", !slices.Contains(a.Freeze, ch), "channel %s is %s", ch, map[bool]string{true: "frozen", false: "not frozen"}[slices.Contains(a.Freeze, ch)])
	rule("fee_ppm_bounds", fee.FeePPM >= a.FeePPMMin && fee.FeePPM <= a.FeePPMMax,
		"%d ppm against bounds of %d-%d ppm", fee.FeePPM, a.FeePPMMin, a.FeePPMMax)
	allowed := max(1, fee.ObservedFeePPM*a.MaxStepPct/100)
	change := fee.FeePPM - fee.ObservedFeePPM
	if change < 0 {
		change = -change
	}
	rule("fee_step", change <= allowed, "a change of %d ppm against a limit of %d ppm (%d%% of %d)", change, allowed, a.MaxStepPct, fee.ObservedFeePPM)

	cooldown := time.Duration(a.CooldownHours) * time.Hour
	if len(approved) > 0 && now.Sub(approved[0]) < cooldown {
		rule("cooldown", false, "a change was approved %s ago; the cooldown is %d hours", now.Sub(approved[0]).Round(time.Minute), a.CooldownHours)
	} else {
		rule("cooldown", true, "no change approved in the last %d hours", a.CooldownHours)
	}
	var today int
	for _, at := range approved {
		if now.Sub(at) < 24*time.Hour {
			today++
		}
	}
	rule("daily_changes", today < a.MaxFeeChangesPerDay, "%d of %d changes approved in the last 24 hours", today, a.MaxFeeChangesPerDay)

	d := &contracts.Decision{
		ID: a.NewID(), Schema: contracts.SchemaDecision, IntentID: intent.ID, SnapshotID: intent.SnapshotID,
		Verdict: contracts.Approved, Rules: rules, PolicyVersion: a.Version(), Authorizer: AuthorizerStandalone,
		Mode: intent.Mode, DecidedAt: now,
	}
	var failed []string
	for _, r := range rules {
		if !r.Passed {
			failed = append(failed, r.Rule+": "+r.Detail)
		}
	}
	if len(failed) > 0 {
		d.Verdict, d.Reason = contracts.Rejected, strings.Join(failed, "; ")
	}
	if err := d.Validate(); err != nil {
		return nil, errors.Join(errors.New("limits: built an invalid decision"), err)
	}
	return d, nil
}

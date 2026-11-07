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

// Config is the limits that bind fee changes and refills.
type Config struct {
	FeesMode            contracts.Mode     `json:"fees_mode"`
	FeePPMMin           int64              `json:"fee_ppm_min"`
	FeePPMMax           int64              `json:"fee_ppm_max"`
	MaxStepPct          int64              `json:"max_step_pct"`
	CooldownHours       int64              `json:"cooldown_hours"`
	MaxFeeChangesPerDay int                `json:"max_fee_changes_per_day"`
	Freeze              []contracts.ChanID `json:"freeze"`
	// RebalanceMode is the refill engine's mode. A refill may pay at most
	// RebalancePPMMax of what it moves; the refills approved in any 24 hours
	// may pay at most RebalanceSatsPerDay between them, counting each at its
	// maximum fee; and a channel is refilled at most once per
	// RebalanceCooldownHours.
	RebalanceMode          contracts.Mode `json:"rebalance_mode"`
	RebalancePPMMax        int64          `json:"rebalance_ppm_max"`
	RebalanceSatsPerDay    int64          `json:"rebalance_sats_per_day"`
	RebalanceCooldownHours int64          `json:"rebalance_cooldown_hours"`
}

// DefaultConfig is the limits the slice runs with.
var DefaultConfig = Config{
	FeesMode: contracts.ModeShadow, FeePPMMin: 1, FeePPMMax: 3000,
	MaxStepPct: 15, CooldownHours: 12, MaxFeeChangesPerDay: 2,
	RebalanceMode: contracts.ModeShadow, RebalancePPMMax: 1000, RebalanceSatsPerDay: 1000, RebalanceCooldownHours: 12,
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
	// ApprovedRefills returns the refill intents approved in a mode since a
	// time, newest first.
	ApprovedRefills(ctx context.Context, mode contracts.Mode, since time.Time) ([]*contracts.Intent, error)
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
	switch {
	case intent.Action == contracts.ActionFeeUpdate && intent.Parameters.Fee != nil:
		return a.authorizeFee(ctx, intent)
	case intent.Action == contracts.ActionRebalance && intent.Parameters.Rebalance != nil &&
		intent.Target.SourceChanID != nil && intent.Limits.MaxFeeMsat != nil:
		return a.authorizeRefill(ctx, intent)
	}
	return nil, fmt.Errorf("limits: no rules for action %q", intent.Action)
}

func (a *Authorizer) authorizeFee(ctx context.Context, intent *contracts.Intent) (*contracts.Decision, error) {
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
	return a.decide(intent, now, rules)
}

func (a *Authorizer) authorizeRefill(ctx context.Context, intent *contracts.Intent) (*contracts.Decision, error) {
	now := contracts.Timestamp(a.Now())
	target, source := intent.Target.ChanID, *intent.Target.SourceChanID
	amount, maxFee := intent.Parameters.Rebalance.AmountMsat, *intent.Limits.MaxFeeMsat
	lookback := max(time.Duration(a.RebalanceCooldownHours)*time.Hour, 24*time.Hour)
	approved, err := a.History.ApprovedRefills(ctx, intent.Mode, now.Add(-lookback))
	if err != nil {
		return nil, fmt.Errorf("limits: reading history: %w", err)
	}

	var rules []contracts.RuleResult
	rule := func(name string, passed bool, detail string, args ...any) {
		rules = append(rules, contracts.RuleResult{Rule: name, Passed: passed, Detail: fmt.Sprintf(detail, args...)})
	}
	rule("mode", a.RebalanceMode != contracts.ModeOff && intent.Mode == a.RebalanceMode,
		"the rebalance engine is %s; the intent was proposed in %s", a.RebalanceMode, intent.Mode)
	rule("expiry", now.Before(intent.ExpiresAt), "the intent expires at %s", intent.ExpiresAt.Format(time.RFC3339))
	frozen := slices.Contains(a.Freeze, target) || slices.Contains(a.Freeze, source)
	rule("freeze", !frozen, "channels %s and %s are %s", target, source, map[bool]string{true: "not both unfrozen", false: "not frozen"}[frozen])
	var ppm int64
	if amount > 0 {
		ppm = int64(maxFee) * 1_000_000 / int64(amount)
	}
	rule("rebalance_ppm", ppm <= a.RebalancePPMMax, "a fee of at most %d sats on %d sats is %d ppm against a limit of %d ppm",
		maxFee.Sats(), amount.Sats(), ppm, a.RebalancePPMMax)
	var reserved contracts.MilliSat
	var last time.Time
	for _, in := range approved {
		if now.Sub(in.CreatedAt) < 24*time.Hour && in.Limits.MaxFeeMsat != nil {
			reserved += *in.Limits.MaxFeeMsat
		}
		if in.Target.ChanID == target && in.CreatedAt.After(last) {
			last = in.CreatedAt
		}
	}
	rule("rebalance_budget", (reserved+maxFee).Sats() <= a.RebalanceSatsPerDay,
		"%d sats reserved by refills approved in the last 24 hours, plus this one's %d, against %d a day",
		reserved.Sats(), maxFee.Sats(), a.RebalanceSatsPerDay)
	cooldown := time.Duration(a.RebalanceCooldownHours) * time.Hour
	if !last.IsZero() && now.Sub(last) < cooldown {
		rule("rebalance_cooldown", false, "a refill of this channel was approved %s ago; the cooldown is %d hours",
			now.Sub(last).Round(time.Minute), a.RebalanceCooldownHours)
	} else {
		rule("rebalance_cooldown", true, "no refill of this channel approved in the last %d hours", a.RebalanceCooldownHours)
	}
	return a.decide(intent, now, rules)
}

// decide turns the rule results into a decision: approved if every rule
// passed.
func (a *Authorizer) decide(intent *contracts.Intent, now time.Time, rules []contracts.RuleResult) (*contracts.Decision, error) {
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

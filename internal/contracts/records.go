package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Schemas name each record's type and major version.
const (
	SchemaSnapshot   = "jet.snapshot/1"
	SchemaEstimate   = "jet.estimate/1"
	SchemaIntent     = "jet.intent/1"
	SchemaAbstention = "jet.abstention/1"
	SchemaDecision   = "jet.decision/1"
	SchemaOutcome    = "jet.outcome/1"
)

// Snapshot is the inputs one evaluation saw, frozen so the decision can be
// reconstructed. Everything in it is observed, not derived.
type Snapshot struct {
	ID            string               `json:"id"`
	Schema        string               `json:"schema"`
	NodePubkey    string               `json:"node_pubkey"`
	BlockHeight   uint32               `json:"block_height"`
	TakenAt       time.Time            `json:"taken_at"`
	Sources       []Source             `json:"sources"`
	Channels      []ChannelObservation `json:"channels"`
	ContentSHA256 string               `json:"content_sha256"`
}

// Source is one input and when it was read.
type Source struct {
	Name   string    `json:"name"`
	ReadAt time.Time `json:"read_at"`
}

// ChannelObservation is what a snapshot saw of one channel.
type ChannelObservation struct {
	ChanID       ChanID      `json:"chan_id"`
	Peer         string      `json:"peer"`
	Active       bool        `json:"active"`
	CapacityMsat MilliSat    `json:"capacity_msat"`
	LocalMsat    MilliSat    `json:"local_msat"`
	RemoteMsat   MilliSat    `json:"remote_msat"`
	OurPolicy    *Policy     `json:"our_policy,omitempty"`
	PeerPolicy   *Policy     `json:"peer_policy,omitempty"`
	Forwards     Forwards    `json:"forwards"`
	DataQuality  DataQuality `json:"data_quality"`
}

// Policy is one side's routing policy for a channel.
type Policy struct {
	BaseMsat      MilliSat  `json:"base_msat"`
	FeePPM        int64     `json:"fee_ppm"`
	TimeLockDelta uint32    `json:"time_lock_delta"`
	MinHTLCMsat   MilliSat  `json:"min_htlc_msat"`
	MaxHTLCMsat   MilliSat  `json:"max_htlc_msat"`
	Disabled      bool      `json:"disabled"`
	LastUpdate    time.Time `json:"last_update"`
}

// Forwards summarizes a channel's completed forwards over a window.
type Forwards struct {
	WindowHours   int64    `json:"window_hours"`
	OutCount      int      `json:"out_count"`
	OutAmountMsat MilliSat `json:"out_amount_msat"`
	OutFeeMsat    MilliSat `json:"out_fee_msat"`
	InCount       int      `json:"in_count"`
	InAmountMsat  MilliSat `json:"in_amount_msat"`
}

// Channel returns the snapshot's observation of a channel.
func (s *Snapshot) Channel(id ChanID) (ChannelObservation, bool) {
	for _, c := range s.Channels {
		if c.ChanID == id {
			return c, true
		}
	}
	return ChannelObservation{}, false
}

// Hash returns the SHA-256 of the snapshot's JSON encoding with
// content_sha256 left empty.
func (s *Snapshot) Hash() (string, error) {
	c := *s
	c.ContentSHA256 = ""
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Seal sets content_sha256, after which the snapshot must not change.
func (s *Snapshot) Seal() error {
	h, err := s.Hash()
	if err != nil {
		return err
	}
	s.ContentSHA256 = h
	return nil
}

// Estimate is what an estimator believes about one channel, with how much it
// should be believed. What the value fields mean depends on Kind.
type Estimate struct {
	ID                 string             `json:"id"`
	Schema             string             `json:"schema"`
	ChanID             ChanID             `json:"chan_id"`
	Kind               string             `json:"kind"`
	SnapshotID         string             `json:"snapshot_id"`
	Estimator          Component          `json:"estimator"`
	HorizonHours       int64              `json:"horizon_hours"`
	ExpectedMsat       *MilliSat          `json:"expected_msat,omitempty"`
	LowMsat            *MilliSat          `json:"low_msat,omitempty"`
	HighMsat           *MilliSat          `json:"high_msat,omitempty"`
	Interval           string             `json:"interval,omitempty"`
	EconomicConfidence EconomicConfidence `json:"economic_confidence"`
	DataQuality        DataQuality        `json:"data_quality"`
	Evidence           []Evidence         `json:"evidence"`
	Assumptions        []string           `json:"assumptions"`
	ComputedAt         time.Time          `json:"computed_at"`
}

// Evidence is one observation behind an estimate. A prior is labeled as one.
type Evidence struct {
	Kind        string `json:"kind"`
	WindowHours int64  `json:"window_hours"`
	Count       int    `json:"count"`
	Source      string `json:"source"`
	Prior       bool   `json:"prior,omitempty"`
	Summary     string `json:"summary"`
}

// Action is what an intent proposes to do.
type Action string

const (
	ActionFeeUpdate Action = "fee_update"
	ActionRebalance Action = "rebalance"
)

// Intent is a proposed action, specific enough to authorize, execute and
// audit. It is immutable.
type Intent struct {
	ID         string           `json:"id"`
	Schema     string           `json:"schema"`
	Action     Action           `json:"action"`
	Target     Target           `json:"target"`
	Parameters IntentParameters `json:"parameters"`
	Limits     IntentLimits     `json:"limits"`
	Rationale  Rationale        `json:"rationale"`
	Strategy   Component        `json:"strategy"`
	SnapshotID string           `json:"snapshot_id"`
	Mode       Mode             `json:"mode"`
	CreatedAt  time.Time        `json:"created_at"`
	ExpiresAt  time.Time        `json:"expires_at"`
}

// Target is the channel an intent acts on, and for a rebalance its source.
type Target struct {
	ChanID       ChanID  `json:"chan_id"`
	SourceChanID *ChanID `json:"source_chan_id,omitempty"`
}

// IntentParameters holds one action's parameters.
type IntentParameters struct {
	Fee *FeeParameters `json:"fee,omitempty"`
}

// FeeParameters is a new policy and the observed one it replaces.
type FeeParameters struct {
	BaseMsat         MilliSat `json:"base_msat"`
	FeePPM           int64    `json:"fee_ppm"`
	ObservedBaseMsat MilliSat `json:"observed_base_msat"`
	ObservedFeePPM   int64    `json:"observed_fee_ppm"`
}

// IntentLimits are the bounds execution must stay inside. A fee update has
// none beyond its parameters.
type IntentLimits struct {
	MaxFeeMsat *MilliSat `json:"max_fee_msat,omitempty"`
}

// Rationale is what an intent rests on.
type Rationale struct {
	EstimateIDs []string `json:"estimate_ids"`
	Summary     string   `json:"summary"`
}

// AbstentionReason says why a strategy did nothing.
type AbstentionReason string

const (
	NoProfitableAction   AbstentionReason = "no_profitable_action"
	InsufficientEvidence AbstentionReason = "insufficient_evidence"
	InsufficientData     AbstentionReason = "insufficient_data"
)

// Abstention is a recorded decision to do nothing, with the reason.
type Abstention struct {
	ID            string           `json:"id"`
	Schema        string           `json:"schema"`
	ChanID        ChanID           `json:"chan_id"`
	Strategy      Component        `json:"strategy"`
	SnapshotID    string           `json:"snapshot_id"`
	EstimateIDs   []string         `json:"estimate_ids"`
	Reason        AbstentionReason `json:"reason"`
	Detail        string           `json:"detail"`
	WouldChangeIf []string         `json:"would_change_if"`
	RevisitAt     time.Time        `json:"revisit_at"`
	CreatedAt     time.Time        `json:"created_at"`
}

// Verdict is authorization's answer to an intent.
type Verdict string

const (
	Approved Verdict = "approved"
	Rejected Verdict = "rejected"
)

// RuleResult is one authorization rule's result.
type RuleResult struct {
	Rule   string `json:"rule"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Decision is authorization's verdict on one intent. It is immutable; what
// happens next is recorded as outcomes.
type Decision struct {
	ID              string       `json:"id"`
	Schema          string       `json:"schema"`
	IntentID        string       `json:"intent_id"`
	SnapshotID      string       `json:"snapshot_id"`
	Verdict         Verdict      `json:"verdict"`
	Reason          string       `json:"reason,omitempty"`
	Rules           []RuleResult `json:"rules"`
	PolicyVersion   string       `json:"policy_version"`
	Authorizer      string       `json:"authorizer"`
	Mode            Mode         `json:"mode"`
	ReservationMsat MilliSat     `json:"reservation_msat"`
	DecidedAt       time.Time    `json:"decided_at"`
}

// OutcomeState is an intent's state after authorization.
type OutcomeState string

const (
	NotExecuted OutcomeState = "not_executed"
	Pending     OutcomeState = "pending"
	Succeeded   OutcomeState = "succeeded"
	Failed      OutcomeState = "failed"
	Expired     OutcomeState = "expired"
	Canceled    OutcomeState = "canceled"
)

// Outcome is appended to an intent each time its state changes.
type Outcome struct {
	ID       string       `json:"id"`
	Schema   string       `json:"schema"`
	IntentID string       `json:"intent_id"`
	State    OutcomeState `json:"state"`
	Detail   string       `json:"detail"`
	At       time.Time    `json:"at"`
}

// Validate checks the rules every snapshot must satisfy.
func (s *Snapshot) Validate() error {
	var errs []error
	req(&errs, s.ID != "", "snapshot: missing id")
	req(&errs, s.Schema == SchemaSnapshot, "snapshot: schema must be "+SchemaSnapshot)
	req(&errs, s.NodePubkey != "", "snapshot: missing node pubkey")
	req(&errs, !s.TakenAt.IsZero(), "snapshot: missing taken_at")
	if s.ContentSHA256 == "" {
		errs = append(errs, errors.New("snapshot: not sealed"))
	} else if h, err := s.Hash(); err != nil || h != s.ContentSHA256 {
		errs = append(errs, errors.New("snapshot: content does not match content_sha256"))
	}
	for _, c := range s.Channels {
		req(&errs, c.DataQuality.Level != "", fmt.Sprintf("snapshot: channel %s has no data quality", c.ChanID))
	}
	return errors.Join(errs...)
}

// Validate checks the rules every estimate must satisfy. At economic
// confidence none there is no forecast, so the value fields must be absent.
func (e *Estimate) Validate() error {
	var errs []error
	req(&errs, e.ID != "", "estimate: missing id")
	req(&errs, e.Schema == SchemaEstimate, "estimate: schema must be "+SchemaEstimate)
	req(&errs, e.Kind != "", "estimate: missing kind")
	req(&errs, e.SnapshotID != "", "estimate: missing snapshot id")
	req(&errs, e.Estimator.Name != "" && e.Estimator.Version != "", "estimate: missing estimator name or version")
	req(&errs, e.HorizonHours > 0, "estimate: horizon must be positive")
	req(&errs, e.EconomicConfidence.Level != "", "estimate: missing economic confidence")
	req(&errs, e.DataQuality.Level != "", "estimate: missing data quality")
	hasValue := e.ExpectedMsat != nil || e.LowMsat != nil || e.HighMsat != nil
	if e.EconomicConfidence.Level == ConfidenceNone {
		req(&errs, !hasValue, "estimate: confidence none must not state a value")
	} else {
		req(&errs, e.ExpectedMsat != nil && e.LowMsat != nil && e.HighMsat != nil && e.Interval != "",
			"estimate: a forecast needs expected, low, high and interval")
		if e.ExpectedMsat != nil && e.LowMsat != nil && e.HighMsat != nil {
			req(&errs, *e.LowMsat <= *e.ExpectedMsat && *e.ExpectedMsat <= *e.HighMsat, "estimate: expected must lie within low and high")
		}
	}
	return errors.Join(errs...)
}

// Validate checks the rules every intent must satisfy.
func (i *Intent) Validate() error {
	var errs []error
	req(&errs, i.ID != "", "intent: missing id")
	req(&errs, i.Schema == SchemaIntent, "intent: schema must be "+SchemaIntent)
	req(&errs, i.SnapshotID != "", "intent: missing snapshot id")
	req(&errs, i.Strategy.Name != "" && i.Strategy.Version != "", "intent: missing strategy name or version")
	req(&errs, len(i.Rationale.EstimateIDs) > 0 && i.Rationale.Summary != "", "intent: rationale needs estimates and a summary")
	req(&errs, i.Mode == ModeShadow || i.Mode == ModeLive, "intent: mode must be shadow or live")
	req(&errs, i.ExpiresAt.After(i.CreatedAt), "intent: must expire after it is created")
	switch i.Action {
	case ActionFeeUpdate:
		req(&errs, i.Parameters.Fee != nil, "intent: fee_update needs fee parameters")
		if f := i.Parameters.Fee; f != nil {
			req(&errs, f.FeePPM >= 0 && f.BaseMsat >= 0, "intent: fee parameters must not be negative")
		}
	default:
		errs = append(errs, fmt.Errorf("intent: unsupported action %q", i.Action))
	}
	return errors.Join(errs...)
}

// Validate checks the rules every abstention must satisfy.
func (a *Abstention) Validate() error {
	var errs []error
	req(&errs, a.ID != "", "abstention: missing id")
	req(&errs, a.Schema == SchemaAbstention, "abstention: schema must be "+SchemaAbstention)
	req(&errs, a.SnapshotID != "", "abstention: missing snapshot id")
	req(&errs, a.Strategy.Name != "" && a.Strategy.Version != "", "abstention: missing strategy name or version")
	switch a.Reason {
	case NoProfitableAction, InsufficientEvidence, InsufficientData:
	default:
		errs = append(errs, fmt.Errorf("abstention: unknown reason %q", a.Reason))
	}
	req(&errs, a.Detail != "", "abstention: missing detail")
	req(&errs, len(a.WouldChangeIf) > 0, "abstention: must say what would change the decision")
	req(&errs, a.RevisitAt.After(a.CreatedAt), "abstention: revisit must come after it is created")
	return errors.Join(errs...)
}

// Validate checks the rules every decision must satisfy.
func (d *Decision) Validate() error {
	var errs []error
	req(&errs, d.ID != "", "decision: missing id")
	req(&errs, d.Schema == SchemaDecision, "decision: schema must be "+SchemaDecision)
	req(&errs, d.IntentID != "", "decision: missing intent id")
	req(&errs, d.Verdict == Approved || d.Verdict == Rejected, "decision: verdict must be approved or rejected")
	req(&errs, len(d.Rules) > 0, "decision: must list the rules evaluated")
	req(&errs, d.PolicyVersion != "", "decision: missing policy version")
	if d.Verdict == Rejected {
		req(&errs, d.Reason != "", "decision: a rejection needs a reason")
	}
	allPassed := true
	for _, r := range d.Rules {
		allPassed = allPassed && r.Passed
	}
	req(&errs, (d.Verdict == Approved) == allPassed, "decision: verdict must match the rules")
	return errors.Join(errs...)
}

// Validate checks the rules every outcome must satisfy.
func (o *Outcome) Validate() error {
	var errs []error
	req(&errs, o.ID != "", "outcome: missing id")
	req(&errs, o.Schema == SchemaOutcome, "outcome: schema must be "+SchemaOutcome)
	req(&errs, o.IntentID != "", "outcome: missing intent id")
	switch o.State {
	case NotExecuted, Pending, Succeeded, Failed, Expired, Canceled:
	default:
		errs = append(errs, fmt.Errorf("outcome: unknown state %q", o.State))
	}
	return errors.Join(errs...)
}

func req(errs *[]error, ok bool, msg string) {
	if !ok {
		*errs = append(*errs, errors.New(msg))
	}
}

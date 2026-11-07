// Package explain tells the operator why Jet did or didn't act on a channel.
// It reads recorded data only and never recomputes, so it explains what
// actually happened, not what the current code would do now.
package explain

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

// Explanation is the stable, structured form of an explanation: what Jet
// observed, estimated, proposed and decided, and what happens next. Its JSON
// is the contract; the text form is presentation.
type Explanation struct {
	ChanID    contracts.ChanID      `json:"chan_id"`
	Observed  Observed              `json:"observed"`
	Estimated []*contracts.Estimate `json:"estimated"`
	Proposed  Proposed              `json:"proposed"`
	Decided   *Decided              `json:"decided,omitempty"`
	Next      Next                  `json:"next"`
	// Refill is the same cycle's refill evaluation of the channel, when it
	// had one.
	Refill *Refill `json:"refill,omitempty"`
}

// Refill is a refill evaluation: what the refill strategy proposed, what
// authorization decided, and what happens next.
type Refill struct {
	Proposed Proposed `json:"proposed"`
	Decided  *Decided `json:"decided,omitempty"`
	Next     Next     `json:"next"`
}

// Observed is the channel as the snapshot saw it.
type Observed struct {
	SnapshotID    string                 `json:"snapshot_id"`
	ContentSHA256 string                 `json:"content_sha256"`
	At            time.Time              `json:"at"`
	Peer          string                 `json:"peer"`
	Active        bool                   `json:"active"`
	CapacityMsat  contracts.MilliSat     `json:"capacity_msat"`
	LocalMsat     contracts.MilliSat     `json:"local_msat"`
	OurPolicy     *contracts.Policy      `json:"our_policy,omitempty"`
	PeerPolicy    *contracts.Policy      `json:"peer_policy,omitempty"`
	Forwards      contracts.Forwards     `json:"forwards"`
	Failures      *contracts.Failures    `json:"failures,omitempty"`
	Competition   *contracts.Competition `json:"competition,omitempty"`
	DataQuality   contracts.DataQuality  `json:"data_quality"`
}

// Proposed is the strategy's result: exactly one of an intent or an
// abstention.
type Proposed struct {
	Intent     *contracts.Intent     `json:"intent,omitempty"`
	Abstention *contracts.Abstention `json:"abstention,omitempty"`
}

// Decided is authorization's verdict on an intent and what followed.
type Decided struct {
	Decision *contracts.Decision  `json:"decision"`
	Outcomes []*contracts.Outcome `json:"outcomes"`
}

// Next is what happens now, and what would change the decision.
type Next struct {
	Outcome       contracts.OutcomeState `json:"outcome,omitempty"`
	RevisitAt     *time.Time             `json:"revisit_at,omitempty"`
	WouldChangeIf []string               `json:"would_change_if,omitempty"`
	Note          string                 `json:"note"`
}

// Build assembles the explanation of an evaluation from its records.
func Build(ev *store.Evaluation) (*Explanation, error) {
	var chanID contracts.ChanID
	switch {
	case ev.Intent != nil:
		chanID = ev.Intent.Target.ChanID
	case ev.Abstention != nil:
		chanID = ev.Abstention.ChanID
	default:
		return nil, fmt.Errorf("explain: the evaluation has neither an intent nor an abstention")
	}
	ch, ok := ev.Snapshot.Channel(chanID)
	if !ok {
		return nil, fmt.Errorf("explain: snapshot %s has no channel %s", ev.Snapshot.ID, chanID)
	}
	x := &Explanation{
		ChanID: chanID,
		Observed: Observed{
			SnapshotID: ev.Snapshot.ID, ContentSHA256: ev.Snapshot.ContentSHA256, At: ev.Snapshot.TakenAt,
			Peer: ch.Peer, Active: ch.Active, CapacityMsat: ch.CapacityMsat, LocalMsat: ch.LocalMsat,
			OurPolicy: ch.OurPolicy, PeerPolicy: ch.PeerPolicy, Forwards: ch.Forwards,
			Failures: ch.Failures, Competition: ch.Competition, DataQuality: ch.DataQuality,
		},
		Estimated: ev.Estimates,
	}
	x.Proposed, x.Decided, x.Next = parts(ev)
	return x, nil
}

// AddRefill adds the channel's refill evaluation from the same cycle.
func (x *Explanation) AddRefill(ev *store.Evaluation) error {
	switch {
	case ev.Snapshot == nil || ev.Snapshot.ID != x.Observed.SnapshotID:
		return fmt.Errorf("explain: the refill evaluation is from another cycle")
	case ev.Intent != nil && ev.Intent.Target.ChanID != x.ChanID, ev.Abstention != nil && ev.Abstention.ChanID != x.ChanID:
		return fmt.Errorf("explain: the refill evaluation is of another channel")
	case ev.Intent == nil && ev.Abstention == nil:
		return fmt.Errorf("explain: the refill evaluation has neither an intent nor an abstention")
	}
	r := &Refill{}
	r.Proposed, r.Decided, r.Next = parts(ev)
	x.Refill = r
	return nil
}

// parts are what an evaluation proposed, what was decided, and what's next.
func parts(ev *store.Evaluation) (Proposed, *Decided, Next) {
	p := Proposed{Intent: ev.Intent, Abstention: ev.Abstention}
	if a := ev.Abstention; a != nil {
		revisit := a.RevisitAt
		return p, nil, Next{RevisitAt: &revisit, WouldChangeIf: a.WouldChangeIf, Note: "nothing to execute"}
	}
	next := Next{Note: "evaluated again in the next cycle"}
	if n := len(ev.Outcomes); n > 0 {
		next.Outcome = ev.Outcomes[n-1].State
		next.Note = ev.Outcomes[n-1].Detail + "; evaluated again in the next cycle"
	}
	return p, &Decided{Decision: ev.Decision, Outcomes: ev.Outcomes}, next
}

// Text renders the explanation for a terminal.
func (x *Explanation) Text() string {
	var b strings.Builder
	line := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "%-12s%s\n", label, fmt.Sprintf(format, args...))
	}
	more := func(format string, args ...any) { line("", format, args...) }
	o := x.Observed

	fmt.Fprintf(&b, "Channel %s with %s\n\n", x.ChanID, short(o.Peer))
	line("Observed", "%s. Local %s sats of %s (%d%%).%s", o.At.Format("2006-01-02 15:04 MST"),
		commas(o.LocalMsat.Sats()), commas(o.CapacityMsat.Sats()), pct(o.LocalMsat, o.CapacityMsat), inactive(o.Active))
	more("Our fee: %s. Peer's fee to us: %s.", policy(o.OurPolicy), policy(o.PeerPolicy))
	f := o.Forwards
	more("%d days: %d forwards out, earning %s sats; %d in.", f.WindowHours/24, f.OutCount, commas(f.OutFeeMsat.Sats()), f.InCount)
	if fl := o.Failures; fl != nil {
		more("Failed for lack of balance: %d forwards, %s sats attempted (recorded for %d of %d hours; attempts, not payments).",
			fl.BalanceFailures, commas(fl.BalanceFailuresMsat.Sats()), fl.CoverageHours, fl.WindowHours)
	}
	more("Data quality: %s.", quality(o.DataQuality))

	shown := map[string]bool{} // a prior two estimates share is shown once
	for i, e := range x.Estimated {
		label := ""
		if i == 0 {
			label = "Estimated"
		}
		name := strings.ReplaceAll(e.Kind, "_", " ")
		name = strings.ToUpper(name[:1]) + name[1:]
		switch {
		case e.ExpectedPPM != nil && e.LowPPM != nil && e.HighPPM != nil:
			line(label, "%s, %d days: %s ppm expected, %s to %s (%s).", name, e.HorizonHours/24,
				commas(*e.ExpectedPPM), commas(*e.LowPPM), commas(*e.HighPPM), e.Interval)
		case e.ExpectedMsat == nil:
			line(label, "%s, %d days: no forecast.", name, e.HorizonHours/24)
		default:
			line(label, "%s, %d days: %s sats expected, %s to %s (%s).", name, e.HorizonHours/24,
				commas(e.ExpectedMsat.Sats()), commas(e.LowMsat.Sats()), commas(e.HighMsat.Sats()), e.Interval)
		}
		more("Economic confidence: %s (%s).", e.EconomicConfidence.Level, e.EconomicConfidence.Basis)
		for _, ev := range e.Evidence {
			if ev.Prior && !shown[ev.Summary] {
				shown[ev.Summary] = true
				more("Prior: %s (a prior, not a price).", ev.Summary)
			}
		}
	}

	if a := x.Proposed.Abstention; a != nil {
		line("Proposed", "Nothing (%s): %s.", strings.ReplaceAll(string(a.Reason), "_", " "), a.Detail)
		line("Decided", "No intent, so no authorization.")
		line("Next", "Evaluated again %s.", a.RevisitAt.Format(stamp))
		for _, w := range a.WouldChangeIf {
			more("Would act if %s.", w)
		}
	} else {
		i := x.Proposed.Intent
		if fee := i.Parameters.Fee; fee != nil {
			line("Proposed", "Change the fee from %d to %d ppm, base %s msat unchanged, by %s.", fee.ObservedFeePPM, fee.FeePPM,
				commas(int64(fee.BaseMsat)), i.ExpiresAt.Format(stamp))
		}
		more("%s", i.Rationale.Summary)
		if d := x.Decided; d != nil && d.Decision != nil {
			line("Decided", "%s.", verdict(d.Decision))
			rules(more, d.Decision)
		}
		line("Next", "%s.", capitalize(x.Next.Note))
	}

	// The refill decision sits under its own label, each part named.
	r := x.Refill
	switch {
	case r == nil:
		line("Refill", "Not evaluated in this cycle: Jet evaluates refills of depleted channels only.")
	case r.Proposed.Abstention != nil:
		a := r.Proposed.Abstention
		line("Refill", "Nothing (%s): %s.", strings.ReplaceAll(string(a.Reason), "_", " "), a.Detail)
		more("Next: evaluated again %s.", a.RevisitAt.Format(stamp))
		for _, w := range a.WouldChangeIf {
			more("Would refill if %s.", w)
		}
	case r.Proposed.Intent != nil:
		i := r.Proposed.Intent
		source := "another channel"
		if i.Target.SourceChanID != nil {
			source = i.Target.SourceChanID.SCID()
		}
		var amount, maxFee contracts.MilliSat
		if i.Parameters.Rebalance != nil {
			amount = i.Parameters.Rebalance.AmountMsat
		}
		if i.Limits.MaxFeeMsat != nil {
			maxFee = *i.Limits.MaxFeeMsat
		}
		line("Refill", "Move %s sats in from %s, paying at most %s sats, by %s.", commas(amount.Sats()), source,
			commas(maxFee.Sats()), i.ExpiresAt.Format(stamp))
		more("%s", i.Rationale.Summary)
		if d := r.Decided; d != nil && d.Decision != nil {
			more("Decided: %s.", verdict(d.Decision))
			rules(more, d.Decision)
		}
		more("Next: %s.", r.Next.Note)
	}
	return b.String()
}

const stamp = "2006-01-02 15:04 MST"

// verdict says how a decision came out: its verdict, mode and rules passed.
func verdict(d *contracts.Decision) string {
	passed := 0
	for _, r := range d.Rules {
		if r.Passed {
			passed++
		}
	}
	return fmt.Sprintf("%s in %s mode: %d of %d rules passed", capitalize(string(d.Verdict)), d.Mode, passed, len(d.Rules))
}

// rules lists a decision's rules, each marked pass or FAIL.
func rules(more func(string, ...any), d *contracts.Decision) {
	for _, r := range d.Rules {
		mark := "pass"
		if !r.Passed {
			mark = "FAIL"
		}
		more("%s  %s: %s", mark, r.Rule, r.Detail)
	}
}

func policy(p *contracts.Policy) string {
	if p == nil {
		return "unknown"
	}
	return fmt.Sprintf("base %s msat, %d ppm", commas(int64(p.BaseMsat)), p.FeePPM)
}

func quality(q contracts.DataQuality) string {
	if len(q.Issues) == 0 {
		return string(q.Level)
	}
	return string(q.Level) + " (" + strings.Join(q.Issues, "; ") + ")"
}

func inactive(active bool) string {
	if active {
		return ""
	}
	return " Inactive."
}

func pct(part, whole contracts.MilliSat) int64 {
	if whole <= 0 {
		return 0
	}
	return int64(part) * 100 / int64(whole)
}

func short(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// commas formats n with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

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
		Proposed:  Proposed{Intent: ev.Intent, Abstention: ev.Abstention},
	}
	if a := ev.Abstention; a != nil {
		revisit := a.RevisitAt
		x.Next = Next{RevisitAt: &revisit, WouldChangeIf: a.WouldChangeIf, Note: "nothing to execute"}
		return x, nil
	}
	x.Decided = &Decided{Decision: ev.Decision, Outcomes: ev.Outcomes}
	x.Next.Note = "evaluated again in the next cycle"
	if n := len(ev.Outcomes); n > 0 {
		x.Next.Outcome = ev.Outcomes[n-1].State
		x.Next.Note = ev.Outcomes[n-1].Detail + "; evaluated again in the next cycle"
	}
	return x, nil
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

	for i, e := range x.Estimated {
		label := ""
		if i == 0 {
			label = "Estimated"
		}
		name := strings.ReplaceAll(e.Kind, "_", " ")
		name = strings.ToUpper(name[:1]) + name[1:]
		if e.ExpectedMsat == nil {
			line(label, "%s, %d days: no forecast.", name, e.HorizonHours/24)
		} else {
			line(label, "%s, %d days: %s sats expected, %s to %s (%s).", name, e.HorizonHours/24,
				commas(e.ExpectedMsat.Sats()), commas(e.LowMsat.Sats()), commas(e.HighMsat.Sats()), e.Interval)
		}
		more("Economic confidence: %s (%s).", e.EconomicConfidence.Level, e.EconomicConfidence.Basis)
		for _, ev := range e.Evidence {
			if ev.Prior {
				more("Prior: %s (a prior, not a price).", ev.Summary)
			}
		}
	}

	if a := x.Proposed.Abstention; a != nil {
		line("Proposed", "Nothing (%s): %s.", strings.ReplaceAll(string(a.Reason), "_", " "), a.Detail)
		line("Decided", "No intent, so no authorization.")
		line("Next", "Evaluated again %s.", a.RevisitAt.Format("2006-01-02 15:04 MST"))
		for _, w := range a.WouldChangeIf {
			more("Would act if %s.", w)
		}
		return b.String()
	}

	i := x.Proposed.Intent
	if fee := i.Parameters.Fee; fee != nil {
		line("Proposed", "Change the fee from %d to %d ppm, base %s msat unchanged, by %s.", fee.ObservedFeePPM, fee.FeePPM,
			commas(int64(fee.BaseMsat)), i.ExpiresAt.Format("2006-01-02 15:04 MST"))
	}
	more("%s", i.Rationale.Summary)
	if d := x.Decided; d != nil && d.Decision != nil {
		passed := 0
		for _, r := range d.Decision.Rules {
			if r.Passed {
				passed++
			}
		}
		line("Decided", "%s in %s mode: %d of %d rules passed.", capitalize(string(d.Decision.Verdict)), d.Decision.Mode, passed, len(d.Decision.Rules))
		for _, r := range d.Decision.Rules {
			mark := "pass"
			if !r.Passed {
				mark = "FAIL"
			}
			more("%s  %s: %s", mark, r.Rule, r.Detail)
		}
	}
	line("Next", "%s.", capitalize(x.Next.Note))
	return b.String()
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

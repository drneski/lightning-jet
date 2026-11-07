// Package engine runs Jet's decision cycle: observe the node, estimate each
// channel, let the fee strategy propose or abstain for every channel and the
// refill strategy for every depleted one, authorize every proposal, and
// record the whole cycle at once.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
	"github.com/drneski/lightning-jet/v2/internal/health"
	"github.com/drneski/lightning-jet/v2/internal/limits"
	"github.com/drneski/lightning-jet/v2/internal/observe"
	"github.com/drneski/lightning-jet/v2/internal/peer"
	"github.com/drneski/lightning-jet/v2/internal/store"
	"github.com/drneski/lightning-jet/v2/internal/strategy/fee"
	"github.com/drneski/lightning-jet/v2/internal/strategy/refill"
)

// Engine runs cycles.
type Engine struct {
	Observer  *observe.Observer
	Estimator *economics.Estimator
	// Strategy decides fees and Refills refills; either may be nil, its
	// engine off.
	Strategy   *fee.Strategy
	Refills    *refill.Strategy
	Authorizer *limits.Authorizer
	Store      *store.Store
	Now        func() time.Time
	NewID      func() string
	Log        *slog.Logger

	busySince atomic.Int64 // Unix ms when the running cycle began; 0 between cycles
}

// Stuck reports whether a cycle has been running longer than limit, which no
// healthy cycle does: every call to LND has its own deadline.
func (e *Engine) Stuck(now time.Time, limit time.Duration) bool {
	b := e.busySince.Load()
	return b != 0 && now.Sub(time.UnixMilli(b)) > limit
}

// Summary is what one cycle did. Intents, Abstentions, Approved and Rejected
// count fee decisions; Refills counts refill evaluations, of which
// RefillsApproved were approved.
type Summary struct {
	SnapshotID      string
	Channels        int
	Intents         int
	Abstentions     int
	Approved        int
	Rejected        int
	Refills         int
	RefillsApproved int
}

func (e *Engine) live() bool {
	return e.Strategy != nil && e.Strategy.Mode == contracts.ModeLive || e.Refills != nil && e.Refills.Mode == contracts.ModeLive ||
		e.Authorizer.FeesMode == contracts.ModeLive || e.Authorizer.RebalanceMode == contracts.ModeLive
}

// ErrLiveUnsupported is returned in live mode: this version has no executor,
// so nothing it approves could be carried out.
var ErrLiveUnsupported = errors.New("engine: live mode needs an executor, which this version of Jet does not have; use shadow")

// Cycle runs one decision cycle. It records everything the cycle produced in
// one transaction, or nothing if any step fails.
func (e *Engine) Cycle(ctx context.Context) (Summary, error) {
	if e.live() {
		return Summary{}, ErrLiveUnsupported
	}
	snap, err := e.Observer.Snapshot(ctx)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{SnapshotID: snap.ID, Channels: len(snap.Channels)}
	cycle := store.Cycle{Snapshot: snap}
	// propose authorizes an intent and records it with its decision and its
	// outcome, which in shadow mode is not to execute.
	propose := func(intent *contracts.Intent) (*contracts.Decision, error) {
		d, err := e.Authorizer.Authorize(ctx, intent)
		if err != nil {
			return nil, fmt.Errorf("engine: authorizing %s for channel %s: %w", intent.Action, intent.Target.ChanID, err)
		}
		cycle.Intents = append(cycle.Intents, intent)
		cycle.Decisions = append(cycle.Decisions, d)
		detail := "shadow mode: approved and recorded, not executed"
		if d.Verdict == contracts.Rejected {
			detail = "rejected: " + d.Reason
		}
		cycle.Outcomes = append(cycle.Outcomes, &contracts.Outcome{
			ID: e.NewID(), Schema: contracts.SchemaOutcome, IntentID: intent.ID,
			State: contracts.NotExecuted, Detail: detail, At: contracts.Timestamp(e.Now()),
		})
		return d, nil
	}

	var channels []refill.Channel
	for _, ch := range snap.Channels {
		est := e.Estimator.OutboundRevenue(snap, ch)
		rc := e.Estimator.ReplacementCost(snap, ch)
		cycle.Estimates = append(cycle.Estimates, est, rc)
		channels = append(channels, refill.Channel{Obs: ch, Revenue: est, Refill: rc})
		if e.Strategy == nil {
			continue
		}
		intent, abst := e.Strategy.Evaluate(snap, peer.New(ch), est, rc)
		if abst != nil {
			cycle.Abstentions = append(cycle.Abstentions, abst)
			sum.Abstentions++
			continue
		}
		d, err := propose(intent)
		if err != nil {
			return Summary{}, err
		}
		sum.Intents++
		if d.Verdict == contracts.Rejected {
			sum.Rejected++
		} else {
			sum.Approved++
		}
	}

	for _, c := range channels {
		if e.Refills == nil || !e.Refills.Target(c.Obs) {
			continue
		}
		sum.Refills++
		intent, abst := e.Refills.Evaluate(snap, c, channels)
		if abst != nil {
			cycle.Abstentions = append(cycle.Abstentions, abst)
			continue
		}
		d, err := propose(intent)
		if err != nil {
			return Summary{}, err
		}
		if d.Verdict == contracts.Approved {
			sum.RefillsApproved++
		}
	}
	if err := e.Store.SaveCycle(ctx, cycle); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

// Run runs a cycle at once and then every interval, until ctx is done. A
// failed cycle is logged and the next one runs on schedule.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	if e.live() {
		return ErrLiveUnsupported
	}
	e.heartbeat(ctx, health.KeyInterval, interval.Milliseconds())
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Log.Info("stopping")
			return nil
		case <-timer.C:
		}
		began := e.Now()
		e.busySince.Store(began.UnixMilli())
		sum, err := e.Cycle(ctx)
		e.busySince.Store(0)
		switch {
		case ctx.Err() != nil:
			e.Log.Info("cycle interrupted by shutdown; nothing from it was recorded")
		case err != nil:
			e.Log.Error("cycle failed; nothing from it was recorded", "err", err)
			e.heartbeat(ctx, health.KeyCycleFail, e.Now().UnixMilli())
			if serr := e.Store.SetMeta(ctx, health.KeyCycleErr, err.Error()); serr != nil {
				e.Log.Error("recording the failure", "err", serr)
			}
		default:
			e.heartbeat(ctx, health.KeyCycleOK, e.Now().UnixMilli())
			e.Log.Info("cycle recorded", "snapshot", sum.SnapshotID, "channels", sum.Channels,
				"intents", sum.Intents, "approved", sum.Approved, "rejected", sum.Rejected,
				"abstentions", sum.Abstentions, "refills", sum.Refills, "refills_approved", sum.RefillsApproved,
				"took", e.Now().Sub(began).Round(time.Millisecond))
		}
		timer.Reset(interval)
	}
}

func (e *Engine) heartbeat(ctx context.Context, key string, v int64) {
	if err := e.Store.SetMeta(ctx, key, strconv.FormatInt(v, 10)); err != nil {
		e.Log.Error("recording a heartbeat", "key", key, "err", err)
	}
}

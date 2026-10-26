// Package fee is Jet's first fee strategy, balance-band. It proposes a small
// fee experiment on a channel whose balance sits outside a band, when there is
// enough traffic to measure the result, and abstains otherwise. It proposes
// and never decides: authorization does that.
package fee

import (
	"fmt"
	"strings"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/economics"
)

// The strategy's identity, recorded on everything it proposes.
const (
	StrategyName    = "fee.balance-band"
	StrategyVersion = "0.1.0"
)

// Config is the strategy's settings.
type Config struct {
	// LowPct and HighPct bound the band of local balance, as a percentage of
	// capacity, inside which no change is called for.
	LowPct, HighPct int64
	// StepPct is the size of a fee change, as a percentage of the current fee.
	// A change is always at least 1 ppm.
	StepPct int64
	// MinConfidence is the economic confidence the baseline needs before an
	// experiment is worth running: below it, the result couldn't be measured.
	MinConfidence contracts.ConfidenceLevel
	// IntentTTL is how long a proposal stays valid.
	IntentTTL time.Duration
	// Revisit is when an abstention is due to be evaluated again.
	Revisit time.Duration
}

// DefaultConfig is the configuration the slice runs with.
var DefaultConfig = Config{
	LowPct: 20, HighPct: 80, StepPct: 10,
	MinConfidence: contracts.ConfidenceMedium,
	IntentTTL:     24 * time.Hour, Revisit: time.Hour,
}

// Strategy evaluates channels.
type Strategy struct {
	Config
	Mode  contracts.Mode
	Now   func() time.Time
	NewID func() string
}

var component = contracts.Component{Name: StrategyName, Version: StrategyVersion}

// Evaluate returns exactly one of an intent or an abstention for the channel,
// from its observation and its outbound revenue estimate.
func (s *Strategy) Evaluate(snap *contracts.Snapshot, ch contracts.ChannelObservation, est *contracts.Estimate) (*contracts.Intent, *contracts.Abstention) {
	now := contracts.Timestamp(s.Now())
	abstain := func(reason contracts.AbstentionReason, detail string, wouldChangeIf ...string) (*contracts.Intent, *contracts.Abstention) {
		return nil, &contracts.Abstention{
			ID: s.NewID(), Schema: contracts.SchemaAbstention, ChanID: ch.ChanID, Strategy: component,
			SnapshotID: snap.ID, EstimateIDs: []string{est.ID}, Reason: reason, Detail: detail,
			WouldChangeIf: wouldChangeIf, RevisitAt: now.Add(s.Revisit), CreatedAt: now,
		}
	}

	if ch.DataQuality.Level == contracts.DataInsufficient || ch.OurPolicy == nil || ch.CapacityMsat <= 0 {
		issues := strings.Join(ch.DataQuality.Issues, "; ")
		if issues == "" {
			issues = "capacity or our policy unknown"
		}
		return abstain(contracts.InsufficientData, "the inputs can't be trusted: "+issues,
			"the missing or failed inputs become available")
	}

	pct := int64(ch.LocalMsat) * 100 / int64(ch.CapacityMsat)
	if pct >= s.LowPct && pct <= s.HighPct {
		return abstain(contracts.NoProfitableAction,
			fmt.Sprintf("local balance %d%% is inside the %d-%d%% band, so no fee change is called for", pct, s.LowPct, s.HighPct),
			fmt.Sprintf("local balance falls below %d%% or rises above %d%%", s.LowPct, s.HighPct))
	}

	conf := est.EconomicConfidence
	if !conf.Level.AtLeast(s.MinConfidence) {
		return abstain(contracts.InsufficientEvidence,
			fmt.Sprintf("%s has economic confidence %s (%s); this strategy needs %s", est.Kind, conf.Level, conf.Basis, s.MinConfidence),
			fmt.Sprintf("at least %d outbound forwards through this channel within %d days",
				economics.ForwardsFor(s.MinConfidence), max(ch.Forwards.WindowHours/24, 1)))
	}

	ppm := ch.OurPolicy.FeePPM
	step := max(1, ppm*s.StepPct/100)
	var newPPM int64
	var why string
	if pct > s.HighPct {
		if ppm == 0 {
			return abstain(contracts.NoProfitableAction,
				fmt.Sprintf("local balance %d%% is above the band, but the fee is already 0 ppm, so there is nothing lower to test", pct),
				"the channel's fee is above 0 ppm")
		}
		newPPM = ppm - step
		why = fmt.Sprintf("local balance %d%% is above the %d%% band; lower the fee from %d to %d ppm to test whether price limits outbound flow",
			pct, s.HighPct, ppm, newPPM)
	} else {
		newPPM = ppm + step
		why = fmt.Sprintf("local balance %d%% is below the %d%% band while it routes; raise the fee from %d to %d ppm to ration scarce outbound liquidity",
			pct, s.LowPct, ppm, newPPM)
	}
	summary := fmt.Sprintf("Experiment: %s. Baseline: %d sats expected over %d days (%d to %d, %s).",
		why, est.ExpectedMsat.Sats(), est.HorizonHours/24, est.LowMsat.Sats(), est.HighMsat.Sats(), est.Interval)

	return &contracts.Intent{
		ID: s.NewID(), Schema: contracts.SchemaIntent, Action: contracts.ActionFeeUpdate,
		Target: contracts.Target{ChanID: ch.ChanID},
		Parameters: contracts.IntentParameters{Fee: &contracts.FeeParameters{
			BaseMsat: ch.OurPolicy.BaseMsat, FeePPM: newPPM,
			ObservedBaseMsat: ch.OurPolicy.BaseMsat, ObservedFeePPM: ppm,
		}},
		Rationale:  contracts.Rationale{EstimateIDs: []string{est.ID}, Summary: summary},
		Strategy:   component,
		SnapshotID: snap.ID, Mode: s.Mode, CreatedAt: now, ExpiresAt: now.Add(s.IntentTTL),
	}, nil
}

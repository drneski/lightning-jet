// Package contracts defines the records every Jet decision produces, as
// specified in docs/architecture/decision-contracts.md: snapshots, estimates,
// intents, abstentions, decisions and outcomes.
package contracts

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// MilliSat is an amount in millisatoshis. Amounts are never floating point.
type MilliSat int64

// Sats returns the amount in whole satoshis, rounded toward zero.
func (m MilliSat) Sats() int64 { return int64(m) / 1000 }

// ChanID is a short channel id. It is written as a decimal string in JSON,
// because it exceeds the integer range some JSON readers handle exactly.
type ChanID uint64

// ParseChanID parses a short channel id written in decimal.
func ParseChanID(s string) (ChanID, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid channel id %q", s)
	}
	return ChanID(v), nil
}

func (c ChanID) String() string { return strconv.FormatUint(uint64(c), 10) }

// FundingHeight returns the block the channel's funding transaction confirmed
// in, which the short channel id encodes in its top 24 bits.
func (c ChanID) FundingHeight() uint32 { return uint32(uint64(c) >> 40) }

func (c ChanID) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

func (c *ChanID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("channel id must be a decimal string: %w", err)
	}
	v, err := ParseChanID(s)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// Timestamp returns t in UTC at millisecond precision, the form every record
// stores.
func Timestamp(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// Component names a versioned piece of Jet: an estimator or a strategy.
type Component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Mode is an engine's mode. In shadow mode intents are authorized and
// recorded, never executed.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeShadow Mode = "shadow"
	ModeLive   Mode = "live"
)

// DataQualityLevel says whether inputs can be trusted as observations.
type DataQualityLevel string

const (
	DataGood         DataQualityLevel = "good"
	DataDegraded     DataQualityLevel = "degraded"
	DataInsufficient DataQualityLevel = "insufficient"
)

var dataRank = map[DataQualityLevel]int{DataInsufficient: 0, DataDegraded: 1, DataGood: 2}

// Worse returns the lower of two data quality levels.
func Worse(a, b DataQualityLevel) DataQualityLevel {
	if dataRank[a] <= dataRank[b] {
		return a
	}
	return b
}

// DataQuality is a data quality level with the issues that set it.
type DataQuality struct {
	Level  DataQualityLevel `json:"level"`
	Issues []string         `json:"issues,omitempty"`
}

// Add records an issue and lowers the level to at most the given one.
func (q *DataQuality) Add(level DataQualityLevel, issue string) {
	q.Level = Worse(q.Level, level)
	q.Issues = append(q.Issues, issue)
}

// ConfidenceLevel says how well the evidence supports a forecast. It is kept
// separate from data quality and never combined with it.
type ConfidenceLevel string

const (
	ConfidenceNone   ConfidenceLevel = "none"
	ConfidenceLow    ConfidenceLevel = "low"
	ConfidenceMedium ConfidenceLevel = "medium"
	ConfidenceHigh   ConfidenceLevel = "high"
)

var confidenceRank = map[ConfidenceLevel]int{ConfidenceNone: 0, ConfidenceLow: 1, ConfidenceMedium: 2, ConfidenceHigh: 3}

// AtLeast reports whether c is at least min.
func (c ConfidenceLevel) AtLeast(min ConfidenceLevel) bool {
	return confidenceRank[c] >= confidenceRank[min]
}

// EconomicConfidence is a confidence level with what it rests on.
type EconomicConfidence struct {
	Level ConfidenceLevel `json:"level"`
	Basis string          `json:"basis"`
}

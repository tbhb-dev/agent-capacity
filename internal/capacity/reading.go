// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package capacity validates plan readings and reads registered sources.
package capacity

import (
	"errors"
	"math"
	"regexp"
	"time"
)

// Reading describes one plan window. Sources convert percentages to ratios.
type Reading struct {
	ResetAt    time.Time
	ObservedAt time.Time
	Vendor     string
	Window     string
	Account    string
	Source     string
	UsedRatio  float64
}

// Source names identify the passive input format.
const (
	ClaudeStatusline   = "claude_statusline"
	ClaudeTranscript   = "claude_transcript"
	CodexRollout       = "codex_rollout"
	AGYStatusline      = "agy_statusline"
	statuslineFreshFor = 10 * time.Minute
	codexFreshFor      = 30 * time.Minute
	retainedFor        = 6 * time.Hour
)

// ErrInvalidReading contains no input values and is safe to classify in logs.
var ErrInvalidReading = errors.New("invalid reading")

var accountAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Validate rejects unknown labels, private account identifiers and invalid ratios.
func (r Reading) Validate() error {
	if !accountAlias.MatchString(r.Account) {
		return ErrInvalidReading
	}

	if math.IsNaN(r.UsedRatio) || r.UsedRatio < 0 || r.UsedRatio > 1 {
		return ErrInvalidReading
	}

	switch r.Vendor {
	case "anthropic", "openai", "google":
	default:
		return ErrInvalidReading
	}

	switch r.Window {
	case "5h", "7d", "7d_opus", "7d_sonnet", "other":
	default:
		return ErrInvalidReading
	}

	switch r.Source {
	case ClaudeStatusline, ClaudeTranscript, CodexRollout, AGYStatusline:
	default:
		return ErrInvalidReading
	}

	return nil
}

// Freshness returns a nonnegative age and fresh, carried or dropped.
// Readings marked dropped must not produce metric points.
func Freshness(reading Reading, now time.Time) (time.Duration, string) {
	age := max(now.Sub(reading.ObservedAt), 0)
	if age > retainedFor {
		return age, "dropped"
	}

	limit := statuslineFreshFor
	if reading.Source == CodexRollout {
		limit = codexFreshFor
	}

	if age <= limit {
		return age, "fresh"
	}

	return age, "carried"
}

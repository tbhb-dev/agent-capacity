// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

// SpoolSource reads the rate limits captured by the Codex probe.
type SpoolSource struct {
	Logger *slog.Logger
	Dir    string
}

// Read isolates failures by account and uses capture timestamps.
func (s SpoolSource) Read(ctx context.Context, _ time.Time) ([]capacity.Reading, error) {
	snapshots, failures, err := spool.Read(s.Dir, "codex")
	if err != nil {
		return nil, fmt.Errorf("read Codex snapshots: %w", err)
	}

	var readings []capacity.Reading

	for _, snapshot := range snapshots {
		batch, parseErr := Parse(snapshot)
		if parseErr != nil {
			failures = append(failures, capacity.ErrorClass(parseErr))

			continue
		}

		readings = append(readings, batch...)
	}

	for _, class := range failures {
		s.Logger.ErrorContext(ctx, "snapshot read failed", "class", class)
	}

	if len(readings) == 0 && len(failures) > 0 {
		return nil, capacity.ErrInvalidReading
	}

	return readings, nil
}

// Parse reads the rateLimits object returned by the app server.
func Parse(snapshot spool.Snapshot) ([]capacity.Reading, error) {
	type quotaWindow struct {
		UsedPercent        float64 `json:"usedPercent"`
		WindowDurationMins int     `json:"windowDurationMins"`
		ResetsAt           int64   `json:"resetsAt"`
	}

	var payload struct {
		Primary   *quotaWindow `json:"primary"`
		Secondary *quotaWindow `json:"secondary"`
	}
	if json.Unmarshal(snapshot.Data, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}

	var readings []capacity.Reading

	for _, window := range []*quotaWindow{payload.Primary, payload.Secondary} {
		if window == nil {
			continue
		}

		label := "5h"
		if window.WindowDurationMins == weekMinutes {
			label = "7d"
		}

		reading := capacity.Reading{
			Vendor: "openai", Account: snapshot.Account, Source: capacity.CodexProbe, Window: label, Bucket: "",
			UsedRatio: window.UsedPercent / percentScale, ResetAt: time.Unix(window.ResetsAt, 0).UTC(),
			ObservedAt: snapshot.ObservedAt,
		}
		if reading.Validate() != nil {
			return nil, capacity.ErrInvalidReading
		}

		readings = append(readings, reading)
	}

	if len(readings) == 0 {
		return nil, capacity.ErrInvalidReading
	}

	return readings, nil
}

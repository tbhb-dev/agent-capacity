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
func (s SpoolSource) Read(ctx context.Context, now time.Time) ([]capacity.Reading, error) {
	snapshots, failures, err := spool.Read(s.Dir, "codex")
	if err != nil {
		return nil, fmt.Errorf("read Codex snapshots: %w", err)
	}

	var readings []capacity.Reading

	for _, snapshot := range snapshots {
		batch, parseErr := Parse(ctx, s.Logger, snapshot, now)
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

// Parse logs the durations in the app server's rateLimits object and reads supported windows.
func Parse(
	ctx context.Context,
	logger *slog.Logger,
	snapshot spool.Snapshot,
	now time.Time,
) ([]capacity.Reading, error) {
	type quotaWindow struct {
		UsedPercent        *float64 `json:"usedPercent"`
		ResetsAt           *int64   `json:"resetsAt"`
		WindowDurationMins *int     `json:"windowDurationMins"`
	}

	var payload struct {
		Primary   *quotaWindow `json:"primary"`
		Secondary *quotaWindow `json:"secondary"`
	}
	if json.Unmarshal(snapshot.Data, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}

	var windows []*quotaWindow

	var seen []int

	for _, window := range []*quotaWindow{payload.Primary, payload.Secondary} {
		if window == nil || window.WindowDurationMins == nil {
			continue
		}

		windows = append(windows, window)
		seen = append(seen, *window.WindowDurationMins)
	}

	if len(windows) == 0 {
		return nil, capacity.ErrInvalidReading
	}

	logWindows(ctx, logger, snapshot.Account, seen, now)

	var readings []capacity.Reading

	for _, window := range windows {
		var label string

		switch *window.WindowDurationMins {
		case shortMinutes:
			label = "5h"
		case weekMinutes:
			label = "7d"
		default:
			continue
		}

		if window.UsedPercent == nil || window.ResetsAt == nil {
			return nil, capacity.ErrInvalidReading
		}

		reading := capacity.Reading{
			Vendor: "openai", Account: snapshot.Account, Source: capacity.CodexProbe, Window: label, Bucket: "",
			UsedRatio: *window.UsedPercent / percentScale, ResetAt: time.Unix(*window.ResetsAt, 0).UTC(),
			ObservedAt: snapshot.ObservedAt,
		}
		if reading.Validate() != nil {
			return nil, capacity.ErrInvalidReading
		}

		readings = append(readings, reading)
	}

	return readings, nil
}

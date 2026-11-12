// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package agy reads captured Antigravity usage windows.
package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

// Source reads each alias independently and reports failures using safe classes.
type Source struct {
	Logger *slog.Logger
	Dir    string
}

const percentageScale = 100

type window struct {
	UsedPercentage *float64 `json:"used_percentage"` //nolint:tagliatelle // The input format uses snake_case field names.
	ResetsAt       *int64   `json:"resets_at"`       //nolint:tagliatelle // The input format uses snake_case field names.
}

// Read uses capture timestamps, never file modification times.
func (s Source) Read(ctx context.Context, _ time.Time) ([]capacity.Reading, error) {
	snapshots, failures, err := spool.Read(s.Dir, "agy")
	if err != nil {
		return nil, fmt.Errorf("read Antigravity snapshots: %w", err)
	}

	var readings []capacity.Reading

	for _, snapshot := range snapshots {
		batch, parseErr := parse(snapshot)
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

func parse(snapshot spool.Snapshot) ([]capacity.Reading, error) {
	var payload struct {
		FiveHour *window `json:"five_hour"` //nolint:tagliatelle // The input format uses snake_case field names.
		SevenDay *window `json:"seven_day"` //nolint:tagliatelle // The input format uses snake_case field names.
	}
	if json.Unmarshal(snapshot.Data, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}

	var readings []capacity.Reading

	for _, entry := range []struct {
		value *window
		name  string
	}{
		{payload.FiveHour, "5h"}, {payload.SevenDay, "7d"},
	} {
		if entry.value == nil {
			continue
		}

		if entry.value.UsedPercentage == nil || entry.value.ResetsAt == nil {
			return nil, capacity.ErrInvalidReading
		}

		reading := capacity.Reading{
			Vendor: "google", Window: entry.name, Account: snapshot.Account,
			Source: capacity.AGYStatusline, UsedRatio: *entry.value.UsedPercentage / percentageScale,
			ResetAt: time.Unix(*entry.value.ResetsAt, 0).UTC(), ObservedAt: snapshot.ObservedAt,
		}

		err := reading.Validate()
		if err != nil {
			return nil, capacity.ErrInvalidReading
		}

		readings = append(readings, reading)
	}

	return readings, nil
}

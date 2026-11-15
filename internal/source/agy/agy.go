// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package agy reads Antigravity usage windows that the probe captured.
package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

// Source reads each alias independently and reports failures using safe classes.
type Source struct {
	Logger *slog.Logger
	Dir    string
}

// bucket is one quota window of one model group.
//
// The usage command leaves out a fraction of zero. A missing value means the
// group has used its whole window.
//
//nolint:tagliatelle // The input format uses snake_case field names.
type bucket struct {
	RemainingFraction *float64 `json:"remaining_fraction"`
	ID                string   `json:"id"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"reset_time"`
}

// label maps a bucket to its window and quota group, and reports unknown buckets.
func label(entry bucket) (string, string, bool) {
	group, _, _ := strings.Cut(entry.ID, "-")
	if group != "gemini" && group != "3p" {
		return "", "", false
	}

	switch entry.Window {
	case "5h":
		return "5h", group, true
	case "weekly":
		return "7d", group, true
	default:
		return "", "", false
	}
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

// parse reads the data object of the usage command, which lists model groups
// and their quota buckets. It skips buckets it doesn't know. A snapshot without
// any known bucket fails. A changed output format then reports an error
// instead of an empty batch.
func parse(snapshot spool.Snapshot) ([]capacity.Reading, error) {
	var payload struct {
		Groups []struct {
			Buckets []bucket `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(snapshot.Data, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}

	var readings []capacity.Reading

	for _, group := range payload.Groups {
		for _, entry := range group.Buckets {
			reading, err := convert(snapshot, entry)
			if err != nil {
				return nil, err
			}

			if reading != nil {
				readings = append(readings, *reading)
			}
		}
	}

	if len(readings) == 0 {
		return nil, capacity.ErrInvalidReading
	}

	return readings, nil
}

// convert turns one known bucket into a reading. An unknown bucket returns nil and no error.
func convert(snapshot spool.Snapshot, entry bucket) (*capacity.Reading, error) {
	window, group, known := label(entry)
	if !known {
		return nil, nil //nolint:nilnil // An unknown bucket is neither a reading nor an error.
	}

	reset, err := time.Parse(time.RFC3339, entry.ResetTime)
	if err != nil {
		return nil, capacity.ErrInvalidReading
	}

	remaining := 0.0
	if entry.RemainingFraction != nil {
		remaining = *entry.RemainingFraction
	}

	reading := capacity.Reading{
		Vendor: "google", Window: window, Bucket: group, Account: snapshot.Account,
		Source: capacity.AGYStatusline, UsedRatio: 1 - remaining,
		ResetAt: reset.UTC(), ObservedAt: snapshot.ObservedAt,
	}
	if reading.Validate() != nil {
		return nil, capacity.ErrInvalidReading
	}

	return &reading, nil
}

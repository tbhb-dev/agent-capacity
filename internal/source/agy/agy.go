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
// The usage command's struct tag leaves out a fraction of zero, and Parse reads
// a missing value as a fully used window. It does so only while another known
// bucket carries the field, because a renamed field looks the same on the wire.
// A snapshot with every window used up at once then reads as invalid until one
// real sample of a used-up bucket shows whether the command leaves the field out.
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

// Parse reads the data object of the usage command, with its model groups and
// quota buckets, and skips buckets it doesn't know. When no known bucket appears,
// or none carries a remaining fraction, it fails: a changed output format then
// reports an error instead of an empty or fully used batch.
func Parse(snapshot spool.Snapshot) ([]capacity.Reading, error) {
	var payload struct {
		Groups []struct {
			Buckets []bucket `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(snapshot.Data, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}

	var readings []capacity.Reading

	carried := false

	for _, group := range payload.Groups {
		for _, entry := range group.Buckets {
			reading, err := convert(snapshot, entry)
			if err != nil {
				return nil, err
			}

			if reading != nil {
				readings = append(readings, *reading)
				carried = carried || entry.RemainingFraction != nil
			}
		}
	}

	if !carried {
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

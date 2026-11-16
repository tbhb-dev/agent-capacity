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

// SpoolSource reads usage captured by the Codex probe for each account.
type SpoolSource struct {
	Logger *slog.Logger
	Dir    string
}

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

// Parse converts the app server's rate limit windows to readings.
func Parse(snapshot spool.Snapshot) ([]capacity.Reading, error) {
	var payload struct {
		Primary   *probeWindow `json:"primary"`
		Secondary *probeWindow `json:"secondary"`
	}
	if json.Unmarshal(snapshot.Data, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}
	var readings []capacity.Reading
	for _, entry := range []*probeWindow{payload.Primary, payload.Secondary} {
		if entry == nil {
			continue
		}
		label := "5h"
		if entry.WindowDurationMins == weekMinutes {
			label = "7d"
		}
		reading := capacity.Reading{
			Vendor: "openai", Account: snapshot.Account, Source: capacity.CodexProbe, Window: label, Bucket: "",
			UsedRatio: entry.UsedPercent / percentScale, ObservedAt: snapshot.ObservedAt, ResetAt: time.Unix(entry.ResetsAt, 0),
		}
		if err := reading.Validate(); err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}
	if len(readings) == 0 {
		return nil, capacity.ErrInvalidReading
	}
	return readings, nil
}

type probeWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

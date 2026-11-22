// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const tokenScannerBytes = chunkSize + 2

//nolint:tagliatelle // Codex rollout usage fields use snake_case.
type tokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
}

// Only metadata is decoded; the decoder discards content fields.
//
//nolint:tagliatelle // Codex rollout usage fields use snake_case.
type tokenEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Payload   struct {
		Timestamp time.Time `json:"timestamp"`
		Info      *struct {
			Total tokenUsage `json:"total_token_usage"`
		} `json:"info"`
		Model string `json:"model"`
		Type  string `json:"type"`
	} `json:"payload"`
	Type string `json:"type"`
}

type sessionTokens struct {
	model    string
	previous [4]int64
	baseline bool
}

func (s Source) windowTokens(
	ctx context.Context, now time.Time, reading capacity.Reading, remaining *int64,
) map[string][4]int64 {
	minutes := shortMinutes
	if reading.Window == "7d" {
		minutes = weekMinutes
	}

	start := reading.ResetAt.Add(-time.Duration(minutes) * time.Minute)
	if start.After(now) {
		return nil
	}

	totals := map[string][4]int64{}

	err := fs.WalkDir(s.files, "sessions", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errRead
		}

		if !rolloutPath.MatchString(path) || !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return errRead
		}

		if !info.ModTime().Before(start) && !s.scanTokens(ctx, path, start, now, remaining, totals) {
			return errRead
		}

		return nil
	})
	if err != nil {
		return nil
	}

	return totals
}

func (s Source) scanTokens(
	ctx context.Context, path string, start, now time.Time, remaining *int64, totals map[string][4]int64,
) bool {
	file, err := s.files.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() > *remaining {
		return false
	}

	*remaining -= info.Size()
	scanner := bufio.NewScanner(io.LimitReader(file, info.Size()))
	scanner.Buffer(make([]byte, tokenScannerBytes), tokenScannerBytes)

	var session sessionTokens

	for scanner.Scan() {
		if ctx.Err() != nil || len(scanner.Bytes()) > chunkSize {
			return false
		}

		var event tokenEvent
		if json.Unmarshal(scanner.Bytes(), &event) == nil && !event.Timestamp.After(now) {
			session.consider(event, start, totals)
		}
	}

	return scanner.Err() == nil
}

func (s *sessionTokens) consider(event tokenEvent, start time.Time, totals map[string][4]int64) {
	if event.Type == "turn_context" || event.Type == "session_meta" {
		s.setContext(event, start)

		return
	}

	if event.Type != "event_msg" || event.Payload.Type != "token_count" || event.Payload.Info == nil {
		return
	}

	if event.Timestamp.Before(start) {
		s.baseline = true

		return
	}

	usage := event.Payload.Info.Total
	current := [4]int64{
		usage.Input - usage.CacheRead - usage.CacheWrite,
		usage.Output,
		usage.CacheRead,
		usage.CacheWrite,
	}

	if !s.baseline && s.model != "" {
		counts := totals[s.model]
		for i, count := range current {
			counts[i] += count - s.previous[i]
		}

		totals[s.model] = counts
	}

	// For a session crossing the start, omit the first in-window sample.
	s.previous = current
	s.baseline = false
}

func (s *sessionTokens) setContext(event tokenEvent, start time.Time) {
	if event.Payload.Model != "" {
		s.model = event.Payload.Model
	}

	began := event.Timestamp
	if !event.Payload.Timestamp.IsZero() {
		began = event.Payload.Timestamp
	}

	if event.Type == "session_meta" && began.Before(start) {
		s.baseline = true
	}
}

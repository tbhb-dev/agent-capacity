// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const tokenLineBytes = 32 * 1024 * 1024

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

type tokenSample struct {
	model  string
	counts [4]int64
}

type windowTokenTotals struct {
	counts map[string][4]int64
	seen   map[tokenSample]struct{}
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

	end := now
	if reading.ResetAt.Before(now) {
		end = reading.ResetAt
	}

	totals := windowTokenTotals{counts: map[string][4]int64{}, seen: map[tokenSample]struct{}{}}

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

		if !info.ModTime().Before(start) && !s.scanTokens(ctx, path, start, end, remaining, totals) {
			return errRead
		}

		return nil
	})
	if err != nil {
		return nil
	}

	return totals.counts
}

func (s Source) scanTokens(
	ctx context.Context, path string, start, end time.Time, remaining *int64, totals windowTokenTotals,
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
	reader := bufio.NewReader(io.LimitReader(file, info.Size()))

	var session sessionTokens

	for {
		if ctx.Err() != nil {
			return false
		}

		line, long, readErr := readTokenLine(reader)
		if !long {
			var event tokenEvent
			if json.Unmarshal(line, &event) == nil && !event.Timestamp.After(end) {
				session.consider(event, start, totals)
			}
		}

		if readErr != nil {
			return errors.Is(readErr, io.EOF)
		}
	}
}

// readTokenLine returns one line of any length up to tokenLineBytes. Longer lines are consumed and reported as long.
func readTokenLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte

	long := false

	for {
		part, err := reader.ReadSlice('\n')
		if !long {
			line = append(line, part...)
			if len(line) > tokenLineBytes {
				line, long = nil, true
			}
		}

		if !errors.Is(err, bufio.ErrBufferFull) {
			return bytes.TrimRight(line, "\r\n"), long, err
		}
	}
}

func (s *sessionTokens) consider(event tokenEvent, start time.Time, totals windowTokenTotals) {
	if event.Type == "turn_context" || event.Type == "session_meta" {
		s.setContext(event, start)

		return
	}

	if event.Type != "event_msg" || event.Payload.Type != "token_count" || event.Payload.Info == nil {
		return
	}

	var zeroUsage tokenUsage

	usage := event.Payload.Info.Total
	if usage == zeroUsage {
		return
	}

	if event.Timestamp.Before(start) {
		s.baseline = true

		return
	}

	current := [4]int64{
		usage.Input - usage.CacheRead - usage.CacheWrite,
		usage.Output,
		usage.CacheRead,
		usage.CacheWrite,
	}

	sample := tokenSample{model: s.model, counts: current}
	_, counted := totals.seen[sample]

	if !s.baseline && s.model != "" && !counted {
		counts := totals.counts[s.model]
		for i, count := range current {
			counts[i] += count - s.previous[i]
		}

		totals.counts[s.model] = counts
		totals.seen[sample] = struct{}{}
	}

	// For a session crossing the start, omit the first in-window sample.
	// A copied sample still sets the previous counts for the fork.
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

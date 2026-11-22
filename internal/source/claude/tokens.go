// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	transcriptTokenBytes = capacity.WindowTokenBytes
	tokenLineBytes       = 32 * 1024 * 1024
	tokenFiveHours       = 5 * time.Hour
	tokenSevenDays       = 7 * 24 * time.Hour
)

// Only usage metadata is decoded; message content is discarded by the decoder.
//
//nolint:tagliatelle // Claude usage field names use snake_case.
type tokenEvent struct {
	Message struct {
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		ID    string `json:"id"`
		Model string `json:"model"`
	} `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
}

func (s TranscriptSource) windowTokens(now time.Time, reading capacity.Reading, remaining *int64) map[string][4]int64 {
	length := tokenFiveHours
	if reading.Window == "7d" {
		length = tokenSevenDays
	}

	start := reading.ResetAt.Add(-length)
	// A window that has already reset ends at its reset time, so the next window's tokens stay out.
	end := now
	if reading.ResetAt.Before(now) {
		end = reading.ResetAt
	}

	messages := map[string]tokenEvent{}

	for _, path := range s.tokenPaths() {
		if !s.scanTokens(path, start, end, remaining, messages) {
			return nil
		}
	}

	totals := map[string][4]int64{}
	for _, event := range messages {
		counts := totals[event.Message.Model]

		usage := event.Message.Usage
		for i, count := range [4]int64{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite} {
			counts[i] += count
		}

		totals[event.Message.Model] = counts
	}

	return totals
}

func (s TranscriptSource) scanTokens(
	path string,
	start, now time.Time,
	remaining *int64,
	messages map[string]tokenEvent,
) bool {
	file, err := s.files.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	if info.ModTime().Before(start) {
		return true
	}

	if info.Size() > *remaining {
		return false
	}

	*remaining -= info.Size()
	reader := bufio.NewReader(io.LimitReader(file, info.Size()))

	for {
		line, long, readErr := readTokenLine(reader)
		if !long {
			considerTokens(line, start, now, messages)
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

func considerTokens(line []byte, start, now time.Time, messages map[string]tokenEvent) {
	event, valid := decodeTokens(line, start, now)
	if !valid {
		return
	}

	previous, exists := messages[event.Message.ID]
	if !exists || !event.Timestamp.Before(previous.Timestamp) {
		messages[event.Message.ID] = event
	}
}

func decodeTokens(line []byte, start, now time.Time) (tokenEvent, bool) {
	var event tokenEvent

	valid := json.Unmarshal(line, &event) == nil && event.Type == "assistant" &&
		event.Message.ID != "" && event.Message.Model != "" && event.Message.Usage != nil &&
		!event.Timestamp.Before(start) && !event.Timestamp.After(now)

	return event, valid
}

func (s TranscriptSource) tokenPaths() []string {
	var paths []string

	seen := map[string]bool{}

	for _, name := range s.paths {
		for _, path := range append(transcriptPaths(s.files, name), subagentPaths(s.files, name)...) {
			if !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
		}
	}

	return paths
}

// subagentPaths finds the transcripts that Task and Agent runs write under <dir>/<session>/subagents.
func subagentPaths(files fs.FS, name string) []string {
	entries, err := fs.ReadDir(files, name)
	if err != nil {
		return nil
	}

	var paths []string

	for _, entry := range entries {
		if entry.IsDir() {
			paths = append(paths, transcriptPaths(files, filepath.Join(name, entry.Name(), "subagents"))...)
		}
	}

	return paths
}

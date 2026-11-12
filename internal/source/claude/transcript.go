// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	transcriptBytes = 8 * 1024 * 1024
	transcriptLine  = 64 * 1024
)

// TranscriptSource keeps only window readings from explicitly named files.
type TranscriptSource struct {
	files   fs.FS
	account string
	paths   []string
}

type transcriptFiles struct{}

func (transcriptFiles) Open(name string) (fs.File, error) {
	file, err := os.Open(name)

	return file, errors.Join(err)
}

// RegisterTranscripts returns no source without explicit transcript paths.
func RegisterTranscripts(paths, account string) (capacity.Registry, error) {
	if paths == "" {
		return nil, nil
	}

	if !spool.ValidIdentity("claude", account) {
		return nil, capacity.ErrInvalidReading
	}

	return capacity.Registry{{
		Name:   capacity.ClaudeTranscript,
		Source: TranscriptSource{files: transcriptFiles{}, paths: strings.Split(paths, ","), account: account},
	}}, nil
}

// Read skips unreadable files and chooses the newest valid event per window.
func (s TranscriptSource) Read(ctx context.Context, _ time.Time) ([]capacity.Reading, error) {
	latest := map[string]capacity.Reading{}

	for _, name := range s.paths {
		err := ctx.Err()
		if err != nil {
			return nil, errors.Join(err)
		}

		for _, path := range transcriptPaths(s.files, name) {
			s.readTranscript(path, latest)
		}
	}

	var readings []capacity.Reading

	for _, name := range []string{"5h", "7d"} {
		if reading, ok := latest[name]; ok {
			readings = append(readings, reading)
		}
	}

	return readings, nil
}

func transcriptPaths(files fs.FS, name string) []string {
	info, err := fs.Stat(files, name)
	if err != nil {
		return nil
	}

	if !info.IsDir() {
		return []string{name}
	}

	entries, err := fs.ReadDir(files, name)
	if err != nil {
		return nil
	}

	var paths []string

	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if entry.Type().IsRegular() && (ext == ".jsonl" || ext == ".ndjson") {
			paths = append(paths, filepath.Join(name, entry.Name()))
		}
	}

	return paths
}

func (s TranscriptSource) readTranscript(name string, latest map[string]capacity.Reading) {
	file, err := s.files.Open(name)
	if err != nil {
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}

	reader, ok := file.(io.ReaderAt)
	if !ok {
		return
	}

	size := min(info.Size(), int64(transcriptBytes))
	offset := info.Size() - size
	data := make([]byte, size)

	_, err = reader.ReadAt(data, offset)
	if err != nil {
		return
	}

	lines := bytes.Split(data, []byte{'\n'})
	if offset > 0 {
		// A bounded tail can start in the middle of a line.
		lines = lines[1:]
	}

	for _, line := range lines {
		if len(line) <= transcriptLine {
			s.considerTranscript(line, info.ModTime(), latest)
		}
	}
}

//nolint:tagliatelle // Claude events mix snake_case and camelCase field names.
type transcriptWindow struct {
	Utilization *float64        `json:"utilization"`
	ResetsAt    *transcriptTime `json:"resetsAt"`
	ResetTime   *transcriptTime `json:"reset_time"`
	Status      *string         `json:"status"`
}

//nolint:tagliatelle // Claude events mix snake_case and camelCase field names.
type transcriptEvent struct {
	Info struct {
		Windows struct {
			FiveHour *transcriptWindow `json:"five_hour"`
			SevenDay *transcriptWindow `json:"seven_day"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
	Timestamp *transcriptTime `json:"timestamp"`
	Type      string          `json:"type"`
}

func (s TranscriptSource) considerTranscript(line []byte, modified time.Time, latest map[string]capacity.Reading) {
	var event transcriptEvent
	if json.Unmarshal(line, &event) != nil || event.Type != "rate_limit_event" {
		return
	}

	observed := modified
	if event.Timestamp != nil {
		observed = event.Timestamp.Time
	}

	for _, entry := range []struct {
		window *transcriptWindow
		name   string
	}{{event.Info.Windows.FiveHour, "5h"}, {event.Info.Windows.SevenDay, "7d"}} {
		reading := transcriptReading(entry.window, entry.name, s.account, observed)
		if reading == nil {
			continue
		}

		previous, exists := latest[entry.name]
		if !exists || !observed.Before(previous.ObservedAt) {
			latest[entry.name] = *reading
		}
	}
}

func transcriptReading(value *transcriptWindow, name, account string, observed time.Time) *capacity.Reading {
	if value == nil || value.Utilization == nil || value.Status == nil {
		return nil
	}

	reset := value.ResetsAt
	if reset == nil {
		reset = value.ResetTime
	}

	if reset == nil {
		return nil
	}

	reading := capacity.Reading{
		Vendor: "anthropic", Source: capacity.ClaudeTranscript, Account: account, Window: name,
		UsedRatio: *value.Utilization, ResetAt: reset.Time, ObservedAt: observed,
	}

	if reading.Validate() != nil {
		return nil
	}

	return &reading
}

// transcriptTime accepts Unix seconds or an Internet timestamp.
type transcriptTime struct{ time.Time }

func (stamp *transcriptTime) UnmarshalJSON(data []byte) error {
	var seconds int64
	if json.Unmarshal(data, &seconds) == nil {
		stamp.Time = time.Unix(seconds, 0).UTC()

		return nil
	}

	return errors.Join(stamp.Time.UnmarshalJSON(data))
}

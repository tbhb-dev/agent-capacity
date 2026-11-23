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
	transcriptBytes       = 8 * 1024 * 1024
	transcriptLine        = 64 * 1024
	transcriptRetainedFor = 6 * time.Hour
	transcriptClockSkew   = 5 * time.Minute
)

// TranscriptSource keeps only window readings from explicitly named files.
type TranscriptSource struct {
	//exhaustruct:optional
	index   *tokenIndex // Keeps parsed token usage between collections; a missing index reads from scratch each time.
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
		Name: capacity.ClaudeTranscript,
		Source: TranscriptSource{
			files: transcriptFiles{}, paths: strings.Split(paths, ","), account: account, index: newTokenIndex(),
		},
	}}, nil
}

// Read skips unreadable files and chooses the newest valid event per window.
func (s TranscriptSource) Read(ctx context.Context, now time.Time) ([]capacity.Reading, error) {
	latest := map[string]capacity.Reading{}

	for _, name := range s.paths {
		err := ctx.Err()
		if err != nil {
			return nil, errors.Join(err)
		}

		for _, path := range mainPaths(s.files, name) {
			s.readTranscript(path, now, latest)
		}
	}

	return s.withTokens(now, latest), nil
}

// withTokens adds token totals to each window reading, in window order. A reading only gets tokens once the
// index has read every transcript to its end, so a host still catching up exports no partial totals.
func (s TranscriptSource) withTokens(now time.Time, latest map[string]capacity.Reading) []capacity.Reading {
	index := s.index
	if index == nil {
		index = newTokenIndex()
	}

	caughtUp := index.refresh(s.files, s.tokenPaths(), now)

	var readings []capacity.Reading

	for _, name := range []string{"5h", "7d"} {
		reading, found := latest[name]
		if !found {
			// Without a rate-limit event the window is rolling: it ends now and reports tokens only.
			reading = capacity.Reading{
				Bucket: "", Vendor: vendorAnthropic, Source: capacity.ClaudeTranscript, Account: s.account,
				Window: name, ObservedAt: now, ResetAt: now, UsedRatio: 0, TokensOnly: true,
			}
		}

		if caughtUp {
			reading.Tokens = windowTokens(index, now, reading)
		}

		if found || len(reading.Tokens) > 0 {
			readings = append(readings, reading)
		}
	}

	return readings
}

// mainPaths reads a project directory as is. A directory without transcripts of its own
// is a projects root, so it reads one level down.
func mainPaths(files fs.FS, name string) []string {
	paths := transcriptPaths(files, name)
	if len(paths) > 0 {
		// Transcripts directly inside mean this is already a project directory.
		return paths
	}

	for _, project := range projectDirs(files, name) {
		paths = append(paths, transcriptPaths(files, project)...)
	}

	return paths
}

func projectDirs(files fs.FS, name string) []string {
	entries, err := fs.ReadDir(files, name)
	if err != nil {
		return nil
	}

	var dirs []string

	for _, entry := range entries {
		dir := filepath.Join(name, entry.Name())

		// A session directory holds subagents, so it is not a project directory.
		_, statErr := fs.Stat(files, filepath.Join(dir, "subagents"))
		if entry.IsDir() && statErr != nil {
			dirs = append(dirs, dir)
		}
	}

	return dirs
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

func (s TranscriptSource) readTranscript(name string, now time.Time, latest map[string]capacity.Reading) {
	file, err := s.files.Open(name)
	if err != nil {
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}

	if info.ModTime().Before(now.Add(-transcriptRetainedFor)) {
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
			s.considerTranscript(line, info.ModTime(), now, latest)
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

func (s TranscriptSource) considerTranscript(line []byte, modified, now time.Time, latest map[string]capacity.Reading) {
	var event transcriptEvent
	if json.Unmarshal(line, &event) != nil || event.Type != "rate_limit_event" {
		return
	}

	observed := modified
	if event.Timestamp != nil {
		observed = event.Timestamp.Time
	}

	if observed.After(now.Add(transcriptClockSkew)) {
		return
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
		Bucket: "", Vendor: vendorAnthropic, Source: capacity.ClaudeTranscript, Account: account, Window: name,
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

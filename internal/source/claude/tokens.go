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
	"sync"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	tokenPassBytes = 512 * 1024 * 1024
	tokenLineBytes = 32 * 1024 * 1024
	tokenFiveHours = 5 * time.Hour
	tokenSevenDays = 7 * 24 * time.Hour
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

func windowTokens(index *tokenIndex, now time.Time, reading capacity.Reading) map[string][4]int64 {
	length := tokenFiveHours
	if reading.Window == "7d" {
		length = tokenSevenDays
	}

	// A window that has already reset ends at its reset time, so the next window's tokens stay out.
	end := now
	if reading.ResetAt.Before(now) {
		end = reading.ResetAt
	}

	return index.totals(reading.ResetAt.Add(-length), end)
}

// tokenIndex keeps parsed usage between collections, so each pass reads only the bytes appended since the last one.
type tokenIndex struct {
	offsets   map[string]int64
	events    map[string]tokenEvent
	passBytes int64
	lock      sync.Mutex
}

func newTokenIndex() *tokenIndex {
	return &tokenIndex{
		offsets: map[string]int64{}, events: map[string]tokenEvent{}, passBytes: tokenPassBytes, lock: sync.Mutex{},
	}
}

// refresh reads new transcript bytes up to the pass budget and drops usage older than the longest window.
// It reports whether every file is read to its end; totals from an index that is still catching up are partial.
func (x *tokenIndex) refresh(files fs.FS, paths []string, now time.Time) bool {
	x.lock.Lock()
	defer x.lock.Unlock()

	budget := x.passBytes
	start := now.Add(-tokenSevenDays)
	caughtUp := true

	for _, path := range paths {
		if !x.scan(files, path, start, now, &budget) {
			caughtUp = false
		}
	}

	for id, event := range x.events {
		if event.Timestamp.Before(start) {
			delete(x.events, id)
		}
	}

	return caughtUp
}

func (x *tokenIndex) scan(files fs.FS, path string, start, now time.Time, budget *int64) bool {
	file, err := files.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	info, err := file.Stat()
	source, readable := file.(io.ReaderAt)

	if err != nil || !info.Mode().IsRegular() || !readable {
		return false
	}

	if info.ModTime().Before(start) {
		delete(x.offsets, path)

		return true
	}

	offset := x.offsets[path]
	if offset > info.Size() {
		offset = 0 // The file shrank, so it was replaced.
	}

	limit := min(info.Size()-offset, *budget)
	caught := limit == info.Size()-offset
	reader := bufio.NewReader(io.NewSectionReader(source, offset, limit))

	for {
		line, long, size, readErr := readTokenLine(reader)
		if readErr != nil { // A partial last line stays unread until its newline arrives.
			break
		}

		offset += size
		*budget -= size

		if !long {
			considerTokens(line, start, now, x.events)
		}
	}

	x.offsets[path] = offset

	return caught
}

func (x *tokenIndex) totals(start, end time.Time) map[string][4]int64 {
	x.lock.Lock()
	defer x.lock.Unlock()

	totals := map[string][4]int64{}

	for _, event := range x.events {
		if event.Timestamp.Before(start) || event.Timestamp.After(end) {
			continue
		}

		counts := totals[event.Message.Model]

		usage := event.Message.Usage
		for i, count := range [4]int64{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite} {
			counts[i] += count
		}

		totals[event.Message.Model] = counts
	}

	return totals
}

// readTokenLine returns one line of any length up to tokenLineBytes and the bytes it consumed.
// Longer lines are consumed and reported as long.
func readTokenLine(reader *bufio.Reader) ([]byte, bool, int64, error) {
	var line []byte

	long := false
	size := int64(0)

	for {
		part, err := reader.ReadSlice('\n')
		size += int64(len(part))

		if !long {
			line = append(line, part...)
			if len(line) > tokenLineBytes {
				line, long = nil, true
			}
		}

		if !errors.Is(err, bufio.ErrBufferFull) {
			return bytes.TrimRight(line, "\r\n"), long, size, err
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
		for _, path := range append(mainPaths(s.files, name), subagentTree(s.files, name)...) {
			if !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
		}
	}

	return paths
}

// subagentTree reads subagents of the directory itself and of each project directory below it.
func subagentTree(files fs.FS, name string) []string {
	paths := subagentPaths(files, name)
	if len(transcriptPaths(files, name)) > 0 {
		return paths
	}

	for _, project := range projectDirs(files, name) {
		paths = append(paths, subagentPaths(files, project)...)
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

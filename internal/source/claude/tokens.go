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
	"slices"
	"sync"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	tokenPassBytes = 512 * 1024 * 1024
	tokenLineBytes = 32 * 1024 * 1024
	tokenFiveHours = 5 * time.Hour
	tokenSevenDays = 7 * 24 * time.Hour
	// The index keeps a day more than the longest window, so a window that reset a few hours ago is still whole.
	tokenRetention  = 8 * 24 * time.Hour
	tokenPrintBytes = 128
)

// Only usage metadata is decoded; message content is discarded by the decoder.
//
//nolint:tagliatelle // Claude usage field names use snake_case.
type tokenRecord struct {
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

// tokenEvent is one assistant line with usage, kept in file order so later lines win ties like a full scan.
type tokenEvent struct {
	Timestamp time.Time
	ID        string
	Model     string
	Counts    [4]int64
}

// transcriptTokens is what the index knows about one transcript: how far it read, the first and last bytes it read
// (to notice a replaced file), and every usage line read so far.
type transcriptTokens struct {
	modTime time.Time
	sample  []byte
	events  []tokenEvent
	offset  int64
}

// windowTokens totals one window, or reports false when the index no longer holds that far back.
func windowTokens(index *tokenIndex, now time.Time, reading capacity.Reading) (map[string][4]int64, bool) {
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
	files     map[string]*transcriptTokens
	cutoff    time.Time
	order     []string
	passBytes int64
	lock      sync.Mutex
}

func newTokenIndex() *tokenIndex {
	return &tokenIndex{
		files: map[string]*transcriptTokens{}, cutoff: time.Time{}, order: nil,
		passBytes: tokenPassBytes, lock: sync.Mutex{},
	}
}

// refresh reads new transcript bytes up to the pass budget. It reports whether the index now holds every
// transcript in full: paths must be a complete listing, and each file must be read to its end. Totals from
// an index that reports false would be partial, so the caller withholds them.
func (x *tokenIndex) refresh(files fs.FS, paths []string, listed bool, now time.Time) bool {
	x.lock.Lock()
	defer x.lock.Unlock()

	x.cutoff = now.Add(-tokenRetention)
	x.order = paths

	if listed {
		for path := range x.files {
			if !slices.Contains(paths, path) {
				delete(x.files, path) // The transcript is gone, so its usage goes with it.
			}
		}
	}

	budget := x.passBytes
	caughtUp := listed

	for _, path := range paths {
		if !x.scan(files, path, &budget) {
			caughtUp = false
		}
	}

	for _, state := range x.files {
		state.events = slices.DeleteFunc(state.events, func(event tokenEvent) bool {
			return event.Timestamp.Before(x.cutoff)
		})
	}

	return caughtUp
}

func (x *tokenIndex) scan(files fs.FS, path string, budget *int64) bool {
	file, err := files.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true // The listing pass drops its usage.
	}

	if err != nil {
		return false
	}

	defer file.Close()

	info, err := file.Stat()
	source, readable := file.(io.ReaderAt)

	if err != nil || !info.Mode().IsRegular() || !readable {
		return false
	}

	if info.ModTime().Before(x.cutoff) {
		delete(x.files, path)

		return true
	}

	state := x.files[path]
	if state == nil || !state.intact(source, info) {
		state = &transcriptTokens{modTime: time.Time{}, sample: nil, events: nil, offset: 0}
		x.files[path] = state // Shrunk or replaced, so start over.
	}

	return state.read(source, info, x.cutoff, budget)
}

// intact reports whether the file still holds what the index read: not shorter, the same first and last bytes,
// and not rewritten in place without growing.
func (f *transcriptTokens) intact(source io.ReaderAt, info fs.FileInfo) bool {
	if f.offset > info.Size() || (f.offset == info.Size() && !f.modTime.Equal(info.ModTime())) {
		return false
	}

	current := fingerprint(source, f.offset)

	return current != nil && bytes.Equal(current, f.sample)
}

// fingerprint is the first and last bytes before offset, or nil when they cannot be read.
func fingerprint(source io.ReaderAt, offset int64) []byte {
	count := min(offset, tokenPrintBytes)
	data := make([]byte, count+count)
	headErr := readFullAt(source, data[:count], 0)
	tailErr := readFullAt(source, data[count:], offset-count)

	if errors.Join(headErr, tailErr) != nil {
		return nil
	}

	return data
}

// readFullAt fills data from offset, failing on a short read.
func readFullAt(source io.ReaderAt, data []byte, offset int64) error {
	_, err := io.ReadFull(io.NewSectionReader(source, offset, int64(len(data))), data)

	return err //nolint:wrapcheck // The caller only tests it.
}

// read takes in the bytes from the stored offset toward the file size, within the pass budget, and reports
// whether it reached the end with every byte read.
func (f *transcriptTokens) read(source io.ReaderAt, info fs.FileInfo, cutoff time.Time, budget *int64) bool {
	want := info.Size() - f.offset
	limit := min(want, *budget)
	reader := bufio.NewReader(io.NewSectionReader(source, f.offset, limit))
	consumed := int64(0)

	for {
		line, long, count, err := readTokenLine(reader)
		consumed += count
		*budget -= count

		// A last line without its newline counts only when it is whole JSON; otherwise it waits for more bytes.
		if err != nil && !json.Valid(line) {
			break
		}

		f.offset += count

		if !long {
			f.add(line, cutoff)
		}

		if err != nil {
			break
		}
	}

	f.sample, f.modTime = fingerprint(source, f.offset), info.ModTime()

	return limit == want && consumed == limit
}

func (f *transcriptTokens) add(line []byte, cutoff time.Time) {
	var decoded tokenRecord

	message := &decoded.Message

	if json.Unmarshal(line, &decoded) != nil || decoded.Type != "assistant" || message.ID == "" ||
		message.Model == "" || message.Usage == nil || decoded.Timestamp.Before(cutoff) {
		return
	}

	usage := message.Usage
	f.events = append(f.events, tokenEvent{
		Timestamp: decoded.Timestamp, ID: message.ID, Model: message.Model,
		Counts: [4]int64{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite},
	})
}

// totals sums the window the way a full scan of every file would: events inside the window, files in listing
// order, and for a repeated message ID the usage with the latest timestamp, the later line on a tie.
func (x *tokenIndex) totals(start, end time.Time) (map[string][4]int64, bool) {
	x.lock.Lock()
	defer x.lock.Unlock()

	if start.Before(x.cutoff) {
		return nil, false
	}

	latest := map[string]tokenEvent{}

	for _, path := range x.order {
		state, known := x.files[path]
		if !known {
			continue
		}

		for _, event := range state.events {
			previous, repeated := latest[event.ID]
			if event.Timestamp.Before(start) || event.Timestamp.After(end) ||
				(repeated && event.Timestamp.Before(previous.Timestamp)) {
				continue
			}

			latest[event.ID] = event
		}
	}

	totals := map[string][4]int64{}

	for _, event := range latest {
		counts := totals[event.Model]
		for i, count := range event.Counts {
			counts[i] += count
		}

		totals[event.Model] = counts
	}

	return totals, true
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

// tokenPaths lists the transcripts to index and whether the listing is complete. A directory that exists
// but cannot be listed makes it incomplete, so its usage is not silently left out of a total.
func (s TranscriptSource) tokenPaths() ([]string, bool) {
	var paths []string

	listing := &listingFS{FS: s.files, failed: false}

	seen := map[string]bool{}

	for _, name := range s.paths {
		for _, path := range append(mainPaths(listing, name), subagentTree(listing, name)...) {
			if !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
		}
	}

	return paths, !listing.failed
}

// listingFS records the first listing failure other than a missing path.
type listingFS struct {
	fs.FS

	failed bool
}

func (l *listingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	info, err := fs.Stat(l.FS, name)
	if err != nil {
		return nil, l.note(err)
	}

	if !info.IsDir() {
		return nil, fs.ErrInvalid // A file has no entries to list; that is not a listing failure.
	}

	entries, err := fs.ReadDir(l.FS, name)

	return entries, l.note(err)
}

func (l *listingFS) Stat(name string) (fs.FileInfo, error) {
	info, err := fs.Stat(l.FS, name)

	return info, l.note(err)
}

func (l *listingFS) note(err error) error {
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		l.failed = true
	}

	return err
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

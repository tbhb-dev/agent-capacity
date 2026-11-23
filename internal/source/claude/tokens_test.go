// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude //nolint:testpackage // Exercise token scan limits with synthetic files.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	tokenModel  = "model-a"
	tokenModelB = "model-b"
	tokenPath   = "new.jsonl"
	tokenVendor = "anthropic"
)

func tokenLine(id, model string, stamp time.Time, output int) string {
	return fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":%q,"usage":`+
			`{"input_tokens":1,"output_tokens":%d,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}}`,
		stamp.Format(time.RFC3339Nano),
		id,
		model,
		output,
	) + "\n"
}

// planReadings drops the tokens-only readings that windows without a rate-limit event produce.
func planReadings(readings []capacity.Reading) []capacity.Reading {
	return slices.DeleteFunc(slices.Clone(readings), func(reading capacity.Reading) bool { return reading.TokensOnly })
}

// paddedTokenLine inserts size bytes of padding into a token line's message so the line is that long.
func paddedTokenLine(key, line string, size int) string {
	return strings.Replace(line, `"message":{`, `"message":{"`+key+`":"`+strings.Repeat("x", size)+`",`, 1)
}

//nolint:govet // Keep fixture columns in input and expected order.
func TestWindowTokens(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	event := strings.ReplaceAll(transcriptEventLine, "seven_day", "five_hour")
	event = strings.ReplaceAll(event, "200000", "103600")
	first := tokenLine("one", tokenModel, now.Add(-time.Hour), 2)
	duplicate := tokenLine("one", tokenModel, now, 9)

	second := tokenLine("two", tokenModelB, now, 5)
	for _, testCase := range []struct {
		name string
		data string
		old  string
		want map[string][4]int64
	}{
		{
			"two models", event + "\n" + first + second, "",
			map[string][4]int64{tokenModel: {1, 2, 3, 4}, tokenModelB: {1, 5, 3, 4}},
		},
		{"last usage", event + "\n" + first + duplicate, "", map[string][4]int64{tokenModel: {1, 9, 3, 4}}},
		{
			"before start", event + "\n" + tokenLine("old", tokenModel, now.Add(-4*time.Hour-time.Nanosecond), 2),
			"",
			map[string][4]int64{},
		},
		{
			"boundary", event + "\n" + tokenLine("edge", tokenModel, now.Add(-4*time.Hour), 2),
			"",
			map[string][4]int64{tokenModel: {1, 2, 3, 4}},
		},
		{"no reset", first, "", nil},
		{"missing id", event + "\n" + strings.ReplaceAll(first, `"id":"one"`, `"id":""`), "", map[string][4]int64{}},
		{"missing model", event + "\n" + strings.ReplaceAll(first, tokenModel, ""), "", map[string][4]int64{}},
		{
			"missing usage", event + "\n" + `{"type":"assistant","message":{"id":"one","model":"test"}}` + "\n",
			"",
			map[string][4]int64{},
		},
		{
			"long line over 1 MiB", event + "\n" + first + paddedTokenLine("pad", second, 1<<20+1), "",
			map[string][4]int64{tokenModel: {1, 2, 3, 4}, tokenModelB: {1, 5, 3, 4}},
		},
		{"old mtime", event + "\n" + first, second, map[string][4]int64{tokenModel: {1, 2, 3, 4}}},
		{"future", event + "\n" + tokenLine("future", tokenModel, now.Add(time.Second), 2), "", map[string][4]int64{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			files := fstest.MapFS{
				tokenPath:   {Data: []byte(testCase.data), ModTime: now, Mode: 0, Sys: nil},
				"old.jsonl": {Data: []byte(testCase.old), ModTime: now.Add(-9 * 24 * time.Hour), Mode: 0, Sys: nil},
			}
			source := tokenSource(files, newTokenIndex(), []string{".", tokenPath})
			got, err := source.Read(t.Context(), now)
			require.NoError(t, err)

			if testCase.name == "no reset" {
				// Without a rate-limit event both windows carry tokens only.
				require.Len(t, got, 2)
				assert.True(t, got[0].TokensOnly)
				assert.True(t, got[1].TokensOnly)
				assert.Equal(t, "7d", got[1].Window)
				assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}}, got[0].Tokens)

				return
			}

			got = planReadings(got)
			require.Len(t, got, 1)

			assert.Equal(t, testCase.want, got[0].Tokens)
		})
	}
}

func TestTokenWindows(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	// A recent reading supplies both resets; old usage is eligible only for 7d.
	event := `{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{` +
		`"five_hour":{"utilization":0.2,"resetsAt":1003600,"status":"allowed"},` +
		`"seven_day":{"utilization":0.3,"resetsAt":1003600,"status":"allowed"}}}}`
	files := fstest.MapFS{
		"recent": {
			Data:    []byte(event + "\n" + tokenLine("shared", tokenModel, now, 7)),
			ModTime: now,
			Mode:    0,
			Sys:     nil,
		},
		"older": {
			Data:    []byte(tokenLine("week", tokenModelB, now.Add(-24*time.Hour), 5)),
			ModTime: now.Add(-24 * time.Hour), Mode: 0, Sys: nil,
		},
		"duplicate": {
			Data:    []byte(tokenLine("shared", tokenModel, now.Add(-time.Minute), 2)),
			ModTime: now,
			Mode:    0,
			Sys:     nil,
		},
	}
	source := tokenSource(files, newTokenIndex(), []string{"recent", "older", "duplicate"})
	shared := map[string][4]int64{tokenModel: {1, 7, 3, 4}}
	week := map[string][4]int64{tokenModel: {1, 7, 3, 4}, tokenModelB: {1, 5, 3, 4}}
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, shared, got[0].Tokens)
	assert.Equal(t, week, got[1].Tokens)

	// A long malformed line is ignored and the sums stay the same.
	files["older"].Data = append(files["older"].Data, []byte(strings.Repeat("x", 1<<17))...)
	got, err = source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, shared, got[0].Tokens)
	assert.Equal(t, week, got[1].Tokens)
}

func TestTokenScanFailures(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{transcriptInput: {Data: []byte("{}"), Mode: 0, ModTime: time.Time{}, Sys: nil}}
	for _, fixture := range []struct {
		files fs.FS
		path  string
		want  bool
	}{
		{files, "gone", true}, // A transcript that vanished is not a failure; its usage is dropped.
		{files, ".", false},
		{fault(files, "openErr"), transcriptInput, false},
		{fault(files, "statErr"), transcriptInput, false},
		{fault(files, "noReader"), transcriptInput, false},
	} {
		budget := int64(tokenPassBytes)
		assert.Equal(t, fixture.want, newTokenIndex().scan(fixture.files, fixture.path, &budget))
	}
}

// A window that already reset excludes later usage, and a line over the cap is skipped without dropping its neighbors.
func TestTokenLineOverCapAndPastReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	reset := now.Add(-time.Hour)
	big := paddedTokenLine("pad", tokenLine("big", tokenModelB, reset.Add(-2*time.Minute), 5), tokenLineBytes+1<<16)
	data := tokenLine("kept", tokenModel, reset.Add(-3*time.Minute), 2) + big +
		tokenLine("tail", "model-c", reset.Add(-time.Minute), 6) + tokenLine("after", "model-d", now, 7)
	files := fstest.MapFS{tokenPath: {Data: []byte(data), ModTime: now, Mode: 0, Sys: nil}}
	index := newTokenIndex()
	require.True(t, index.refresh(files, []string{tokenPath}, true, now))

	got, ok := windowTokens(index, now, fiveHours(reset, now))
	require.True(t, ok)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}, "model-c": {1, 6, 3, 4}}, got)
}

func TestSubagentTokens(t *testing.T) {
	t.Parallel()

	now, event, file := subagentFixture()
	files := fstest.MapFS{
		tokenPath:                         file(event + "\n" + tokenLine("main", tokenModel, now, 2)),
		"session/subagents/agent-a.jsonl": file(tokenLine("sub", tokenModelB, now, 5)),
		"session/subagents/agent-a.meta":  file(tokenLine("meta", tokenModelB, now, 7)),
		"session/other.jsonl":             file(tokenLine("other", tokenModelB, now, 9)),
	}
	source := tokenSource(files, newTokenIndex(), []string{".", tokenPath})
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)

	got = planReadings(got)
	require.Len(t, got, 1)
	assert.Equal(t, subagentWant(), got[0].Tokens)
}

func TestProjectsRoot(t *testing.T) {
	t.Parallel()

	now, event, file := subagentFixture()
	files := fstest.MapFS{
		"projects/notes.txt":                            file(tokenLine("notes", tokenModel, now, 9)),
		"projects/project-a/s1.jsonl":                   file(event + "\n" + tokenLine("main", tokenModel, now, 2)),
		"projects/project-a/s1/subagents/agent-a.jsonl": file(tokenLine("sub", tokenModelB, now, 5)),
		"projects/project-b/s2.jsonl":                   file(tokenLine("other", tokenModel, now, 3)),
		"projects/s9/subagents/agent-s.jsonl":           file(tokenLine("sess", tokenModelB, now, 4)),
	}
	// A session directory beside the projects is not a project, but its subagents still count.
	want := map[string][4]int64{tokenModel: {2, 5, 6, 8}, tokenModelB: {2, 9, 6, 8}}

	// The projects root, listed twice, reaches each file once.
	source := tokenSource(files, newTokenIndex(), []string{"projects", "projects/project-a"})
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)

	got = planReadings(got)
	require.Len(t, got, 1)
	assert.Equal(t, want, got[0].Tokens)

	// A project directory keeps its own behavior.
	source.paths = []string{"projects/project-a"}
	got, err = source.Read(t.Context(), now)
	require.NoError(t, err)

	got = planReadings(got)
	require.Len(t, got, 1)
	assert.Equal(t, subagentWant(), got[0].Tokens)
}

// subagentFixture returns the shared clock, a five hour window event and a transcript file builder.
func subagentFixture() (time.Time, string, func(string) *fstest.MapFile) {
	now := time.Unix(100000, 0).UTC()
	event := strings.ReplaceAll(transcriptEventLine, "seven_day", "five_hour")
	event = strings.ReplaceAll(event, "200000", "103600")

	return now, event, func(data string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(data), ModTime: now, Mode: 0, Sys: nil}
	}
}

func subagentWant() map[string][4]int64 {
	return map[string][4]int64{tokenModel: {1, 2, 3, 4}, tokenModelB: {1, 5, 3, 4}}
}

func TestTokensOnlyWithoutUsage(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	files := fstest.MapFS{tokenPath: {Data: nil, ModTime: now, Mode: 0, Sys: nil}}
	source := tokenSource(files, newTokenIndex(), []string{tokenPath})
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestTokensOnlyWindows(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	// Usage 6 hours old lies outside the 5h window and inside the 7d window.
	files := fstest.MapFS{tokenPath: {
		Data:    []byte(tokenLine("old", tokenModel, now.Add(-6*time.Hour), 2)),
		ModTime: now, Mode: 0, Sys: nil,
	}}
	source := tokenSource(files, newTokenIndex(), []string{tokenPath})
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "7d", got[0].Window)
	assert.True(t, got[0].TokensOnly)
	assert.Equal(t, now, got[0].ObservedAt)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}}, got[0].Tokens)
	require.NoError(t, got[0].Validate())
}

// fault wraps files so that the one named operation fails.
func fault(files fs.FS, which string) transcriptFaultFS {
	return transcriptFaultFS{
		FS: files, openErr: which == "openErr", statErr: which == "statErr",
		readErr: which == "readErr", noReader: which == "noReader",
	}
}

func tokenSource(files fs.FS, index *tokenIndex, paths []string) TranscriptSource {
	return TranscriptSource{files: files, paths: paths, account: transcriptAccount, index: index}
}

func mapFile(data string, modTime time.Time) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(data), ModTime: modTime, Mode: 0, Sys: nil}
}

// tokensOf refreshes the index over the paths and returns the five hour totals ending now.
func tokensOf(t *testing.T, index *tokenIndex, files fs.FS, now time.Time, paths ...string) map[string][4]int64 {
	t.Helper()

	require.True(t, index.refresh(files, paths, true, now))

	got, ok := windowTokens(index, now, fiveHours(now, now))
	require.True(t, ok)

	return got
}

// Later passes read only appended bytes and keep a half-written line for its newline.
func TestTokenIndexIncremental(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	first := tokenLine("a", tokenModel, now, 2)
	second := tokenLine("b", tokenModel, now, 5)
	files := fstest.MapFS{tokenPath: mapFile(first+second[:10], now)}
	index := newTokenIndex()

	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}}, tokensOf(t, index, files, now, tokenPath))
	assert.Equal(t, int64(len(first)), index.files[tokenPath].offset)

	files[tokenPath] = mapFile(first+second, now)
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 7, 6, 8}}, tokensOf(t, index, files, now, tokenPath))

	// A last line that is whole JSON counts before its newline arrives, and only once.
	files[tokenPath] = mapFile(first+second+strings.TrimSuffix(tokenLine("c", tokenModel, now, 1), "\n"), now)
	want := map[string][4]int64{tokenModel: {3, 8, 9, 12}}
	assert.Equal(t, want, tokensOf(t, index, files, now, tokenPath))
}

// A shrunk or replaced transcript gives up what the old one contributed, whatever its new size.
func TestTokenIndexReplacedFile(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	first := tokenLine("a", tokenModel, now, 2)
	second := tokenLine("b", tokenModel, now, 6)
	index := newTokenIndex()
	files := fstest.MapFS{tokenPath: mapFile(first+second, now)}
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 8, 6, 8}}, tokensOf(t, index, files, now, tokenPath))

	// Truncated to one line.
	files[tokenPath] = mapFile(first, now)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}}, tokensOf(t, index, files, now, tokenPath))

	// Replaced by a file of the same size, then by a larger one.
	other := tokenLine("a", tokenModelB, now, 2)
	files[tokenPath] = mapFile(other, now.Add(time.Second))
	assert.Equal(t, map[string][4]int64{tokenModelB: {1, 2, 3, 4}}, tokensOf(t, index, files, now, tokenPath))

	larger := tokenLine("c", tokenModel, now, 3) + tokenLine("d", tokenModel, now, 4)
	files[tokenPath] = mapFile(larger, now.Add(2*time.Second))
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 7, 6, 8}}, tokensOf(t, index, files, now, tokenPath))
}

// A transcript that left the listing, or fell out of the retention window, stops counting and stops being tracked.
func TestTokenIndexForgetsFiles(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	line := tokenLine("a", tokenModel, now, 2)
	files := fstest.MapFS{tokenPath: mapFile(line, now), "b.jsonl": mapFile(tokenLine("b", tokenModelB, now, 3), now)}
	index := newTokenIndex()
	assert.Len(t, tokensOf(t, index, files, now, tokenPath, "b.jsonl"), 2)

	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}}, tokensOf(t, index, files, now, tokenPath))

	// A path in the listing that disappeared before it was opened is dropped too.
	assert.Empty(t, tokensOf(t, index, files, now, "gone.jsonl"))

	// A file last written before the retention window is forgotten, and old usage in a recent file ages out.
	later := now.Add(tokenRetention + time.Hour)
	files[tokenPath] = mapFile(line, now)
	assert.Empty(t, tokensOf(t, index, files, later, tokenPath))
	assert.Empty(t, index.files)

	files[tokenPath] = mapFile(line, later)
	assert.Empty(t, tokensOf(t, index, files, later, tokenPath))
}

// Windows total the way a full scan does: past resets, updates after the reset, future stamps and ties.
func TestTokenIndexMatchesFullScan(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	reset := now.Add(-time.Hour)
	sevenDays := fiveHours(reset, now)
	sevenDays.Window = "7d"

	// Usage 7 days before a reset that is an hour old is older than 7 days from now, yet inside the window.
	old := tokenLine("old", tokenModel, reset.Add(-7*24*time.Hour+time.Minute), 2)
	// An update after the reset must not displace the usage the window already holds for that id.
	before := tokenLine("u", tokenModel, reset.Add(-time.Minute), 3)
	after := tokenLine("u", tokenModel, reset.Add(time.Minute), 9)
	files := fstest.MapFS{tokenPath: mapFile(old+before+after, now)}
	index := newTokenIndex()
	require.True(t, index.refresh(files, []string{tokenPath}, true, now))

	got, found := windowTokens(index, now, sevenDays)
	require.True(t, found)
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 5, 6, 8}}, got)

	// A record stamped just ahead of the clock counts once the clock catches up.
	ahead := fstest.MapFS{tokenPath: mapFile(tokenLine("ahead", tokenModelB, now.Add(time.Second), 4), now)}
	rolling := newTokenIndex()
	assert.Empty(t, tokensOf(t, rolling, ahead, now, tokenPath))
	caught := tokensOf(t, rolling, ahead, now.Add(2*time.Second), tokenPath)
	assert.Equal(t, map[string][4]int64{tokenModelB: {1, 4, 3, 4}}, caught)

	// Equal stamps: the later line in listing order wins, as in a fresh index over the same files.
	files = fstest.MapFS{
		"a.jsonl": mapFile(tokenLine("t", tokenModel, now, 2), now),
		"b.jsonl": mapFile(tokenLine("t", tokenModel, now, 5)+tokenLine("t", tokenModel, now, 4), now),
	}
	want := map[string][4]int64{tokenModel: {1, 4, 3, 4}}
	assert.Equal(t, want, tokensOf(t, newTokenIndex(), files, now, "a.jsonl", "b.jsonl"))

	// A window that reaches back past what the index keeps reports no totals instead of partial ones.
	far := fiveHours(now.Add(-5*24*time.Hour), now)
	far.Window = "7d"
	_, found = windowTokens(index, now, far)
	assert.False(t, found)
}

// cut serves each file as if it ended after limit bytes, the way a read cut short by an error does.
func cut(files fs.FS, data string, limit int) transcriptProbeFS {
	return transcriptProbeFS{FS: files, mode: 0, readAt: bytes.NewReader([]byte(data)[:limit]).ReadAt}
}

// A read that stops early, or a tail check that fails, leaves the index behind instead of reporting a total.
func TestTokenIndexShortRead(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	first := tokenLine("a", tokenModel, now, 2)
	second := tokenLine("b", tokenModel, now, 5)
	whole := fstest.MapFS{tokenPath: mapFile(first+second, now)}
	index := newTokenIndex()

	short := cut(whole, first+second, len(first)+10)
	assert.False(t, index.refresh(short, []string{tokenPath}, true, now))
	assert.Equal(t, int64(len(first)), index.files[tokenPath].offset)
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 7, 6, 8}}, tokensOf(t, index, whole, now, tokenPath))

	// A file that can no longer be read back where the index left off is read again from the start.
	assert.False(t, index.refresh(cut(whole, first+second, len(first)), []string{tokenPath}, true, now))
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 7, 6, 8}}, tokensOf(t, index, whole, now, tokenPath))

	// A read that errors keeps the pass behind.
	assert.False(t, newTokenIndex().refresh(fault(whole, "readErr"), []string{tokenPath}, true, now))
}

// A listing that is not complete keeps tokens out, and keeps what the index already holds.
func TestTokenListingFailure(t *testing.T) {
	t.Parallel()

	now, event, file := subagentFixture()
	files := fstest.MapFS{
		"projects/p1/s.jsonl": file(event + "\n" + tokenLine("a", tokenModel, now, 2)),
		"projects/p2/s.jsonl": file(tokenLine("b", tokenModelB, now, 5)),
	}
	index := newTokenIndex()
	source := tokenSource(files, index, []string{"projects", "gone"})
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)

	got = planReadings(got)
	require.Len(t, got, 1)
	assert.Len(t, got[0].Tokens, 2)

	// A project directory that cannot be listed leaves its usage out of the totals, so no totals are published.
	source.files = listFaultFS{FS: files, bad: "projects/p2"}
	got, err = source.Read(t.Context(), now)
	require.NoError(t, err)

	got = planReadings(got)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Tokens)

	paths, listed := source.tokenPaths()
	assert.False(t, listed)
	assert.Equal(t, []string{"projects/p1/s.jsonl"}, paths)
}

// listFaultFS fails to list one path with a permission error.
type listFaultFS struct {
	fs.FS

	bad string
}

func (files listFaultFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == files.bad {
		return nil, fs.ErrPermission
	}

	entries, err := fs.ReadDir(files.FS, name)

	return entries, errors.Join(err)
}

// While the index is behind, Read exports the plan readings without tokens instead of partial totals.
func TestReadWithholdsTokensUntilCaughtUp(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	line := tokenLine("a", tokenModel, now, 2)
	big := tokenLine("b", tokenModel, now, 5) + strings.Repeat("\n", 4096)
	files := fstest.MapFS{
		tokenPath: mapFile(line, now),
		"big":     mapFile(big, now),
	}
	index := newTokenIndex()
	index.passBytes = int64(len(line)) + 100
	source := tokenSource(files, index, []string{tokenPath, "big"})

	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	assert.Empty(t, got)

	index.passBytes = tokenPassBytes

	got, err = source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, map[string][4]int64{tokenModel: {2, 7, 6, 8}}, got[0].Tokens)
}

func fiveHours(reset, now time.Time) capacity.Reading {
	return capacity.Reading{
		ResetAt: reset, Window: "5h", ObservedAt: now, Vendor: tokenVendor,
		Bucket: "", Account: transcriptAccount, Source: capacity.ClaudeTranscript, UsedRatio: 0,
	}
}

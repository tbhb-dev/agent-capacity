// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude //nolint:testpackage // Exercise token scan limits with synthetic files.

import (
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
	tokenFile   = "new.jsonl"
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
			"missing usage", event + "\n" + `{"type":"assistant","message":{"id":"one","model":"test"}}`,
			"",
			map[string][4]int64{},
		},
		{
			"long line over 1 MiB", event + "\n" + first + paddedTokenLine("pad", second, 1<<20+1), "",
			map[string][4]int64{tokenModel: {1, 2, 3, 4}, tokenModelB: {1, 5, 3, 4}},
		},
		{"byte cap", event + "\n" + first, "", nil},
		{"old mtime", event + "\n" + first, second, map[string][4]int64{tokenModel: {1, 2, 3, 4}}},
		{"future", event + "\n" + tokenLine("future", tokenModel, now.Add(time.Second), 2), "", map[string][4]int64{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			files := fstest.MapFS{
				tokenFile:   {Data: []byte(testCase.data), ModTime: now, Mode: 0, Sys: nil},
				"old.jsonl": {Data: []byte(testCase.old), ModTime: now.Add(-5 * time.Hour), Mode: 0, Sys: nil},
			}
			source := TranscriptSource{files: files, paths: []string{".", tokenFile}, account: transcriptAccount}
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

			if testCase.name == "byte cap" {
				remaining := int64(1)
				got[0].Tokens = source.windowTokens(now, got[0], &remaining)
			}

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
	source := TranscriptSource{
		files:   files,
		paths:   []string{"recent", "older", "duplicate"},
		account: transcriptAccount,
	}
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
	}{
		{files, "missing"},
		{files, "."},
		{transcriptFaultFS{FS: files, openErr: false, statErr: true, readErr: false, noReader: false}, transcriptInput},
	} {
		source := TranscriptSource{files: fixture.files, account: transcriptAccount, paths: nil}
		remaining := int64(transcriptTokenBytes)
		assert.False(t, source.scanTokens(fixture.path, time.Time{}, time.Time{}, &remaining, map[string]tokenEvent{}))
	}
	// The second eligible file exceeds the shared budget, discarding the first sum.
	source := TranscriptSource{files: files, account: transcriptAccount, paths: []string{transcriptInput, "second"}}
	files["second"] = &fstest.MapFile{Data: []byte("{}"), Mode: 0, ModTime: time.Time{}, Sys: nil}
	remaining := int64(3)
	assert.Nil(t, source.windowTokens(time.Time{}, capacity.Reading{
		ResetAt: time.Time{}.Add(tokenFiveHours), Window: "5h", ObservedAt: time.Time{}, Vendor: tokenVendor,
		Bucket: "", Account: transcriptAccount, Source: capacity.ClaudeTranscript, UsedRatio: 0,
	}, &remaining))
}

// A window that already reset excludes later usage, and a line over the cap is skipped without dropping its neighbors.
func TestTokenLineOverCapAndPastReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	reset := now.Add(-time.Hour)
	big := paddedTokenLine("pad", tokenLine("big", tokenModelB, reset.Add(-2*time.Minute), 5), tokenLineBytes+1<<16)
	data := tokenLine("kept", tokenModel, reset.Add(-3*time.Minute), 2) + big +
		tokenLine("tail", "model-c", reset.Add(-time.Minute), 6) + tokenLine("after", "model-d", now, 7)
	files := fstest.MapFS{tokenFile: {Data: []byte(data), ModTime: now, Mode: 0, Sys: nil}}
	source := TranscriptSource{files: files, paths: []string{tokenFile}, account: transcriptAccount}
	remaining := int64(transcriptTokenBytes)
	got := source.windowTokens(now, capacity.Reading{
		ResetAt: reset, Window: "5h", ObservedAt: now, Vendor: tokenVendor,
		Bucket: "", Account: transcriptAccount, Source: capacity.ClaudeTranscript, UsedRatio: 0,
	}, &remaining)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}, "model-c": {1, 6, 3, 4}}, got)
}

func TestSubagentTokens(t *testing.T) {
	t.Parallel()

	now, event, file := subagentFixture()
	files := fstest.MapFS{
		tokenFile:                         file(event + "\n" + tokenLine("main", tokenModel, now, 2)),
		"session/subagents/agent-a.jsonl": file(tokenLine("sub", tokenModelB, now, 5)),
		"session/subagents/agent-a.meta":  file(tokenLine("meta", tokenModelB, now, 7)),
		"session/other.jsonl":             file(tokenLine("other", tokenModelB, now, 9)),
	}
	source := TranscriptSource{files: files, paths: []string{".", tokenFile}, account: transcriptAccount}
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
	source := TranscriptSource{
		files:   files,
		paths:   []string{"projects", "projects/project-a"},
		account: transcriptAccount,
	}
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
	files := fstest.MapFS{tokenFile: {Data: nil, ModTime: now, Mode: 0, Sys: nil}}
	source := TranscriptSource{files: files, paths: []string{tokenFile}, account: transcriptAccount}
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestTokensOnlyWindows(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	// Usage 6 hours old lies outside the 5h window and inside the 7d window.
	files := fstest.MapFS{tokenFile: {
		Data:    []byte(tokenLine("old", tokenModel, now.Add(-6*time.Hour), 2)),
		ModTime: now, Mode: 0, Sys: nil,
	}}
	source := TranscriptSource{files: files, paths: []string{tokenFile}, account: transcriptAccount}
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "7d", got[0].Window)
	assert.True(t, got[0].TokensOnly)
	assert.Equal(t, now, got[0].ObservedAt)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}}, got[0].Tokens)
	require.NoError(t, got[0].Validate())
}

func TestRollingWindowKeepsBudgetForEvents(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	event := `{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{` +
		`"seven_day":{"utilization":0.3,"resetsAt":1003600,"status":"allowed"}}}}`
	// The reader finds events in the last 8 MiB of a file, so the event sits at the end.
	// Two files near 30 MiB fit the scan budget once, not twice.
	padding := strings.Repeat("\n", 30<<20)
	usage := now.Add(-6 * time.Hour)
	fileA := tokenLine("a", tokenModel, usage, 2) + padding + event + "\n"
	fileB := tokenLine("b", tokenModelB, usage, 5) + padding
	files := fstest.MapFS{
		"a": {Data: []byte(fileA), ModTime: now, Mode: 0, Sys: nil},
		"b": {Data: []byte(fileB), ModTime: now, Mode: 0, Sys: nil},
	}
	source := TranscriptSource{files: files, paths: []string{"a", "b"}, account: transcriptAccount}
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "7d", got[0].Window)
	assert.False(t, got[0].TokensOnly)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 2, 3, 4}, tokenModelB: {1, 5, 3, 4}}, got[0].Tokens)
}

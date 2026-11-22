// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude //nolint:testpackage // Exercise token scan limits with synthetic files.

import (
	"fmt"
	"io/fs"
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

//nolint:govet // Keep fixture columns in input and expected order.
func TestWindowTokens(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0).UTC()
	event := strings.ReplaceAll(transcriptEventLine, "seven_day", "five_hour")
	event = strings.ReplaceAll(event, "200000", "103600")
	first := tokenLine("one", tokenModel, now.Add(-time.Hour), 2)
	duplicate := tokenLine("one", tokenModel, now, 9)

	second := tokenLine("two", "model-b", now, 5)
	for _, testCase := range []struct {
		name string
		data string
		old  string
		want map[string][4]int64
	}{
		{
			"two models", event + "\n" + first + second, "",
			map[string][4]int64{tokenModel: {1, 2, 3, 4}, "model-b": {1, 5, 3, 4}},
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
		{"scanner cap", event + "\n" + first + strings.Repeat("x", tokenScannerBytes+1), "", nil},
		{"line cap", event + "\n" + first + strings.Repeat("x", transcriptLine+1), "", nil},
		{"byte cap", event + "\n" + first, "", nil},
		{"old mtime", event + "\n" + first, second, map[string][4]int64{tokenModel: {1, 2, 3, 4}}},
		{"future", event + "\n" + tokenLine("future", tokenModel, now.Add(time.Second), 2), "", map[string][4]int64{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			files := fstest.MapFS{
				"new.jsonl": {Data: []byte(testCase.data), ModTime: now, Mode: 0, Sys: nil},
				"old.jsonl": {Data: []byte(testCase.old), ModTime: now.Add(-5 * time.Hour), Mode: 0, Sys: nil},
			}
			source := TranscriptSource{files: files, paths: []string{".", "new.jsonl"}, account: transcriptAccount}
			got, err := source.Read(t.Context(), now)
			require.NoError(t, err)

			if testCase.name == "no reset" {
				assert.Empty(t, got)

				return
			}

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
			Data:    []byte(tokenLine("week", "model-b", now.Add(-24*time.Hour), 5)),
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
	got, err := source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 7, 3, 4}}, got[0].Tokens)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 7, 3, 4}, "model-b": {1, 5, 3, 4}}, got[1].Tokens)

	// An oversized old line invalidates only the weekly sum.
	files["older"].Data = append(files["older"].Data, []byte(strings.Repeat("x", tokenScannerBytes+1))...)
	got, err = source.Read(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, map[string][4]int64{tokenModel: {1, 7, 3, 4}}, got[0].Tokens)
	assert.Nil(t, got[1].Tokens)
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

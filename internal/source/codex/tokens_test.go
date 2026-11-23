// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex //nolint:testpackage // Exercise cumulative usage and scan limits with synthetic rollouts.

import (
	"context"
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

const tokenModel = "model-a"

func modelLine(kind, model string, stamp time.Time) string {
	return fmt.Sprintf(`{"type":%q,"timestamp":%q,"payload":{"model":%q,"timestamp":%q}}`+"\n",
		kind, stamp.Format(time.RFC3339Nano), model, stamp.Format(time.RFC3339Nano))
}

func usageLine(stamp time.Time, input, output, cached int) string {
	return fmt.Sprintf(
		`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":{`+
			`"total_token_usage":{"input_tokens":%d,"output_tokens":%d,"cached_input_tokens":%d,`+
			`"cache_write_input_tokens":0,"reasoning_output_tokens":2},"last_usage_includes_reasoning":true,`+
			`"last_token_usage":{"input_tokens":1,"output_tokens":1}}}}`+"\n",
		stamp.Format(time.RFC3339Nano), input, output, cached,
	)
}

func tokenLimits(now time.Time) string {
	return eventLine(now.Format(time.RFC3339Nano), fmt.Sprintf(
		`{"primary":{"used_percent":20,"window_minutes":300,"resets_at":%d},`+
			`"secondary":{"used_percent":30,"window_minutes":10080,"resets_at":%d}}`,
		now.Add(time.Hour).Unix(), now.Add(time.Hour).Unix(),
	)) + "\n"
}

//nolint:govet // Keep fixture columns in input and expected order.
func TestWindowTokens(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	start := now.Add(-4 * time.Hour)
	model := modelLine("session_meta", tokenModel, start)
	first := usageLine(start, 100, 10, 40)
	last := usageLine(now, 160, 16, 60)
	limits := tokenLimits(now)
	// A resumed session can have a newer envelope than its metadata start.
	crossing := strings.Replace(modelLine("session_meta", tokenModel, start.Add(-time.Hour)),
		start.Add(-time.Hour).Format(time.RFC3339Nano), start.Format(time.RFC3339Nano), 1)

	for _, testCase := range []struct {
		name string
		data string
		want map[string][4]int64
	}{
		{
			"two models", limits + model + first + modelLine("turn_context", "model-b", now) + last,
			map[string][4]int64{tokenModel: {60, 10, 40, 0}, "model-b": {40, 6, 20, 0}},
		},
		{
			"cumulative repeated", limits + model + first + first + last + last,
			map[string][4]int64{tokenModel: {100, 16, 60, 0}},
		},
		{
			"crossing start", limits + modelLine("session_meta", tokenModel, start.Add(-time.Hour)) +
				usageLine(start.Add(-time.Second), 70, 7, 30) + first + last,
			map[string][4]int64{tokenModel: {40, 6, 20, 0}},
		},
		{
			"crossing first only", limits + crossing + first,
			map[string][4]int64{},
		},
		{
			"before start", limits + modelLine("session_meta", tokenModel, start.Add(-time.Hour)) +
				usageLine(start.Add(-time.Nanosecond), 100, 10, 40),
			map[string][4]int64{},
		},
		{"no valid reading", model + first, nil},
		{"no model", limits + first, map[string][4]int64{}},
		{"no reset", eventLine(now.Format(time.RFC3339Nano), short) + "\n" + model + first, nil},
		{
			// The rate tail reads only the last 8 MiB, so the limits line follows the long one.
			"long line skipped", model + first + strings.Repeat("x", tokenLineBytes+8192) + "\n" + limits + last,
			map[string][4]int64{tokenModel: {100, 16, 60, 0}},
		},
		{
			"long line kept", limits + model + first + strings.Repeat("x", 3*chunkSize) + "\n" + last,
			map[string][4]int64{tokenModel: {100, 16, 60, 0}},
		},
		{
			"future excluded", limits + model + first + usageLine(now.Add(time.Second), 160, 16, 60),
			map[string][4]int64{tokenModel: {60, 10, 40, 0}},
		},
		{
			"context window overflow", limits + model + first + fmt.Sprintf(
				`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":{`+
					`"total_token_usage":{"total_tokens":128000,"input_tokens":0,"output_tokens":0,`+
					`"cached_input_tokens":0,"cache_write_input_tokens":0},"model_context_window":128000}}}`+"\n",
				now.Format(time.RFC3339Nano),
			),
			map[string][4]int64{tokenModel: {60, 10, 40, 0}},
		},
		{
			"turn context", limits + modelLine("session_meta", "", start) +
				`{"type":"turn_context","payload":{"model":"model-a"}}` + "\nmalformed\n{}\n" +
				`{"type":"event_msg","payload":{"type":"other"}}` + "\n" + first,
			map[string][4]int64{tokenModel: {60, 10, 40, 0}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			writeRollout(t, home, rollout, testCase.data)
			registry, err := Register(home, primaryAccount)
			require.NoError(t, err)
			readings, failures := registry.Read(t.Context(), now)
			require.Empty(t, failures)

			if testCase.name == "no valid reading" {
				assert.Empty(t, readings)

				return
			}

			require.NotEmpty(t, readings)
			assert.Equal(t, testCase.want, readings[0].Tokens)
		})
	}
}

func TestWindowTokensAtReset(t *testing.T) {
	t.Parallel()

	reset := time.Unix(1000000, 0).UTC()
	now := reset.Add(time.Minute)
	data := modelLine("session_meta", tokenModel, reset) + usageLine(reset, 100, 10, 40) +
		usageLine(now, 160, 16, 60)
	files := fstest.MapFS{rollout: {Data: []byte(data), ModTime: now, Mode: 0, Sys: nil}}
	source := Source{files: files, account: primaryAccount}
	remaining := int64(capacity.WindowTokenBytes)

	assert.Equal(t, map[string][4]int64{tokenModel: {60, 10, 40, 0}},
		source.windowTokens(t.Context(), now, wantReading("5h", 0.2, reset), &remaining))
}

func TestWindowTokensWithFork(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	forkedAt := now.Add(-time.Minute)
	parent := modelLine("session_meta", tokenModel, forkedAt) + usageLine(forkedAt, 100, 10, 40)
	fork := parent + usageLine(now, 160, 16, 60)
	files := fstest.MapFS{
		"sessions/2026/01/02/rollout-1.jsonl": {Data: []byte(parent), ModTime: forkedAt, Mode: 0, Sys: nil},
		"sessions/2026/01/02/rollout-2.jsonl": {Data: []byte(fork), ModTime: now, Mode: 0, Sys: nil},
	}
	source := Source{files: files, account: primaryAccount}
	remaining := int64(capacity.WindowTokenBytes)

	assert.Equal(t, map[string][4]int64{tokenModel: {100, 16, 60, 0}},
		source.windowTokens(t.Context(), now, wantReading("5h", 0.2, now.Add(time.Hour)), &remaining))
}

func TestTokenFailures(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	files := fstest.MapFS{rollout: {Data: []byte("{}\n"), ModTime: now, Mode: 0, Sys: nil}}
	reading := wantReading("5h", 0.2, now.Add(time.Hour))

	for _, broken := range []fs.FS{
		faultFS{FS: nil, err: fs.ErrPermission},
		infoFailureFS{FS: files},
	} {
		remaining := int64(capacity.WindowTokenBytes)
		source := Source{files: broken, account: primaryAccount}
		assert.Nil(t, source.windowTokens(t.Context(), now, reading, &remaining))
	}

	var totals windowTokenTotals

	for _, broken := range []fs.FS{
		faultFS{FS: nil, err: fs.ErrPermission},
		oneFileFS{FS: nil, file: faultFile{File: nil, err: fs.ErrPermission}},
	} {
		remaining := int64(capacity.WindowTokenBytes)
		source := Source{files: broken, account: primaryAccount}
		assert.False(t, source.scanTokens(t.Context(), rollout, now, now, &remaining, totals))
	}

	source := Source{files: files, account: primaryAccount}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	remaining := int64(capacity.WindowTokenBytes)
	assert.False(t, source.scanTokens(ctx, rollout, now, now, &remaining, totals))

	files[rollout].Mode = fs.ModeSymlink

	assert.Empty(t, source.windowTokens(t.Context(), now, reading, &remaining))
}

func TestTokenWindowsAndBudget(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000000, 0).UTC()
	data := modelLine("session_meta", tokenModel, now) + usageLine(now, 100, 10, 40)
	files := fstest.MapFS{}

	for index := range 6 {
		path := fmt.Sprintf("sessions/2026/01/02/rollout-%d.jsonl", index)
		stamp := now.Add(-time.Duration(index) * time.Second)
		distinct := modelLine("session_meta", tokenModel, stamp) + usageLine(stamp, 100, 10, 40)
		files[path] = &fstest.MapFile{Data: []byte(distinct), ModTime: now, Mode: 0, Sys: nil}
	}

	old := "sessions/2026/01/01/rollout-old.jsonl"
	files[old] = &fstest.MapFile{
		Data: []byte(modelLine("session_meta", "model-b", now.Add(-24*time.Hour)) +
			usageLine(now.Add(-24*time.Hour), 50, 5, 20)),
		ModTime: now.Add(-24 * time.Hour), Mode: 0, Sys: nil,
	}
	source := Source{files: files, account: primaryAccount}
	reading := wantReading("5h", 0.2, now.Add(time.Hour))
	remaining := int64(6 * len(data))
	assert.Equal(t, map[string][4]int64{tokenModel: {360, 60, 240, 0}},
		source.windowTokens(t.Context(), now, reading, &remaining))
	assert.Zero(t, remaining)

	reading.Window = "7d"
	assert.Nil(t, source.windowTokens(t.Context(), now, reading, &remaining))
	remaining = capacity.WindowTokenBytes
	assert.Equal(t, map[string][4]int64{tokenModel: {360, 60, 240, 0}, "model-b": {30, 5, 20, 0}},
		source.windowTokens(t.Context(), now, reading, &remaining))

	// A later eligible file exceeds the budget and discards the earlier sums.
	remaining = int64(len(data) + 1)
	assert.Nil(t, source.windowTokens(t.Context(), now, reading, &remaining))

	// Files with old modification times never consume the byte budget.
	files[old].Data = []byte(strings.Repeat("x", 7*len(data)))
	remaining = int64(6 * len(data))
	reading.Window = "5h"
	assert.NotNil(t, source.windowTokens(t.Context(), now, reading, &remaining))

	remaining = int64(6 * len(data))
	reading.Window = "7d"
	assert.Nil(t, source.windowTokens(t.Context(), now, reading, &remaining))
}

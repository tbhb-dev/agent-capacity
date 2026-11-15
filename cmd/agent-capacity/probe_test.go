// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

const usageFixture = "testdata/agy-usage.json"

func fixed(output []byte, err error) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		if _, found := ctx.Deadline(); !found {
			return nil, errors.ErrUnsupported
		}

		return output, err
	}
}

func TestProbeUsage(t *testing.T) {
	t.Parallel()

	output, err := os.ReadFile(usageFixture)
	require.NoError(t, err)

	dir := t.TempDir()
	observed := time.Unix(100000, 0).UTC()
	require.NoError(t, probeUsage(t.Context(), []string{"--spool-dir=" + dir, workFlag}, fixed(output, nil), observed))

	stored, err := os.ReadFile(filepath.Join(dir, "agy-work.json"))
	require.NoError(t, err)

	for _, discarded := range []string{"conversation_id", "response", "num_turns", "SUCCESS"} {
		assert.NotContains(t, string(stored), discarded)
	}

	snapshots, failures, err := spool.Read(dir, "agy")
	require.NoError(t, err)
	assert.Empty(t, failures)
	require.Len(t, snapshots, 1)
	assert.Equal(t, observed, snapshots[0].ObservedAt)
	assert.Contains(t, string(snapshots[0].Data), `"gemini-5h"`)

	readings, readFailures := newRuntime(t.Context(), slog.New(slog.DiscardHandler)).Registry(dir).Read(
		t.Context(), observed,
	)
	assert.Empty(t, readFailures)
	assert.Len(t, readings, 4)
}

func TestProbeUsageErrors(t *testing.T) {
	t.Parallel()

	valid, err := os.ReadFile(usageFixture)
	require.NoError(t, err)

	renamed := bytes.Replace(valid, []byte(`"name": "usage"`), []byte(`"name": "quota"`), 1)
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	for _, testCase := range []struct {
		usage func(context.Context) ([]byte, error)
		want  error
		args  []string
	}{
		{fixed(valid, nil), capacity.ErrInvalidReading, []string{"--account=@"}},
		{fixed(valid, nil), capacity.ErrInvalidReading, []string{unknownFlag}},
		{fixed(valid, nil), capacity.ErrInvalidReading, []string{workFlag, "extra"}},
		{fixed(valid, nil), spool.ErrDirectory, []string{workFlag, "--spool-dir="}},
		{fixed(nil, os.ErrNotExist), os.ErrNotExist, nil},
		{fixed([]byte(`{`), nil), capacity.ErrInvalidReading, nil},
		{fixed(bytes.Replace(valid, []byte(`"SUCCESS"`), []byte(`"ERROR"`), 1), nil), capacity.ErrInvalidReading, nil},
		{fixed(renamed, nil), capacity.ErrInvalidReading, nil},
		{fixed([]byte(`{"status":"SUCCESS","command":{"name":"usage"}}`), nil), capacity.ErrInvalidReading, nil},
		{fixed([]byte(`{"status":"SUCCESS","command":{"name":"usage","data":null}}`), nil), capacity.ErrInvalidReading, nil},
		{fixed([]byte(`{"status":"SUCCESS","command":{"name":"usage","data":{}}}`), nil), capacity.ErrInvalidReading, nil},
		{fixed(append(valid, bytes.Repeat([]byte(" "), inputLimit)...), nil), capacity.ErrInvalidReading, nil},
		{fixed(valid, nil), os.ErrExist, []string{workFlag, "--spool-dir=" + blocked}},
	} {
		args := testCase.args
		if args == nil {
			args = []string{workFlag, "--spool-dir=" + t.TempDir()}
		}

		got := probeUsage(t.Context(), args, testCase.usage, time.Now())
		if errors.Is(testCase.want, os.ErrExist) {
			require.Error(t, got)
			require.NotErrorIs(t, got, capacity.ErrInvalidReading)

			continue
		}

		require.ErrorIs(t, got, testCase.want, args)
	}
}

func TestProbeUsageKeepsSnapshot(t *testing.T) {
	t.Parallel()

	valid, err := os.ReadFile(usageFixture)
	require.NoError(t, err)

	dir := t.TempDir()
	args := []string{"--spool-dir=" + dir, workFlag}
	require.NoError(t, probeUsage(t.Context(), args, fixed(valid, nil), time.Now()))

	before, err := os.ReadFile(filepath.Join(dir, "agy-work.json"))
	require.NoError(t, err)

	empty := []byte(`{"status":"SUCCESS","command":{"name":"usage","data":null}}`)
	require.ErrorIs(t, probeUsage(t.Context(), args, fixed(empty, nil), time.Now()), capacity.ErrInvalidReading)

	after, err := os.ReadFile(filepath.Join(dir, "agy-work.json"))
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestProbe(t *testing.T) {
	t.Setenv("AGENT_CAPACITY_SPOOL_DIR", "")

	valid, err := os.ReadFile(usageFixture)
	require.NoError(t, err)

	for _, testCase := range []struct {
		message string
		args    []string
		code    int
	}{
		{"", []string{workFlag, "--spool-dir=" + t.TempDir()}, 0},
		{"spool directory is required", []string{workFlag}, 1},
		{"probe failed", []string{unknownFlag}, 1},
	} {
		var logs bytes.Buffer

		assert.Equal(t, testCase.code, probe(
			t.Context(), testCase.args, fixed(valid, nil), slog.New(slog.NewJSONHandler(&logs, nil)),
		))

		if testCase.message == "" {
			assert.Empty(t, logs.String())
		} else {
			assert.Contains(t, logs.String(), `"msg":"`+testCase.message+`"`)
		}
	}

	var out, logs bytes.Buffer

	dir := t.TempDir()
	runtime := Runtime{Registry: nil, NewRegistry: nil, NewProvider: nil, Usage: fixed(valid, nil)}
	assert.Equal(t, 0, runtime.Run(
		t.Context(), []string{"probe", workFlag, "--spool-dir=" + dir}, &out, slog.New(slog.NewJSONHandler(&logs, nil)),
	))
	assert.Empty(t, out.String())
	assert.FileExists(t, filepath.Join(dir, "agy-work.json"))
}

// TestRunUsage runs a stand-in script on PATH, never the real Antigravity command.
func TestRunUsage(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' \"$*\"\n"
	stub := filepath.Join(bin, "agy")
	//nolint:gosec // The stand-in must be executable.
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o700))
	t.Setenv("PATH", bin)

	output, err := runUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "-p /usage --output-format json --print-timeout 100s", strings.TrimSpace(string(output)))

	//nolint:gosec // The stand-in must be executable.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nexit 3\n"), 0o700))

	output, err = runUsage(t.Context())
	require.ErrorContains(t, err, "run usage command")
	assert.Equal(t, "read", capacity.ErrorClass(err))
	assert.Nil(t, output)

	//nolint:gosec // The stand-in must be executable.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o700))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err = runUsage(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	t.Setenv("PATH", t.TempDir())

	_, err = runUsage(t.Context())
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Equal(t, "missing", capacity.ErrorClass(err))
}

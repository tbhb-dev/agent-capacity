// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	usageFixture = "testdata/agy-usage.json"
	// The probe gives the usage command two minutes. Spelled out here so a changed constant fails the tests.
	usageDeadline = 2 * time.Minute
)

func fixed(output []byte, err error) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		deadline, found := ctx.Deadline()
		if left := time.Until(deadline); !found || left > usageDeadline || left < usageDeadline-time.Minute {
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

	padded := append(bytes.Clone(output), bytes.Repeat([]byte(" "), inputLimit-len(output))...)
	require.NoError(t, probeUsage(t.Context(), []string{"--spool-dir=" + dir, workFlag}, fixed(padded, nil), observed))
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
		{fixed(nil, os.ErrNotExist), spool.ErrDirectory, []string{workFlag, "--spool-dir="}},
		{fixed(nil, os.ErrNotExist), os.ErrNotExist, nil},
		{fixed([]byte(`{`), nil), capacity.ErrInvalidReading, nil},
		{fixed(bytes.Replace(valid, []byte(`"SUCCESS"`), []byte(`"ERROR"`), 1), nil), capacity.ErrInvalidReading, nil},
		{fixed(renamed, nil), capacity.ErrInvalidReading, nil},
		{fixed([]byte(`{"status":"SUCCESS","command":{"name":"usage"}}`), nil), capacity.ErrInvalidReading, nil},
		{fixed([]byte(`{"status":"SUCCESS","command":{"name":"usage","data":null}}`), nil), capacity.ErrInvalidReading, nil},
		{fixed([]byte(`{"status":"SUCCESS","command":{"name":"usage","data":{}}}`), nil), capacity.ErrInvalidReading, nil},
		{fixed(append(valid, bytes.Repeat([]byte(" "), inputLimit)...), nil), capacity.ErrInvalidReading, nil},
		{fixed(valid, nil), os.ErrExist, []string{workFlag, "--spool-dir=" + blocked}},
		{nil, capacity.ErrInvalidReading, nil},
	} {
		args := testCase.args
		if args == nil {
			args = []string{workFlag, "--spool-dir=" + t.TempDir()}
		}

		got := probeUsage(t.Context(), args, testCase.usage, time.Now())
		if errors.Is(testCase.want, os.ErrExist) {
			var pathErr *fs.PathError
			require.ErrorAs(t, got, &pathErr)
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

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
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
	require.ErrorIs(t, err, exec.ErrNotFound)
	assert.Equal(t, "missing", capacity.ErrorClass(err))
}

func TestUsageCommand(t *testing.T) {
	t.Parallel()

	command := usageCommand(t.Context())
	assert.Equal(t, 5*time.Second, command.WaitDelay)
	assert.Equal(t, []string{"agy", "-p", "/usage", "--output-format", "json", "--print-timeout", "100s"}, command.Args)
}

// TestRunCommandChild checks that a child process holding the output pipe open can't stretch the deadline. The shell
// reads the stand-in as a file: running a file this test just wrote can fail with "text file busy" while a parallel
// test's fork still holds the write descriptor.
func TestRunCommandChild(t *testing.T) {
	t.Parallel()

	stub := filepath.Join(t.TempDir(), "agy")
	require.NoError(t, os.WriteFile(stub, []byte("/bin/sleep 3 &\nwait\n"), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	command := exec.CommandContext(ctx, "/bin/sh", stub)
	command.WaitDelay = 100 * time.Millisecond
	start := time.Now()

	_, err := runCommand(ctx, command)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestRunCommandLimit(t *testing.T) {
	t.Parallel()

	command := exec.CommandContext(t.Context(), "/usr/bin/head", "-c", "1048600", "/dev/zero")
	output, err := runCommand(t.Context(), command)
	require.NoError(t, err)
	assert.Len(t, output, inputLimit+1)
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()

	buffer := &limitedBuffer{data: nil, limit: 4}
	for _, chunk := range []string{"abc", "def", "g"} {
		written, err := buffer.Write([]byte(chunk))
		require.NoError(t, err)
		assert.Equal(t, len(chunk), written)
	}

	assert.Equal(t, "abcd", string(buffer.data))
}

// TestProbeProcess checks the exit code and that a bad flag logs one line and no usage text.
func TestProbeProcess(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)

	command := exec.CommandContext(t.Context(), executable, "probe", unknownFlag)

	command.Env = append(os.Environ(), processTest+"=1")

	var out, logs bytes.Buffer

	command.Stdout = &out
	command.Stderr = &logs

	var exitErr *exec.ExitError
	require.ErrorAs(t, command.Run(), &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Empty(t, out.String())

	decoder := json.NewDecoder(&logs)

	var entry map[string]any
	require.NoError(t, decoder.Decode(&entry))
	assert.Equal(t, "probe failed", entry["msg"])
	assert.Equal(t, invalidClass, entry["class"])

	var trailing any
	require.ErrorIs(t, decoder.Decode(&trailing), io.EOF)
}

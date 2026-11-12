// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	claudeVendor   = "claude"
	claudeFlag     = "--vendor=claude"
	workFlag       = "--account=work"
	emptyLimits    = `{"rate_limits":{}}`
	inputLimit     = 1048576
	unknownFlag    = "--unknown"
	captureFailure = "statusline capture failed"
)

func TestCapture(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct{ vendor, input, want string }{
		{
			claudeVendor,
			`{"rate_limits":{"seven_day":{"used_percentage":10,"resets_at":200000}},"private":"discard"}`,
			`{"seven_day":{"used_percentage":10,"resets_at":200000}}`,
		},
		{"agy", `{"quota":{"remaining":25},"rate_limits":{"discard":true},"private":"discard"}`, `{"remaining":25}`},
		{claudeVendor, `{"rate_limits":null}`, `null`},
		{claudeVendor, `{"private":"discard"}`, ""},
		{"agy", emptyLimits, ""},
		{claudeVendor, `null`, ""},
	} {
		dir := filepath.Join(t.TempDir(), "spool")
		now := time.Unix(100000, 0).In(time.FixedZone("offset", 3600))
		require.NoError(
			t,
			capture(
				[]string{"--vendor=" + testCase.vendor, workFlag, "--spool-dir=" + dir},
				strings.NewReader(testCase.input),
				now,
			),
		)
		got, failures, err := spool.Read(dir, testCase.vendor)
		require.NoError(t, err)
		assert.Empty(t, failures)

		if testCase.want == "" {
			assert.Nil(t, got)

			_, err = os.Stat(dir)
			require.ErrorIs(t, err, os.ErrNotExist)

			continue
		}

		require.Len(t, got, 1)
		assert.Equal(t, testCase.vendor, got[0].Vendor)
		assert.Equal(t, "work", got[0].Account)
		assert.Equal(t, now.UTC(), got[0].ObservedAt)
		assert.JSONEq(t, testCase.want, string(got[0].Data))
		data, err := os.ReadFile(filepath.Join(dir, testCase.vendor+"-work.json"))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "private")
		assert.NotContains(t, string(data), "discard")
	}
}

type failedInput struct{}

func (failedInput) Read([]byte) (int, error) { return 0, errors.ErrUnsupported }

func TestCaptureFailures(t *testing.T) {
	t.Setenv("AGENT_CAPACITY_SPOOL_DIR", "")
	dir := t.TempDir()

	args := []string{claudeFlag, workFlag, "--spool-dir=" + dir}
	for _, input := range []io.Reader{
		strings.NewReader(`{`), strings.NewReader(`[]`), strings.NewReader(`{} {}`),
		strings.NewReader(strings.Repeat(" ", inputLimit+1)),
	} {
		require.ErrorIs(t, capture(args, input, time.Now()), capacity.ErrInvalidReading)
	}

	for _, invalidArgs := range [][]string{
		{unknownFlag},
		{claudeFlag, workFlag, "extra"},
		{"--vendor=invalid", workFlag},
		{claudeFlag, "--account=work@"},
	} {
		require.ErrorIs(
			t,
			capture(append([]string{"--spool-dir=" + dir}, invalidArgs...), strings.NewReader(emptyLimits), time.Now()),
			capacity.ErrInvalidReading,
		)
	}

	require.ErrorIs(t, capture(args, failedInput{}, time.Now()), errors.ErrUnsupported)

	blocked := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	var cause *os.PathError

	require.ErrorAs(t, capture(
		[]string{claudeFlag, workFlag, "--spool-dir=" + blocked},
		strings.NewReader(emptyLimits), time.Now(),
	), &cause)
	require.NoError(t, os.Remove(blocked))

	minimal := []string{claudeFlag, workFlag}
	require.ErrorIs(t, capture(minimal, strings.NewReader(`{}`), time.Now()), spool.ErrDirectory)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)

	boundary := emptyLimits
	boundary += strings.Repeat(" ", inputLimit-len(boundary))
	require.NoError(t, capture(args, strings.NewReader(boundary), time.Now()))
	oversized := bytes.NewBufferString(boundary + "  ")
	require.ErrorIs(t, capture(args, oversized, time.Now()), capacity.ErrInvalidReading)
	assert.Equal(t, 1, oversized.Len())
}

func TestSpoolConfiguration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_CAPACITY_SPOOL_DIR", dir)

	_, got, err := parseServe([]string{serveCommand})
	require.NoError(t, err)
	assert.Equal(t, dir, got)
	_, got, err = parseServe([]string{serveCommand, "--spool-dir=explicit"})
	require.NoError(t, err)
	assert.Equal(t, "explicit", got)
	require.NoError(
		t,
		capture([]string{"--vendor=agy", workFlag}, strings.NewReader(`{"quota":{}}`), time.Now()),
	)

	gotSnapshots, _, err := spool.Read(dir, "agy")
	require.NoError(t, err)
	require.Len(t, gotSnapshots, 1)
	t.Setenv("AGENT_CAPACITY_SPOOL_DIR", "")

	var out, logs bytes.Buffer

	runtime := Runtime{Registry: nil, NewProvider: nil}
	assert.Equal(
		t,
		1,
		runtime.Run(t.Context(), []string{serveCommand}, &out, slog.New(slog.NewJSONHandler(&logs, nil))),
	)
	assert.Empty(t, out.String())
	assert.Contains(t, logs.String(), `"msg":"spool directory is required"`)
}

func TestStatuslineProcess(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	for _, testCase := range []struct {
		input, message, class string
		args                  []string
	}{
		{args: []string{claudeFlag, workFlag, "--spool-dir=" + t.TempDir()}, input: emptyLimits, message: "", class: ""},
		{args: []string{"--vendor=agy", workFlag, "--spool-dir=" + t.TempDir()}, input: `{}`, message: "", class: ""},
		{
			args: []string{claudeFlag, workFlag, "--spool-dir=" + t.TempDir()}, input: `{`,
			message: captureFailure, class: invalidClass,
		},
		{
			args: []string{claudeFlag, "--account=@", "--spool-dir=" + t.TempDir()}, input: `{}`,
			message: captureFailure, class: invalidClass,
		},
		{
			args: []string{unknownFlag}, input: `{}`,
			message: captureFailure, class: invalidClass,
		},
		{args: []string{claudeFlag, workFlag}, input: `{}`, message: "spool directory is required", class: readClass},
		{
			args: []string{claudeFlag, workFlag, "--spool-dir=" + blocked}, input: emptyLimits,
			message: captureFailure, class: readClass,
		},
	} {
		command := exec.CommandContext(t.Context(), executable, append([]string{"statusline"}, testCase.args...)...)
		command.Env = append(os.Environ(), processTest+"=1", "AGENT_CAPACITY_SPOOL_DIR=")
		command.Stdin = strings.NewReader(testCase.input)

		var out, logs bytes.Buffer

		command.Stdout = &out
		command.Stderr = &logs
		require.NoError(t, command.Run())
		assert.Empty(t, out.String())

		if testCase.message == "" {
			assert.Empty(t, logs.String())

			continue
		}

		decoder := json.NewDecoder(&logs)

		var entry map[string]any
		require.NoError(t, decoder.Decode(&entry))
		assert.Equal(t, testCase.message, entry["msg"])
		assert.Equal(t, testCase.class, entry["class"])
		assert.Len(t, entry, 4)

		var trailing any
		require.ErrorIs(t, decoder.Decode(&trailing), io.EOF)
	}
}

func TestStatusline(t *testing.T) {
	t.Setenv("AGENT_CAPACITY_SPOOL_DIR", "")

	for _, args := range [][]string{
		{claudeFlag, workFlag, "--spool-dir=" + t.TempDir()},
		{claudeFlag, workFlag},
		{unknownFlag},
	} {
		var logs bytes.Buffer

		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		assert.Equal(t, 0, statusline(t.Context(), args, strings.NewReader(emptyLimits), logger))
	}

	var out, logs bytes.Buffer

	runtime := Runtime{Registry: nil, NewProvider: nil}
	assert.Equal(
		t,
		0,
		runtime.Run(t.Context(), []string{"statusline"}, &out, slog.New(slog.NewJSONHandler(&logs, nil))),
	)
	assert.Empty(t, out.String())
	assert.Contains(t, logs.String(), `"msg":"`+captureFailure+`"`)
}

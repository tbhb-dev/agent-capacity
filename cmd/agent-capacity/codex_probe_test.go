//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"encoding/json"
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

//nolint:lll // Keep the complete server response readable.
const codexAnswer = `{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":200000},"secondary":{"usedPercent":50,"windowDurationMins":10080,"resetsAt":300000}}`

func fakeCodex(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	//nolint:gosec // The stand-in must be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"+script), 0o700))
	t.Setenv("PATH", bin)
}

//nolint:paralleltest // fakeCodex changes PATH.
func TestCodexProbe(t *testing.T) {
	fakeCodex(t, `test "$*" = app-server || exit 3
read -r init
expected='{"id":1,"method":"initialize","params":{"clientInfo":{"name":"agent-capacity","version":"0.0.0"}}}'
[ "$init" = "$expected" ] || exit 4
printf '%s\n' '{"id":1,"result":{}}'
read -r ready
[ "$ready" = '{"method":"initialized"}' ] || exit 5
read -r request
[ "$request" = '{"id":2,"method":"account/rateLimits/read"}' ] || exit 6
printf '%s\n' '{"method":"notice"}' '{"id":2,"result":{"rateLimits":`+codexAnswer+`},"private":"discard"}'
read -r held
`)
	dir := t.TempDir()
	now := time.Unix(100000, 0).UTC()
	args := []string{"--vendor=codex", "--account=work", "--spool-dir=" + dir}
	require.NoError(t, probeUsage(t.Context(), args, nil, now))

	snapshots, failures, err := spool.Read(dir, "codex")
	require.NoError(t, err)
	assert.Empty(t, failures)
	require.Len(t, snapshots, 1)
	assert.JSONEq(t, codexAnswer, string(snapshots[0].Data))
	assert.Equal(t, now, snapshots[0].ObservedAt)
	readings, failuresRead := newRuntime(
		t.Context(),
		slog.New(slog.DiscardHandler),
	).Registry(dir).
		Read(t.Context(), now)
	assert.Empty(t, failuresRead)
	require.Len(t, readings, 2)
	assert.Equal(t, "work", readings[0].Account)
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	require.Error(t, probeCodex(t.Context(), blocked, "work", now))
}

func TestCodexProbeErrors(t *testing.T) {
	fakeCodex(t, `read -r init
printf '%s\n' '{"id":1,"result":{}}'
read -r ready
read -r request
printf '%s\n' '{"id":2,"result":{"rateLimits":{}}}'
read -r held
`)

	require.ErrorIs(t, probeCodex(t.Context(), t.TempDir(), "work", time.Now()), capacity.ErrInvalidReading)

	for _, vendor := range []string{"claude", "unknown"} {
		require.ErrorIs(
			t,
			probeUsage(t.Context(), []string{"--vendor=" + vendor, workFlag}, nil, time.Now()),
			capacity.ErrInvalidReading,
		)
	}

	fakeCodex(t, "read -r init\nexec /bin/sleep 10\n")

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := runCodexUsage(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	fakeCodex(t, "exit 3\n")
	require.Error(t, probeCodex(t.Context(), t.TempDir(), "work", time.Now()))

	t.Setenv("PATH", t.TempDir())
	_, err = runCodexUsage(t.Context())
	require.ErrorIs(t, err, exec.ErrNotFound)
	command := exec.CommandContext(t.Context(), "unused")
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	_, err = runCodexCommand(command)
	require.ErrorContains(t, err, "already set")
	assert.Equal(t, 30*time.Second, codexProbeTimeout)
}

func TestCodexExchange(t *testing.T) {
	t.Parallel()

	for _, response := range []string{
		`{`,
		`{"id":1,"error":{"message":"private"}}`,
		`{"id":1,"result":{}}` + "\n" + `{"id":2,"result":[]}`,
	} {
		_, err := codexExchange(io.Discard, json.NewDecoder(strings.NewReader(response)))
		require.Error(t, err)
	}

	_, err := codexExchange(failingWriter{}, json.NewDecoder(bytes.NewReader(nil)))
	require.Error(t, err)
}

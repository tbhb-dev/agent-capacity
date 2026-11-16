// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const codexData = `{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":200000},"secondary":{"usedPercent":50,"windowDurationMins":10080,"resetsAt":300000}}`

func fakeCodex(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	//nolint:gosec // The fake command must be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"+script), 0o700))
	t.Setenv("PATH", bin)
}

func TestCodexProbe(t *testing.T) {
	fakeCodex(t, `IFS= read -r initialize
IFS= read -r initialized
IFS= read -r request
[ "$initialize
$initialized
$request
" = '`+codexHandshake+`' ] || exit 2
printf '%s\n' '{"id":1,"result":{}}' '{"method":"notice"}'
/bin/sleep 0.05
printf '%s\n' '{"id":2,"result":{"rateLimits":`+codexData+`}}'
IFS= read -r end
`)
	dir := t.TempDir()
	now := time.Unix(100000, 0).UTC()
	args := []string{"--vendor=codex", workFlag, "--spool-dir=" + dir}
	require.NoError(t, probeUsage(t.Context(), args, nil, now))
	stored, err := os.ReadFile(filepath.Join(dir, "codex-work.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "rateLimits")
	readings, failures := newRuntime(t.Context(), slog.New(slog.DiscardHandler)).Registry(dir).Read(t.Context(), now)
	assert.Empty(t, failures)
	require.Len(t, readings, 2)
	assert.Equal(t, capacity.Reading{Vendor: "openai", Account: "work", Source: capacity.CodexProbe,
		Window: "5h", Bucket: "", UsedRatio: 0.25, ResetAt: time.Unix(200000, 0), ObservedAt: now}, readings[0])
	assert.Equal(t, "7d", readings[1].Window)
	assert.InDelta(t, 0.5, readings[1].UsedRatio, 0)
	assert.Equal(t, time.Unix(300000, 0), readings[1].ResetAt)
	fakeCodex(t, `printf '%s\n' '{"id":2,"error":{"message":"private"}}'`)
	require.ErrorIs(t, probeUsage(t.Context(), args, nil, now), capacity.ErrInvalidReading)
	after, err := os.ReadFile(filepath.Join(dir, "codex-work.json"))
	require.NoError(t, err)
	assert.Equal(t, stored, after)
	fakeCodex(t, `printf '%s\n' '{"id":2,"result":{"rateLimits":`+codexData+`}}'`)
	require.Error(t, probeUsage(t.Context(), []string{"--vendor=codex", workFlag, "--spool-dir=" + filepath.Join(dir, "codex-work.json")}, nil, now))
}

func TestCodexProcessErrors(t *testing.T) {
	fakeCodex(t, "exit 3\n")
	_, err := runCodex(t.Context())
	require.Error(t, err)
	fakeCodex(t, "exec /bin/sleep 10\n")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = runCodex(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	t.Setenv("PATH", t.TempDir())
	_, err = runCodex(t.Context())
	require.Error(t, err)
}

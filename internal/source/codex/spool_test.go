// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex //nolint:testpackage // Exercise the spool parser beside the rollout parser.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

func TestSpoolParse(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{`, `{}`, `{"primary":{"usedPercent":101}}`} {
		_, err := Parse(spool.Snapshot{Vendor: "codex", Account: "work", ObservedAt: time.Time{}, Data: json.RawMessage(data)})
		require.ErrorIs(t, err, capacity.ErrInvalidReading)
	}
	got, err := Parse(spool.Snapshot{Vendor: "codex", Account: "work", ObservedAt: time.Time{}, Data: json.RawMessage(`{"secondary":{"windowDurationMins":60}}`)})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "5h", got[0].Window)
}

func TestSpoolSource(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	dir := t.TempDir()
	source := SpoolSource{Dir: dir, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	got, err := source.Read(t.Context(), time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex-bad.json"), []byte(`{`), 0o600))
	_, err = source.Read(t.Context(), time.Time{})
	require.ErrorIs(t, err, capacity.ErrInvalidReading)
	for _, account := range []string{"first", "second", "invalid"} {
		data := `{"primary":{"usedPercent":25,"resetsAt":200000,"windowDurationMins":10080}}`
		if account == "invalid" {
			data = `{}`
		}
		require.NoError(t, spool.Write(dir, spool.Snapshot{Vendor: "codex", Account: account, ObservedAt: time.Unix(100000, 0).UTC(), Data: json.RawMessage(data)}))
	}
	got, err = source.Read(t.Context(), time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "first", got[0].Account)
	assert.Equal(t, "second", got[1].Account)
	assert.Equal(t, time.Unix(100000, 0).UTC(), got[0].ObservedAt)
	assert.Contains(t, logs.String(), `"class":"invalid"`)
	assert.NotContains(t, logs.String(), dir)
	source.Dir = ""
	_, err = source.Read(t.Context(), time.Time{})
	require.ErrorIs(t, err, spool.ErrDirectory)
}

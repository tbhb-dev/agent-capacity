// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex

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

const quotaData = `{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":200000},"secondary":{"usedPercent":50,"windowDurationMins":10080,"resetsAt":300000}}`

func TestSpoolParse(t *testing.T) {
	t.Parallel()
	snapshot := spool.Snapshot{Vendor: "codex", Account: "work", ObservedAt: time.Unix(100000, 0).UTC(), Data: json.RawMessage(quotaData)}
	got, err := Parse(snapshot)
	require.NoError(t, err)
	assert.Equal(t, []capacity.Reading{
		{Vendor: "openai", Account: "work", Source: capacity.CodexRollout, Window: "5h", Bucket: "", UsedRatio: 0.25, ResetAt: time.Unix(200000, 0).UTC(), ObservedAt: snapshot.ObservedAt},
		{Vendor: "openai", Account: "work", Source: capacity.CodexRollout, Window: "7d", Bucket: "", UsedRatio: 0.5, ResetAt: time.Unix(300000, 0).UTC(), ObservedAt: snapshot.ObservedAt},
	}, got)
	for _, data := range []string{`{`, `{}`, `null`, `{"primary":{"usedPercent":101}}`} {
		snapshot.Data = json.RawMessage(data)
		_, err = Parse(snapshot)
		require.ErrorIs(t, err, capacity.ErrInvalidReading)
	}
	snapshot.Data = json.RawMessage(`{"secondary":{"usedPercent":0,"windowDurationMins":1}}`)
	got, err = Parse(snapshot)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "5h", got[0].Window)
}

func TestSpoolSource(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	source := SpoolSource{Dir: t.TempDir(), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	got, err := source.Read(t.Context(), time.Now())
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, os.WriteFile(filepath.Join(source.Dir, "codex-bad.json"), []byte(`{`), 0o600))
	_, err = source.Read(t.Context(), time.Now())
	require.ErrorIs(t, err, capacity.ErrInvalidReading)
	require.NoError(t, spool.Write(source.Dir, spool.Snapshot{Vendor: "codex", Account: "bad-data", ObservedAt: time.Now(), Data: json.RawMessage(`{}`)}))
	for _, account := range []string{"work", "personal"} {
		require.NoError(t, spool.Write(source.Dir, spool.Snapshot{Vendor: "codex", Account: account, ObservedAt: time.Unix(100000, 0).UTC(), Data: json.RawMessage(quotaData)}))
	}
	require.NoError(t, spool.Write(source.Dir, spool.Snapshot{Vendor: "agy", Account: "ignored", ObservedAt: time.Now(), Data: json.RawMessage(`{}`)}))
	got, err = source.Read(t.Context(), time.Now())
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, "personal", got[0].Account)
	assert.Equal(t, "work", got[2].Account)
	assert.Equal(t, time.Unix(100000, 0).UTC(), got[0].ObservedAt)
	assert.NotContains(t, logs.String(), source.Dir)
	assert.Contains(t, logs.String(), `"class":"invalid"`)
	source.Dir = ""
	_, err = source.Read(t.Context(), time.Now())
	require.ErrorIs(t, err, spool.ErrDirectory)
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package codex //nolint:testpackage // Match the existing parser tests.

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

const (
	spoolAccount = "work"
	spoolVendor  = "codex"
)

//nolint:lll // Keep the complete server response readable.
const quotaData = `{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":200000},"secondary":{"usedPercent":50,"windowDurationMins":10080,"resetsAt":300000}}`

func TestSpoolParse(t *testing.T) {
	t.Parallel()

	snapshot := spool.Snapshot{
		Vendor:     spoolVendor,
		Account:    spoolAccount,
		ObservedAt: time.Unix(100000, 0).UTC(),
		Data:       json.RawMessage(quotaData),
	}
	got, err := Parse(t.Context(), nil, snapshot, snapshot.ObservedAt)
	require.NoError(t, err)
	require.Len(t, got, 2)

	want := capacity.Reading{
		Vendor: "openai", Account: spoolAccount, Source: capacity.CodexProbe, Window: "5h", Bucket: "",
		UsedRatio: 0.25, ResetAt: time.Unix(200000, 0).UTC(), ObservedAt: snapshot.ObservedAt,
	}
	assert.Equal(t, want, got[0])
	want.Window = "7d"
	want.UsedRatio = 0.5
	want.ResetAt = time.Unix(300000, 0).UTC()
	assert.Equal(t, want, got[1])

	for _, data := range []string{
		`{`, `{}`, `{"primary":{},"secondary":[]}`, `{"primary":{}}`,
		`{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":200000},"secondary":[]}`,
		`{"primary":{"windowDurationMins":300,"resetsAt":200000}}`,
		`{"primary":{"usedPercent":25,"windowDurationMins":300}}`,
		`{"primary":{"usedPercent":101,"windowDurationMins":300,"resetsAt":200000}}`,
	} {
		snapshot.Data = json.RawMessage(data)
		_, err = Parse(t.Context(), nil, snapshot, snapshot.ObservedAt)
		require.ErrorIs(t, err, capacity.ErrInvalidReading)
	}

	snapshot.Data = json.RawMessage(`{"secondary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":200000}}`)
	got, err = Parse(t.Context(), nil, snapshot, snapshot.ObservedAt)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "5h", got[0].Window)
}

func TestSpoolWindowsSeen(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	source := SpoolSource{Dir: t.TempDir(), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	snapshot := spool.Snapshot{
		Vendor: spoolVendor, Account: "spool-seen", ObservedAt: time.Unix(100000, 0).UTC(), Data: nil,
	}

	for _, testCase := range []struct {
		err     error
		primary string
		logged  string
		window  string
	}{
		{nil, `{"usedPercent":25,"windowDurationMins":60,"resetsAt":200000}`, "[60,10080]", "7d"},
		{nil, `{"usedPercent":25,"resetsAt":200000}`, "[10080]", "7d"},
		{capacity.ErrInvalidReading, `{"windowDurationMins":300}`, "[300,10080]", ""},
	} {
		snapshot.Data = json.RawMessage(`{"primary":` + testCase.primary +
			`,"secondary":{"usedPercent":50,"windowDurationMins":10080,"resetsAt":300000}}`)
		require.NoError(t, spool.Write(source.Dir, snapshot))
		logs.Reset()

		readings, err := source.Read(t.Context(), snapshot.ObservedAt)
		require.ErrorIs(t, err, testCase.err)
		assert.Contains(t, logs.String(), `"windows_seen":`+testCase.logged)

		if testCase.window == "" {
			assert.Empty(t, readings)
		} else {
			require.Len(t, readings, 1)
			assert.Equal(t, testCase.window, readings[0].Window)
		}
	}
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

	for _, fixture := range []struct{ account, data string }{
		{"bad-data", `{}`}, {spoolAccount, quotaData}, {"personal", quotaData},
	} {
		snapshot := spool.Snapshot{
			Vendor: spoolVendor, Account: fixture.account, ObservedAt: time.Unix(100000, 0).UTC(),
			Data: json.RawMessage(fixture.data),
		}
		require.NoError(t, spool.Write(source.Dir, snapshot))
	}

	logs.Reset()

	got, err = source.Read(t.Context(), time.Now())
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, "personal", got[0].Account)
	assert.Equal(t, spoolAccount, got[2].Account)
	assert.Equal(t, time.Unix(100000, 0).UTC(), got[0].ObservedAt)
	assert.NotContains(t, logs.String(), source.Dir)
	assert.Contains(t, logs.String(), `"class":"invalid"`)
	assert.Contains(t, logs.String(), `"windows_seen":[300,10080]`)
	assert.Equal(t, 4, bytes.Count(logs.Bytes(), []byte("\n")))

	source.Dir = ""
	_, err = source.Read(t.Context(), time.Now())
	require.ErrorIs(t, err, spool.ErrDirectory)
}

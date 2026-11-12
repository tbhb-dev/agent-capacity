// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package claude //nolint:testpackage // Exercise snapshot parsing errors separately from file errors.

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

const claudeVendor = "claude"

func TestParse(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		data    string
		windows []string
		ratios  []float64
		invalid bool
	}{
		{`{"five_hour":{"used_percentage":25,
"resets_at":200000},
"seven_day":{"used_percentage":75,
"resets_at":200000}}`, []string{"5h", "7d"}, []float64{0.25, 0.75}, false},
		{`{"seven_day":{"used_percentage":40,"resets_at":200000}}`, []string{"7d"}, []float64{0.4}, false},
		{`{"five_hour":{"used_percentage":100,"resets_at":200000}}`, []string{"5h"}, []float64{1}, false},
		{`{"five_hour":{"used_percentage":0,"resets_at":200000}}`, []string{"5h"}, []float64{0}, false},
		{`null`, nil, nil, false},
		{`{}`, nil, nil, false},
		{`{"five_hour":null,"seven_day":null}`, nil, nil, false},
		{`{`, nil, nil, true},
		{`[]`, nil, nil, true},
		{`{"five_hour":4}`, nil, nil, true},
		{`{"five_hour":{"used_percentage":-0.1,"resets_at":200000}}`, nil, nil, true},
		{`{"five_hour":{"used_percentage":25,
"resets_at":200000},
"seven_day":{"used_percentage":100.1,
"resets_at":200000}}`, nil, nil, true},
		{`{"five_hour":{"used_percentage":null,"resets_at":200000}}`, nil, nil, true},
		{`{"five_hour":{"used_percentage":25}}`, nil, nil, true},
	} {
		snapshot := spool.Snapshot{
			Vendor:     claudeVendor,
			Account:    "work",
			ObservedAt: time.Unix(100000, 0).UTC(),
			Data:       json.RawMessage(testCase.data),
		}

		got, err := parse(snapshot)
		if testCase.invalid {
			require.ErrorIs(t, err, capacity.ErrInvalidReading)
			assert.Nil(t, got)

			continue
		}

		require.NoError(t, err)
		require.Len(t, got, len(testCase.windows))

		for index, reading := range got {
			assert.Equal(
				t,
				capacity.Reading{
					Vendor:     "anthropic",
					Account:    "work",
					Source:     capacity.ClaudeStatusline,
					Window:     testCase.windows[index],
					UsedRatio:  testCase.ratios[index],
					ObservedAt: snapshot.ObservedAt,
					ResetAt:    time.Unix(200000, 0).UTC(),
				},
				reading,
			)
		}
	}
}

func TestSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var logs bytes.Buffer

	source := Source{Dir: dir, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	got, err := source.Read(t.Context(), time.Now())
	require.NoError(t, err)
	assert.Nil(t, got)

	for _, account := range []string{"first", "second"} {
		require.NoError(
			t,
			spool.Write(
				dir,
				spool.Snapshot{
					Vendor:     claudeVendor,
					Account:    account,
					ObservedAt: time.Unix(100000, 0).UTC(),
					Data:       json.RawMessage(`{"seven_day":{"used_percentage":50,"resets_at":200000}}`),
				},
			),
		)
		require.NoError(
			t,
			os.Chtimes(filepath.Join(dir, "claude-"+account+".json"), time.Unix(300000, 0), time.Unix(300000, 0)),
		)
	}

	for _, fixture := range []struct{ vendor, account, data string }{
		{"agy", "other", `{}`},
		{claudeVendor, "invalid", `{"five_hour":{"used_percentage":101,"resets_at":200000}}`},
	} {
		require.NoError(
			t,
			spool.Write(
				dir,
				spool.Snapshot{
					Vendor:     fixture.vendor,
					Account:    fixture.account,
					ObservedAt: time.Unix(100000, 0).UTC(),
					Data:       json.RawMessage(fixture.data),
				},
			),
		)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-bad.json"), []byte(`{`), 0o600))
	got, err = source.Read(t.Context(), time.Unix(400000, 0))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "first", got[0].Account)
	assert.Equal(t, "second", got[1].Account)
	assert.Equal(t, time.Unix(100000, 0).UTC(), got[0].ObservedAt)
	assert.Equal(t, time.Unix(100000, 0).UTC(), got[1].ObservedAt)
	assert.Equal(t, 2, bytes.Count(logs.Bytes(), []byte("\n")))
	assert.NotContains(t, logs.String(), dir)
	assert.Contains(t, logs.String(), `"class":"invalid"`)

	source.Dir = ""
	got, err = source.Read(t.Context(), time.Time{})
	require.ErrorIs(t, err, spool.ErrDirectory)
	assert.Nil(t, got)
}

func TestMalformedSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var logs bytes.Buffer

	source := Source{Dir: dir, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-bad.json"), []byte(`{`), 0o600))
	got, err := source.Read(t.Context(), time.Time{})
	require.ErrorIs(t, err, capacity.ErrInvalidReading)
	assert.Nil(t, got)
}

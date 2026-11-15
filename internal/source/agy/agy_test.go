// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package agy //nolint:testpackage // Exercise snapshot parsing errors separately from file errors.

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
	agyVendor = "agy"
	validData = `{"groups":[{"buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.5,
"reset_time":"2026-11-21T02:22:26Z"}]}]}`
)

func snapshot(data string) spool.Snapshot {
	return spool.Snapshot{
		Vendor: agyVendor, Account: "work", ObservedAt: time.Unix(100000, 0).UTC(), Data: json.RawMessage(data),
	}
}

func TestParseFixture(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/usage-data.json")
	require.NoError(t, err)

	got, err := Parse(snapshot(string(data)))
	require.NoError(t, err)

	// Subtract at run time, as the parser does, rather than in constant arithmetic.
	used := func(remaining float64) float64 { return 1 - remaining }
	want := []struct {
		window, bucket, reset string
		used                  float64
	}{
		{"7d", "gemini", "2026-11-21T02:22:26Z", used(0.9976193308830261)},
		{"5h", "gemini", "2026-11-15T07:19:38Z", used(0.9904893636703491)},
		{"7d", "3p", "2026-11-22T02:20:48Z", 0},
		{"5h", "3p", "2026-11-15T07:20:48Z", 0},
	}
	require.Len(t, got, len(want))

	for index, reading := range got {
		reset, parseErr := time.Parse(time.RFC3339, want[index].reset)
		require.NoError(t, parseErr)
		assert.Equal(t, capacity.Reading{
			Vendor: "google", Window: want[index].window, Bucket: want[index].bucket, Account: "work",
			Source: capacity.AGYStatusline, UsedRatio: want[index].used, ResetAt: reset,
			ObservedAt: time.Unix(100000, 0).UTC(),
		}, reading)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()

	const (
		reset = `"reset_time":"2026-11-15T07:19:38Z"`
		good  = `{"id":"gemini-5h","window":"5h","remaining_fraction":0.25,` + reset + `}`
	)

	for _, testCase := range []struct {
		data    string
		used    []float64
		invalid bool
	}{
		{
			`{"groups":[{"buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":0.25,` + reset + `}]}]}`,
			[]float64{0.75},
			false,
		},
		{`{"groups":[{"buckets":[{"id":"3p-weekly","window":"weekly",` + reset + `},
{"id":"3p-5h","window":"5h","remaining_fraction":0.5,` + reset + `}]}]}`, []float64{1, 0.5}, false},
		{`{"groups":[{"buckets":[{"id":"3p-5h","window":"5h","remaining_fraction":0.5,` + reset + `},
{"id":"3p-weekly","window":"weekly",` + reset + `}]}]}`, []float64{0.5, 1}, false},
		{`{"groups":[{"buckets":[` + good + `]},{"buckets":"none"}]}`, nil, true},
		{`{"groups":[{"buckets":[` + good + `,
{"id":"gemini-weekly","window":"weekly","remaining_fraction":1}]}]}`, nil, true},
		{`{"groups":[{"buckets":[` + good + `,
{"id":"3p-5h","window":"5h","remaining_fraction":1.5,` + reset + `}]}]}`, nil, true},
		{`{"groups":[{"buckets":[{"id":"other-5h","window":"5h","remaining_fraction":1,` + reset + `},
{"id":"3p-weekly","window":"weekly",` + reset + `},
{"id":"gemini-5h","window":"5h",` + reset + `}]}]}`, nil, true},
		{`{"groups":[{"buckets":[{"id":"other-5h","window":"5h","remaining_fraction":1,` + reset + `},
{"id":"gemini-month","window":"month","remaining_fraction":1,` + reset + `},
{"id":"gemini-5h","window":"5h","remaining_fraction":1,` + reset + `}]}]}`, []float64{0}, false},
		{`{"groups":[{"buckets":[{"id":"other-5h","window":"5h","remaining_fraction":1,` + reset + `}]}]}`, nil, true},
		{`{"groups":[]}`, nil, true},
		{`null`, nil, true},
		{`{`, nil, true},
		{`[]`, nil, true},
		{`{"groups":[{"buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":1}]}]}`, nil, true},
		{`{"groups":[{"buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":1.5,` + reset + `}]}]}`, nil, true},
		{`{"groups":[{"buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":-0.1,` + reset + `}]}]}`, nil, true},
	} {
		got, err := Parse(snapshot(testCase.data))
		if testCase.invalid {
			require.ErrorIs(t, err, capacity.ErrInvalidReading, testCase.data)
			assert.Nil(t, got)

			continue
		}

		require.NoError(t, err, testCase.data)
		require.Len(t, got, len(testCase.used))

		for index, reading := range got {
			assert.InDelta(t, testCase.used[index], reading.UsedRatio, 0, testCase.data)
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
					Vendor:     agyVendor,
					Account:    account,
					ObservedAt: time.Unix(100000, 0).UTC(),
					Data:       json.RawMessage(validData),
				},
			),
		)
		require.NoError(
			t,
			os.Chtimes(filepath.Join(dir, "agy-"+account+".json"), time.Unix(300000, 0), time.Unix(300000, 0)),
		)
	}

	for _, fixture := range []struct{ vendor, account, data string }{
		{"claude", "other", `{}`},
		{agyVendor, "invalid", `{"groups":[]}`},
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

	require.NoError(t, os.WriteFile(filepath.Join(dir, "agy-bad.json"), []byte(`{`), 0o600))
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agy-bad.json"), []byte(`{`), 0o600))
	got, err := source.Read(t.Context(), time.Time{})
	require.ErrorIs(t, err, capacity.ErrInvalidReading)
	assert.Nil(t, got)
}

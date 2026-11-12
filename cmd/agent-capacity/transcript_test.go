// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/export"
	"go.tbhb.dev/agent-capacity/internal/source/claude"
)

const transcriptAccount = "transcript-work"

func TestTranscriptFlags(t *testing.T) {
	t.Setenv("AGENT_CAPACITY_CLAUDE_TRANSCRIPTS", "synthetic-env-file")

	options, err := parseServe([]string{serveCommand})
	require.NoError(t, err)
	assert.Equal(t, "synthetic-env-file", options.claudeTranscripts)
	assert.Equal(t, "primary", options.claudeAccount)
	options, err = parseServe(
		[]string{serveCommand, "--claude-transcripts=one,two", "--claude-account=" + transcriptAccount},
	)
	require.NoError(t, err)
	assert.Equal(t, "one,two", options.claudeTranscripts)
	assert.Equal(t, transcriptAccount, options.claudeAccount)
	options, err = parseServe([]string{serveCommand, "--claude-transcripts="})
	require.NoError(t, err)
	assert.Empty(t, options.claudeTranscripts)
	t.Setenv("AGENT_CAPACITY_CLAUDE_TRANSCRIPTS", "")

	options, err = parseServe([]string{serveCommand})
	require.NoError(t, err)
	assert.Empty(t, options.claudeTranscripts)
}

func TestTranscriptRuntime(t *testing.T) {
	t.Setenv("CODEX_HOME", "")

	for _, account := range []string{transcriptAccount, "private@example.invalid"} {
		var out, logs bytes.Buffer

		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		sdk := sdkmetric.NewMeterProvider()

		t.Cleanup(func() { require.NoError(t, sdk.Shutdown(context.Background())) })

		stub := &provider{
			MeterProvider: sdk,
			shutdownErr:   nil,
			deadline:      time.Time{},
			shutdownAt:    time.Time{},
			stopped:       false,
		}
		runtime := Runtime{
			NewRegistry: nil,
			Registry:    func(string) capacity.Registry { return nil },
			NewProvider: func(context.Context) (Provider, error) { return stub, nil },
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		code := runtime.Run(
			ctx,
			withSpool([]string{serveCommand, "--claude-transcripts=synthetic-file", "--claude-account=" + account}),
			&out,
			logger,
		)
		if account == transcriptAccount {
			assert.Equal(t, 0, code)
			assert.Contains(t, logs.String(), `"source":"claude_transcript","class":"canceled"`)
			assert.True(t, stub.stopped)
		} else {
			assert.Equal(t, 1, code)
			assertLogs(t, &logs, invalidCommand)
			assert.False(t, stub.stopped)
		}

		assert.NotContains(t, logs.String(), "synthetic-file")
		assert.NotContains(t, logs.String(), account)
	}
}

func TestTranscriptMetrics(t *testing.T) {
	t.Parallel()

	observed := time.Unix(100000, 0).UTC()
	name := filepath.Join(t.TempDir(), "events.jsonl")
	payload := `{"timestamp":"1970-01-02T03:46:40Z","type":"rate_limit_event","ignored":"private content",` +
		`"rate_limit_info":{"unifiedWindows":{"seven_day":` +
		`{"utilization":0.25,"resetsAt":200000,"status":"allowed"},"five_hour":null}}}`
	require.NoError(t, os.WriteFile(name, []byte(payload), 0o600))
	require.NoError(t, os.Chtimes(name, observed.Add(time.Hour), observed.Add(time.Hour)))
	registry, err := claude.RegisterTranscripts(name, transcriptAccount)
	require.NoError(t, err)
	readings, failures := registry.Read(t.Context(), observed)
	assert.Empty(t, failures)
	require.Len(t, readings, 1)

	for _, fixture := range []struct {
		freshness string
		age       time.Duration
		count     int
	}{
		{freshLabel, -time.Second, 3},
		{freshLabel, 10 * time.Minute, 3},
		{carriedLabel, 10*time.Minute + time.Nanosecond, 3},
		{carriedLabel, 6 * time.Hour, 3},
		{droppedLabel, 6*time.Hour + time.Nanosecond, 0},
	} {
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

		t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

		collector, createErr := export.New(
			provider.Meter("test"),
			func() time.Time { return observed.Add(fixture.age) },
		)
		require.NoError(t, createErr)
		require.NoError(t, collector.Replace(readings))

		var data metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(t.Context(), &data))

		count := 0
		values := map[string]float64{
			utilizationGauge:                   0.25,
			"afakt.capacity.window.reset_time": 200000,
			"afakt.capacity.snapshot.age":      max(fixture.age, 0).Seconds(),
		}

		for _, scope := range data.ScopeMetrics {
			for _, measurement := range scope.Metrics {
				count++

				require.Contains(t, values, measurement.Name)
				gauge, ok := measurement.Data.(metricdata.Gauge[float64])
				require.True(t, ok)
				require.Len(t, gauge.DataPoints, 1)
				assert.InDelta(t, values[measurement.Name], gauge.DataPoints[0].Value, 0)
				assert.Equal(t, attribute.NewSet(
					attribute.String("afakt.plan.vendor", "anthropic"),
					attribute.String("afakt.plan.window", "7d"),
					attribute.String("afakt.capacity.source", "claude_transcript"),
					attribute.String("afakt.capacity.freshness", fixture.freshness),
					attribute.String("afakt.account", transcriptAccount),
				), gauge.DataPoints[0].Attributes)
			}
		}

		assert.Equal(t, fixture.count, count)
	}
}

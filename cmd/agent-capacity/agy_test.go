// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/export"
)

const agyVendorFlag = "--vendor=agy"

func TestAGYMetrics(t *testing.T) {
	t.Setenv("CODEX_HOME", "")

	previous := otel.GetErrorHandler()

	t.Cleanup(func() { otel.SetErrorHandler(previous) })

	dir := t.TempDir()
	observed := time.Unix(100000, 0).UTC()
	payload := `{"quota":{"five_hour":{"used_percentage":25,"resets_at":200000},"seven_day":null}}`
	require.NoError(t, capture([]string{"--spool-dir=" + dir, agyVendorFlag, "--account=work"},
		strings.NewReader(payload), observed))

	logger := slog.New(slog.DiscardHandler)
	runtime := newRuntime(t.Context(), logger)
	registry := runtime.Registry(dir)
	require.Len(t, registry, 2)
	assert.Equal(t, capacity.ClaudeStatusline, registry[0].Name)
	assert.Equal(t, capacity.AGYStatusline, registry[1].Name)
	readings, failures := registry.Read(t.Context(), observed)
	assert.Empty(t, failures)

	require.Len(t, readings, 1)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	for _, testCase := range []struct {
		freshness string
		age       time.Duration
	}{
		{freshLabel, 0},
		{freshLabel, 10 * time.Minute},
		{carriedLabel, 10*time.Minute + time.Nanosecond},
		{carriedLabel, 6 * time.Hour},
		{droppedLabel, 6*time.Hour + time.Nanosecond},
	} {
		collector, err := export.New(provider.Meter(testCase.age.String()), func() time.Time {
			return observed.Add(testCase.age)
		})
		require.NoError(t, err)
		require.NoError(t, collector.Replace(readings))

		var data metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(t.Context(), &data))
		assertAGYGauges(t, data, testCase.freshness, testCase.age)
		require.NoError(t, collector.Replace(nil))
	}
}

func assertAGYGauges(t *testing.T, data metricdata.ResourceMetrics, freshness string, age time.Duration) {
	t.Helper()

	values := map[string]float64{
		utilizationGauge:                   0.25,
		"afakt.capacity.window.reset_time": 200000,
		"afakt.capacity.snapshot.age":      age.Seconds(),
	}
	count := 0

	for _, scope := range data.ScopeMetrics {
		for _, measurement := range scope.Metrics {
			count++

			require.Contains(t, values, measurement.Name)
			gauge, ok := measurement.Data.(metricdata.Gauge[float64])
			require.True(t, ok)
			require.Len(t, gauge.DataPoints, 1)
			assert.InDelta(t, values[measurement.Name], gauge.DataPoints[0].Value, 0)
			assert.Equal(t, attribute.NewSet(
				attribute.String("afakt.plan.vendor", "google"),
				attribute.String("afakt.plan.window", "5h"),
				attribute.String("afakt.capacity.source", "agy_statusline"),
				attribute.String("afakt.capacity.freshness", freshness),
				attribute.String("afakt.account", "work"),
			), gauge.DataPoints[0].Attributes)
		}
	}

	if freshness == droppedLabel {
		assert.Equal(t, 0, count)
	} else {
		assert.Equal(t, 3, count)
	}
}

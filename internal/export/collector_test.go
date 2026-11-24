// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package export_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/export"
)

const (
	personalAlias = "personal"
	workAlias     = "work"
	resetName     = "agent.capacity.window.reset_time"
)

func reading(now time.Time) capacity.Reading {
	return capacity.Reading{
		Bucket: "", Vendor: "openai", Window: "7d", Account: workAlias, Source: "codex_rollout",
		UsedRatio: 0.5, ResetAt: now.Add(time.Hour), ObservedAt: now,
	}
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &data))

	var metrics []metricdata.Metrics
	for _, scope := range data.ScopeMetrics {
		metrics = append(metrics, scope.Metrics...)
	}

	return metrics
}

func TestCollector(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 500000000)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	collector, err := export.New(provider.Meter("test"), func() time.Time { return now })
	require.NoError(t, err)
	assert.Empty(t, collect(t, reader))

	first := reading(now)
	unknownReset := first
	unknownReset.Account = personalAlias
	unknownReset.ResetAt = time.Time{}
	unknownReset.ObservedAt = now.Add(-time.Hour)
	dropped := first
	dropped.Account = "old"
	dropped.ObservedAt = now.Add(-6*time.Hour - time.Nanosecond)
	dropped.ResetAt = now
	readings := []capacity.Reading{first, dropped, unknownReset}
	require.NoError(t, collector.Replace(readings))
	readings[0].Account = "changed"

	metrics := collect(t, reader)
	require.Len(t, metrics, 3)

	assertMetrics(t, metrics)

	invalid := first
	invalid.Account = "@"
	require.ErrorIs(t, collector.Replace([]capacity.Reading{invalid}), capacity.ErrInvalidReading)
	assert.Len(t, collect(t, reader), 3)

	now = now.Add(7 * time.Hour)

	assert.Empty(t, collect(t, reader))
	require.NoError(t, collector.Replace(nil))
	assert.Empty(t, collect(t, reader))
}

func assertMetrics(t *testing.T, metrics []metricdata.Metrics) {
	t.Helper()

	units := map[string]string{
		"agent.capacity.window.utilization": "1",
		resetName:                           "s",
		"agent.capacity.snapshot.age":       "s",
	}
	for _, measurement := range metrics {
		assert.Equal(t, units[measurement.Name], measurement.Unit)
		require.Contains(t, units, measurement.Name)
		gauge, ok := measurement.Data.(metricdata.Gauge[float64])
		require.True(t, ok)

		wantCount := 2
		if measurement.Name == resetName {
			wantCount = 1
		}

		require.Len(t, gauge.DataPoints, wantCount)

		for _, point := range gauge.DataPoints {
			account, found := point.Attributes.Value("agent.account")
			require.True(t, found)

			alias := account.AsString()
			require.Contains(t, []string{workAlias, personalAlias}, alias)

			freshness := "fresh"
			age := 0.0

			if alias == personalAlias {
				freshness = "carried"
				age = time.Hour.Seconds()
			}

			wantAttrs := attribute.NewSet(
				attribute.String("agent.plan.vendor", "openai"),
				attribute.String("agent.plan.window", "7d"),
				attribute.String("agent.capacity.source", "codex_rollout"),
				attribute.String("agent.capacity.freshness", freshness),
				attribute.String("agent.account", alias),
			)
			assert.Equal(t, wantAttrs, point.Attributes)

			for _, attr := range point.Attributes.ToSlice() {
				assert.NotContains(t, attr.Value.AsString(), "@")
			}

			switch measurement.Name {
			case "agent.capacity.window.utilization":
				assert.InDelta(t, 0.5, point.Value, 0)
			case resetName:
				assert.InDelta(t, 103600.5, point.Value, 0)
			case "agent.capacity.snapshot.age":
				assert.InDelta(t, age, point.Value, 0)
			}
		}
	}
}

type failingMeter struct {
	metric.Meter

	failGauge bool
}

//nolint:ireturn // The SDK requires this interface return type.
func (measurement failingMeter) Float64ObservableGauge(
	string,
	...metric.Float64ObservableGaugeOption,
) (metric.Float64ObservableGauge, error) {
	if measurement.failGauge {
		return nil, errors.ErrUnsupported
	}

	return nil, nil //nolint:nilnil // Gauges are unused when callback registration fails.
}

//nolint:ireturn // The SDK requires this interface return type.
func (measurement failingMeter) RegisterCallback(metric.Callback, ...metric.Observable) (metric.Registration, error) {
	return nil, errors.ErrUnsupported
}

func TestRegistrationErrors(t *testing.T) {
	t.Parallel()

	for _, failGauge := range []bool{true, false} {
		collector, err := export.New(failingMeter{Meter: nil, failGauge: failGauge}, time.Now)
		require.ErrorIs(t, err, errors.ErrUnsupported)
		assert.Nil(t, collector)

		if failGauge {
			require.ErrorContains(t, err, "create gauge")
		} else {
			require.ErrorContains(t, err, "register gauges")
		}
	}
}

func TestCollectorBucket(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	collector, err := export.New(provider.Meter("bucket"), func() time.Time { return now })
	require.NoError(t, err)

	quota := reading(now)
	quota.Vendor, quota.Source, quota.Bucket = "google", "agy_statusline", "3p"
	require.NoError(t, collector.Replace([]capacity.Reading{quota}))

	metrics := collect(t, reader)
	require.Len(t, metrics, 3)

	for _, measurement := range metrics {
		gauge, ok := measurement.Data.(metricdata.Gauge[float64])
		require.True(t, ok)
		require.Len(t, gauge.DataPoints, 1)

		bucket, found := gauge.DataPoints[0].Attributes.Value("agent.plan.bucket")
		require.True(t, found)
		assert.Equal(t, "3p", bucket.AsString())
	}
}

func TestCollectorTokens(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	collector, err := export.New(provider.Meter("tokens"), func() time.Time { return now })
	require.NoError(t, err)

	quota := reading(now)
	quota.Tokens = map[string][4]int64{"model-a": {1, 2, 3, 4}}
	require.NoError(t, collector.Replace([]capacity.Reading{quota}))
	quota.Tokens["model-a"] = [4]int64{}
	metrics := collect(t, reader)
	require.Len(t, metrics, 4)

	for _, measurement := range metrics {
		if measurement.Name != "agent.capacity.window.tokens" {
			continue
		}

		gauge, ok := measurement.Data.(metricdata.Gauge[float64])
		require.True(t, ok)
		require.Len(t, gauge.DataPoints, 4)

		for _, point := range gauge.DataPoints {
			model, found := point.Attributes.Value("model")
			require.True(t, found)
			assert.Equal(t, "model-a", model.AsString())

			tokenType, found := point.Attributes.Value("token_type")
			require.True(t, found)
			assert.InDelta(
				t,
				map[string]float64{"input": 1, "output": 2, "cache_read": 3, "cache_write": 4}[tokenType.AsString()],
				point.Value,
				0,
			)

			for key, value := range map[string]string{
				"agent.account": workAlias, "agent.plan.vendor": "openai", "agent.plan.window": "7d",
				"agent.capacity.source": "codex_rollout", "agent.capacity.freshness": "fresh",
			} {
				attr, exists := point.Attributes.Value(attribute.Key(key))
				require.True(t, exists)
				assert.Equal(t, value, attr.AsString())
			}
		}
	}

	now = now.Add(7 * time.Hour)

	assert.Empty(t, collect(t, reader))
	require.NoError(t, collector.Replace([]capacity.Reading{reading(now)}))
	assert.Len(t, collect(t, reader), 3)
}

func TestCollectorTokensOnly(t *testing.T) {
	t.Parallel()

	now := time.Unix(100000, 0)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	collector, err := export.New(provider.Meter("tokens-only"), func() time.Time { return now })
	require.NoError(t, err)

	quota := reading(now)
	quota.Tokens = map[string][4]int64{"model-a": {1, 2, 3, 4}}
	quota.TokensOnly = true
	require.NoError(t, collector.Replace([]capacity.Reading{quota}))

	// A tokens-only reading has no plan window, so only the token gauge reports.
	metrics := collect(t, reader)
	require.Len(t, metrics, 1)
	assert.Equal(t, "agent.capacity.window.tokens", metrics[0].Name)
}

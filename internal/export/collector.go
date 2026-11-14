// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package export observes capacity readings through the OpenTelemetry SDK.
package export

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

// Collector observes a copy of the latest successful collection.
type Collector struct {
	now      func() time.Time
	readings []capacity.Reading
	mu       sync.RWMutex
}

// New registers three observable gauges with the supplied meter.
// Standard OpenTelemetry Protocol (OTLP) to Prometheus translation adds unit suffixes.
func New(meter metric.Meter, now func() time.Time) (*Collector, error) {
	collector := &Collector{mu: sync.RWMutex{}, readings: nil, now: now}

	var gauges []metric.Float64ObservableGauge

	for _, spec := range []struct{ name, unit string }{
		{"agent.capacity.window.utilization", "1"},
		{"agent.capacity.window.reset_time", "s"},
		{"agent.capacity.snapshot.age", "s"},
	} {
		gauge, err := meter.Float64ObservableGauge(spec.name, metric.WithUnit(spec.unit))
		if err != nil {
			return nil, fmt.Errorf("create gauge: %w", err)
		}

		gauges = append(gauges, gauge)
	}

	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		collector.observe(observer, gauges)

		return nil
	}, gauges[0], gauges[1], gauges[2])
	if err != nil {
		return nil, fmt.Errorf("register gauges: %w", err)
	}

	return collector, nil
}

// Replace validates before replacing the snapshot. Invalid batches never export.
func (collector *Collector) Replace(readings []capacity.Reading) error {
	for _, reading := range readings {
		err := reading.Validate()
		if err != nil {
			return fmt.Errorf("replace readings: %w", err)
		}
	}

	collector.mu.Lock()
	defer collector.mu.Unlock()

	collector.readings = slices.Clone(readings)

	return nil
}

func (collector *Collector) observe(observer metric.Observer, gauges []metric.Float64ObservableGauge) {
	collector.mu.RLock()
	defer collector.mu.RUnlock()

	now := collector.now()
	for _, reading := range collector.readings {
		age, freshness := capacity.Freshness(reading, now)
		if freshness == "dropped" {
			continue
		}

		attrs := metric.WithAttributes(
			attribute.String("agent.plan.vendor", reading.Vendor),
			attribute.String("agent.plan.window", reading.Window),
			attribute.String("agent.capacity.source", reading.Source),
			attribute.String("agent.capacity.freshness", freshness),
			attribute.String("agent.account", reading.Account),
		)
		observer.ObserveFloat64(gauges[0], reading.UsedRatio, attrs)

		if !reading.ResetAt.IsZero() {
			observer.ObserveFloat64(gauges[1], float64(reading.ResetAt.UnixNano())/float64(time.Second), attrs)
		}

		observer.ObserveFloat64(gauges[2], age.Seconds(), attrs)
	}
}

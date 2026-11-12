// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package export

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// ErrProtocol rejects protocols outside the SDK's OpenTelemetry Protocol (OTLP) metric transports.
var ErrProtocol = errors.New("unsupported OTLP metrics protocol")

// NewProvider uses only standard OpenTelemetry environment configuration.
// Selecting the two metric exporters directly avoids the `autoexport` trace, log,
// stdout and Prometheus dependencies. Exporters handle endpoints and headers.
// PeriodicReader handles OTEL_METRIC_EXPORT_INTERVAL and its timeout.
func NewProvider(ctx context.Context) (*sdkmetric.MeterProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "agent-capacity")),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	exporter, err := newExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("create exporter: %w", err)
	}

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(NonemptyExporter{Exporter: exporter})),
	), nil
}

// NonemptyExporter suppresses export requests when there are no metric points.
// This lets a service with no sources run without sending empty snapshots.
type NonemptyExporter struct {
	sdkmetric.Exporter
}

// Export skips empty collections and delegates other snapshots to the SDK.
//
//nolint:wrapcheck // Preserve the SDK error for its error handler.
func (e NonemptyExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	if len(data.ScopeMetrics) == 0 {
		return nil
	}

	return e.Exporter.Export(ctx, data)
}

//nolint:ireturn,wrapcheck // Both SDK transports implement Exporter. NewProvider wraps setup errors.
func newExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}

	switch protocol {
	case "grpc":
		return otlpmetricgrpc.New(ctx)
	case "", "http/protobuf":
		return otlpmetrichttp.New(ctx)
	default:
		return nil, ErrProtocol
	}
}

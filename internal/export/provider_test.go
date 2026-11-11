// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package export_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.tbhb.dev/agent-capacity/internal/export"
)

const (
	grpcProtocol    = "grpc"
	unknownProtocol = "unknown"
)

func TestProviderEnvironment(t *testing.T) {
	for _, testCase := range []struct {
		general, metrics string
		valid            bool
	}{
		{"", "", true},
		{"http/protobuf", "", true},
		{grpcProtocol, "", true},
		{unknownProtocol, grpcProtocol, true},
		{grpcProtocol, "http/protobuf", true},
		{unknownProtocol, "", false},
		{grpcProtocol, unknownProtocol, false},
	} {
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", testCase.general)
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", testCase.metrics)
		t.Setenv("OTEL_SERVICE_NAME", "test-service")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test")

		provider, err := export.NewProvider(t.Context())
		if testCase.valid {
			require.NoError(t, err)
			require.NoError(t, provider.Shutdown(context.Background()))
		} else {
			require.ErrorIs(t, err, export.ErrProtocol)
			assert.Nil(t, provider)
		}
	}

	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "malformed")
	provider, err := export.NewProvider(t.Context())
	require.ErrorIs(t, err, resource.ErrPartialResource)
	require.ErrorContains(t, err, "create resource")
	assert.Nil(t, provider)
}

type recordingExporter struct {
	sdkmetric.Exporter

	got *metricdata.ResourceMetrics
}

func (e *recordingExporter) Export(_ context.Context, data *metricdata.ResourceMetrics) error {
	e.got = data

	return errors.ErrUnsupported
}

func TestNonemptyExporter(t *testing.T) {
	t.Parallel()

	recording := &recordingExporter{Exporter: nil, got: nil}
	exporter := export.NonemptyExporter{Exporter: recording}
	require.NoError(t, exporter.Export(t.Context(), &metricdata.ResourceMetrics{Resource: nil, ScopeMetrics: nil}))
	assert.Nil(t, recording.got)

	data := &metricdata.ResourceMetrics{
		Resource: nil,
		ScopeMetrics: []metricdata.ScopeMetrics{
			{
				Metrics: nil,
				Scope:   instrumentation.Scope{Name: "", Version: "", SchemaURL: "", Attributes: attribute.NewSet()},
			},
		},
	}
	require.ErrorIs(t, exporter.Export(t.Context(), data), errors.ErrUnsupported)
	assert.Same(t, data, recording.got)
}

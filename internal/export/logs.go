// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package export

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// ErrLogProtocol rejects unsupported log transports.
var ErrLogProtocol = errors.New("unsupported OTLP logs protocol")

// NewLogProvider batches logs when an OpenTelemetry Protocol (OTLP) endpoint is configured.
// A nil provider leaves logging on stderr. The bounded queue drops records when full.
func NewLogProvider(ctx context.Context) (*sdklog.LoggerProvider, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return nil, nil //nolint:nilnil // A nil provider disables export when no endpoint is configured.
	}

	res, err := newResource(ctx)
	if err != nil {
		return nil, fmt.Errorf("create log resource: %w", err)
	}

	exporter, err := newLogExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("create log exporter: %w", err)
	}

	return sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	), nil
}

//nolint:ireturn,wrapcheck // SDK transports implement Exporter. NewLogProvider wraps setup errors.
func newLogExporter(ctx context.Context) (sdklog.Exporter, error) {
	protocol := signalProtocol("LOGS")
	if protocol == "grpc" {
		return otlploggrpc.New(ctx)
	}

	if protocol == "" || protocol == "http/protobuf" {
		return otlploghttp.New(ctx)
	}

	return nil, ErrLogProtocol
}

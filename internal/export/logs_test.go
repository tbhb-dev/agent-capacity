// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package export_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/sdk/resource"

	"go.tbhb.dev/agent-capacity/internal/export"
)

func TestLogProviderEnvironment(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", unknownProtocol)

	provider, err := export.NewLogProvider(t.Context())
	require.NoError(t, err)
	assert.Nil(t, provider)

	const httpProtocol = "http/protobuf"

	for _, testCase := range []struct {
		wantErr                 error
		general, logs, endpoint string
	}{
		{nil, "", "", "http://localhost:4318"},
		{nil, httpProtocol, "", ""},
		{nil, grpcProtocol, "", ""},
		{nil, unknownProtocol, grpcProtocol, ""},
		{nil, grpcProtocol, httpProtocol, ""},
		{export.ErrLogProtocol, unknownProtocol, "", ""},
		{export.ErrLogProtocol, grpcProtocol, unknownProtocol, ""},
	} {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", testCase.endpoint)
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", testCase.general)
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", testCase.logs)

		provider, err = export.NewLogProvider(t.Context())
		if testCase.wantErr != nil {
			require.ErrorIs(t, err, testCase.wantErr)
			assert.Nil(t, provider)

			continue
		}

		require.NoError(t, err)
		require.NoError(t, provider.Shutdown(context.Background()))
	}

	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "malformed")
	provider, err = export.NewLogProvider(t.Context())
	require.ErrorIs(t, err, resource.ErrPartialResource)
	assert.Nil(t, provider)
}

func TestLogQueueAndShutdownDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		_, err := io.Copy(io.Discard, request.Body)
		assert.NoError(t, err)

		started <- struct{}{}

		<-release
		out.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "1")

	provider, err := export.NewLogProvider(t.Context())
	require.NoError(t, err)

	logger := otelslog.NewLogger("test", otelslog.WithLoggerProvider(provider))
	logger.ErrorContext(t.Context(), "first record")

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}

	emitted := make(chan struct{})

	go func() {
		for range 4096 {
			logger.ErrorContext(t.Context(), "queued record")
		}

		close(emitted)
	}()

	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("log emission waited for export")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, provider.Shutdown(ctx), context.DeadlineExceeded)
}

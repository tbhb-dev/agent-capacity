// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

func loggingEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AGENT_CAPACITY_CLAUDE_TRANSCRIPTS", "")
	t.Setenv("AGENT_CAPACITY_SPOOL_DIR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "host.name=test-host")
	t.Setenv("OTEL_SERVICE_NAME", "test-service")

	previous := otel.GetErrorHandler()

	t.Cleanup(func() { otel.SetErrorHandler(previous) })
}

func TestLoggingWithoutExporter(t *testing.T) {
	loggingEnvironment(t)

	for _, args := range [][]string{nil, {versionCommand}, {serveCommand}} {
		var logs bytes.Buffer

		code := runWithLogging(t.Context(), args, io.Discard, slog.New(slog.NewJSONHandler(&logs, nil)))
		if len(args) > 0 && args[0] == versionCommand {
			assert.Equal(t, 0, code)
			assert.Empty(t, logs.String())
		} else {
			assert.Equal(t, 1, code)
			assert.NotContains(t, logs.String(), "log setup failed")
		}
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", unknownCommand)

	var logs bytes.Buffer

	code := runWithLogging(t.Context(), []string{versionCommand}, io.Discard,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	assert.Equal(t, 0, code)
	assert.Empty(t, logs.String())

	code = runWithLogging(t.Context(), []string{serveCommand}, io.Discard,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	assert.Equal(t, 1, code)
	assert.Contains(t, logs.String(), "log setup failed")
	assert.Contains(t, logs.String(), "spool directory is required")
}

func TestCollectorLogExport(t *testing.T) {
	loggingEnvironment(t)

	requests := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/v1/logs", request.URL.Path)
		assert.Equal(t, "application/x-protobuf", request.Header.Get("Content-Type"))
		assert.Equal(t, "synthetic-token", request.Header.Get("Authorization"))

		body, err := io.ReadAll(request.Body)
		assert.NoError(t, err)

		requests <- body

		out.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(server.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=synthetic-token")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-sample.json"),
		[]byte(`{"token":"sample-token"}`), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var logs bytes.Buffer

	code := runWithLogging(ctx, []string{serveCommand, "--spool-dir", dir}, io.Discard,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	assert.Equal(t, 0, code)
	assert.Contains(t, logs.String(), "snapshot read failed")
	assert.Contains(t, logs.String(), "source read failed")

	require.Len(t, requests, 1)
	body := <-requests

	for _, want := range []string{
		"test-service", "host.name", "test-host", "snapshot read failed",
		"source read failed", "class", "source", "claude_statusline",
	} {
		assert.Contains(t, string(body), want)
	}

	assert.NotContains(t, string(body), "sample")
	assert.NotContains(t, string(body), "synthetic-token")
	assert.NotContains(t, logs.String(), "sample")
}

func TestLogExportFailure(t *testing.T) {
	loggingEnvironment(t)

	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		assert.NoError(t, err)

		requests <- string(body)

		http.Error(out, "fixture-receiver-error", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL+"/custom-logs")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-example.json"), []byte("sample-token"), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var logs bytes.Buffer

	code := runWithLogging(ctx, []string{serveCommand, "--spool-dir", dir}, io.Discard,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	assert.Equal(t, 0, code)
	assert.Contains(t, logs.String(), "log shutdown failed")
	assert.NotContains(t, logs.String(), "fixture-receiver-error")
	require.Len(t, requests, 1)
	assert.NotContains(t, <-requests, "log shutdown failed")
}

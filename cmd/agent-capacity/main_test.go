// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/export"
)

const (
	serveCommand    = "serve"
	processTest     = "AGENT_CAPACITY_PROCESS_TEST"
	versionCommand  = "version"
	extraArgument   = "extra"
	invalidCommand  = "invalid command"
	invalidInterval = "--interval=invalid"
	codexSource     = "codex_rollout"
	readClass       = "read"
	invalidClass    = "invalid"
)

func TestMain(tests *testing.M) {
	if os.Getenv(processTest) == "1" {
		main()
		os.Exit(99)
	}

	os.Exit(tests.Run())
}

func TestProcess(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)

	for _, testCase := range []struct {
		output  string
		message string
		args    []string
		code    int
	}{
		{"agent-capacity 0.0.0-dev\n", "", []string{versionCommand}, 0},
		{"", invalidCommand, nil, 1},
		{"", invalidCommand, []string{serveCommand, invalidInterval}, 1},
		{"", "setup failed", []string{serveCommand}, 1},
	} {
		command := exec.CommandContext(t.Context(), executable, testCase.args...)

		command.Env = append(
			os.Environ(),
			processTest+"=1",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=unknown",
			"AGENT_CAPACITY_SPOOL_DIR="+t.TempDir(),
		)

		var out, logs bytes.Buffer

		command.Stdout = &out
		command.Stderr = &logs

		runErr := command.Run()
		if testCase.code == 0 {
			require.NoError(t, runErr)
		} else {
			var exitErr *exec.ExitError
			require.ErrorAs(t, runErr, &exitErr)
			assert.Equal(t, testCase.code, exitErr.ExitCode())
		}

		assert.Equal(t, testCase.output, out.String())
		assertLogs(t, &logs, testCase.message)
	}
}

func TestParseInterval(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		args []string
		want time.Duration
	}{
		{[]string{serveCommand}, time.Minute},
		{[]string{serveCommand, "--interval=1ns"}, time.Nanosecond},
		{[]string{serveCommand, "--interval=2s"}, 2 * time.Second},
	} {
		got, _, err := parseServe(testCase.args)
		require.NoError(t, err)
		assert.Equal(t, testCase.want, got)
	}

	for _, args := range [][]string{
		nil,
		{versionCommand},
		{serveCommand, invalidInterval},
		{serveCommand, "--interval=0"},
		{serveCommand, "--interval=-1ns"},
		{serveCommand, extraArgument},
		{serveCommand, "--endpoint=unused"},
	} {
		got, _, err := parseServe(args)
		require.EqualError(t, err, "invalid arguments")
		assert.Zero(t, got)
	}
}

func TestRuntimeSetup(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "unknown")

	previous := otel.GetErrorHandler()

	t.Cleanup(func() { otel.SetErrorHandler(previous) })

	var logs bytes.Buffer

	runtime := newRuntime(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
	assert.Len(t, runtime.Registry(t.TempDir()), 1)

	provider, err := runtime.NewProvider(t.Context())
	require.ErrorIs(t, err, export.ErrProtocol)
	assert.Nil(t, provider)

	otel.Handle(errors.ErrUnsupported)
	assertLogs(t, &logs, "export failed")
}

func assertLogs(t *testing.T, logs *bytes.Buffer, message string) {
	t.Helper()

	decoder := json.NewDecoder(logs)

	if message != "" {
		var entry map[string]any
		require.NoError(t, decoder.Decode(&entry))
		assert.Equal(t, message, entry["msg"])
		assert.Equal(t, "ERROR", entry["level"])

		class := readClass
		if message == invalidCommand {
			class = "arguments"
		}

		assert.Equal(t, class, entry["class"])
		assert.Len(t, entry, 4)
	}

	var trailing any
	require.ErrorIs(t, decoder.Decode(&trailing), io.EOF)
}

type provider struct {
	metric.MeterProvider

	shutdownErr error
	deadline    time.Time
	shutdownAt  time.Time
	stopped     bool
}

func (stub *provider) Shutdown(ctx context.Context) error {
	stub.stopped = true
	stub.deadline, _ = ctx.Deadline()
	stub.shutdownAt = time.Now()

	if ctx.Err() != nil {
		return errors.Join(ctx.Err())
	}

	return stub.shutdownErr
}

type failingMeter struct{ metric.Meter }

//nolint:ireturn // The SDK requires this interface return type.
func (failingMeter) Float64ObservableGauge(
	string,
	...metric.Float64ObservableGaugeOption,
) (metric.Float64ObservableGauge, error) {
	return nil, errors.ErrUnsupported
}

type failingProvider struct{ metric.MeterProvider }

//nolint:ireturn // The SDK requires this interface return type.
func (failingProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return failingMeter{Meter: nil}
}

type runCase struct {
	output      string
	args        []string
	want        int
	setupErr    bool
	meterErr    bool
	shutdownErr bool
}

func runCases() []runCase {
	return []runCase{
		{
			args:        []string{versionCommand},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        0,
			output:      "agent-capacity 0.0.0-dev\n",
		},
		{args: nil, setupErr: false, meterErr: false, shutdownErr: false, want: 1, output: ""},
		{args: []string{"unknown"}, setupErr: false, meterErr: false, shutdownErr: false, want: 1, output: ""},
		{
			args:        []string{versionCommand, extraArgument},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        1,
			output:      "",
		},
		{
			args:        []string{serveCommand, invalidInterval},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        1,
			output:      "",
		},
		{
			args:        []string{serveCommand, "--interval=0"},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        1,
			output:      "",
		},
		{
			args:        []string{serveCommand, "--interval=-1s"},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        1,
			output:      "",
		},
		{
			args:        []string{serveCommand, extraArgument},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        1,
			output:      "",
		},
		{
			args:        []string{serveCommand, "--endpoint=unused"},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        1,
			output:      "",
		},
		{args: []string{serveCommand}, setupErr: true, meterErr: false, shutdownErr: false, want: 1, output: ""},
		{args: []string{serveCommand}, setupErr: false, meterErr: true, shutdownErr: false, want: 1, output: ""},
		{args: []string{serveCommand}, setupErr: false, meterErr: false, shutdownErr: true, want: 1, output: ""},
		{args: []string{serveCommand}, setupErr: false, meterErr: false, shutdownErr: false, want: 0, output: ""},
		{
			args:        []string{serveCommand, "--interval=2s"},
			setupErr:    false,
			meterErr:    false,
			shutdownErr: false,
			want:        0,
			output:      "",
		},
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := runCases()

	for _, testCase := range cases {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		var out, logs bytes.Buffer

		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		sdk := sdkmetric.NewMeterProvider()

		t.Cleanup(func() { require.NoError(t, sdk.Shutdown(context.Background())) })

		stub := &provider{
			MeterProvider: sdk, shutdownErr: nil, stopped: false,
			deadline: time.Time{}, shutdownAt: time.Time{},
		}
		if testCase.meterErr {
			stub.MeterProvider = failingProvider{MeterProvider: nil}
		}

		if testCase.shutdownErr {
			stub.shutdownErr = errors.ErrUnsupported
		}

		calls := 0
		runtime := Runtime{
			Registry: func(dir string) capacity.Registry {
				assert.Equal(t, filepath.Join("spool", "test"), dir)

				return capacity.Registry{{Name: codexSource, Source: sourceFunc(
					func(got context.Context, _ time.Time) ([]capacity.Reading, error) {
						assert.Equal(t, ctx, got)

						calls++

						return nil, nil
					},
				)}}
			},
			NewProvider: func(context.Context) (Provider, error) {
				if testCase.setupErr {
					return nil, errors.ErrUnsupported
				}

				return stub, nil
			},
		}
		started := time.Now()

		assert.Equal(t, testCase.want, runtime.Run(ctx, withSpool(testCase.args), &out, logger))
		assert.Equal(t, testCase.output, out.String())
		assertRunState(t, testCase, stub, calls, started)
		assertLogs(t, &logs, testCase.message())
	}
}

func assertRunState(t *testing.T, testCase runCase, stub *provider, calls int, started time.Time) {
	t.Helper()

	if testCase.want == 0 && testCase.output == "" || testCase.meterErr || testCase.shutdownErr {
		assert.True(t, stub.stopped)
		assert.WithinRange(t, stub.deadline, started.Add(5*time.Second), stub.shutdownAt.Add(5*time.Second))

		if testCase.meterErr {
			assert.Zero(t, calls)
		} else {
			assert.Equal(t, 1, calls)
		}
	} else {
		assert.False(t, stub.stopped)
		assert.Zero(t, calls)
	}
}

func (testCase runCase) message() string {
	switch {
	case testCase.setupErr || testCase.meterErr:
		return "setup failed"
	case testCase.shutdownErr:
		return "shutdown failed"
	case testCase.want != 0:
		return invalidCommand
	default:
		return ""
	}
}

type sourceFunc func(context.Context, time.Time) ([]capacity.Reading, error)

func (f sourceFunc) Read(ctx context.Context, now time.Time) ([]capacity.Reading, error) {
	return f(ctx, now)
}

func TestServe(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	now := time.Unix(100000, 0)
	reader := sdkmetric.NewManualReader()
	sdk := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, sdk.Shutdown(context.Background())) })

	collector, err := export.New(sdk.Meter("test"), func() time.Time { return now })
	require.NoError(t, err)

	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	calls := 0
	registry := capacity.Registry{
		{Name: "claude_statusline", Source: sourceFunc(func(context.Context, time.Time) ([]capacity.Reading, error) {
			return nil, errors.ErrUnsupported
		})},
		{
			Name: codexSource,
			Source: sourceFunc(func(got context.Context, observed time.Time) ([]capacity.Reading, error) {
				assert.Equal(t, ctx, got)
				assert.Equal(t, now, observed)

				calls++
				if calls == 2 {
					cancel()
				}

				return []capacity.Reading{{
					Vendor: "openai", Window: "5h", Account: "work", Source: codexSource,
					UsedRatio: float64(calls) / 10, ResetAt: time.Time{}, ObservedAt: observed,
				}}, nil
			}),
		},
	}

	ticks := make(chan time.Time, 1)
	ticks <- now

	Serve(ctx, registry, collector, logger, ticks, func() time.Time { return now })
	assert.Equal(t, 2, calls)

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &metrics))
	require.Len(t, metrics.ScopeMetrics, 1)
	require.Len(t, metrics.ScopeMetrics[0].Metrics, 2)

	for _, m := range metrics.ScopeMetrics[0].Metrics {
		gauge, ok := m.Data.(metricdata.Gauge[float64])
		require.True(t, ok)
		require.Len(t, gauge.DataPoints, 1)

		if m.Name == "afakt.capacity.window.utilization" {
			assert.InDelta(t, 0.2, gauge.DataPoints[0].Value, 0)
		}
	}

	decoder := json.NewDecoder(&logs)

	for range 2 {
		var entry map[string]any
		require.NoError(t, decoder.Decode(&entry))
		assert.Equal(t, "claude_statusline", entry["source"])
		assert.Equal(t, readClass, entry["class"])
		assert.NotContains(t, entry, "error")
	}

	var trailing any
	require.ErrorIs(t, decoder.Decode(&trailing), io.EOF)
}

func withSpool(args []string) []string {
	if len(args) > 0 && args[0] == serveCommand {
		return append([]string{serveCommand, "--spool-dir=" + filepath.Join("spool", "test")}, args[1:]...)
	}

	return args
}

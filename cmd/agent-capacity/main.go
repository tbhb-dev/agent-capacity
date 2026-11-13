// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Command agent-capacity exports plan capacity metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/export"
	"go.tbhb.dev/agent-capacity/internal/source/agy"
	"go.tbhb.dev/agent-capacity/internal/source/claude"
	"go.tbhb.dev/agent-capacity/internal/source/codex"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

// Provider supplies a meter and flushes on shutdown.
type Provider interface {
	metric.MeterProvider
	Shutdown(ctx context.Context) error
}

// Runtime keeps source registration and SDK creation at the command boundary.
type Runtime struct {
	NewRegistry func(string, string) (capacity.Registry, error)
	NewProvider func(context.Context) (Provider, error)
	Registry    func(string) capacity.Registry
}

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	runtime := newRuntime(ctx, logger)

	return runtime.Run(ctx, os.Args[1:], os.Stdout, logger)
}

func newRuntime(ctx context.Context, logger *slog.Logger) Runtime {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.ErrorContext(ctx, "export failed", "class", capacity.ErrorClass(err))
	}))

	return Runtime{
		NewRegistry: codex.Register,
		Registry: func(dir string) capacity.Registry {
			return capacity.Registry{
				{Name: capacity.ClaudeStatusline, Source: claude.Source{Dir: dir, Logger: logger}},
				{Name: capacity.AGYStatusline, Source: agy.Source{Dir: dir, Logger: logger}},
			}
		},
		NewProvider: func(ctx context.Context) (Provider, error) { return export.NewProvider(ctx) },
	}
}

const shutdownTimeout = 5 * time.Second

var errArguments = errors.New("invalid arguments")

// Run executes serve or version. Errors log fixed messages and safe classes.
func (r Runtime) Run(ctx context.Context, args []string, out io.Writer, logger *slog.Logger) int {
	if code, handled := runCommand(ctx, args, out, logger); handled {
		return code
	}

	options, err := parseServe(args)
	if err != nil {
		logger.ErrorContext(ctx, "invalid command", "class", "arguments")

		return 1
	}

	if options.spoolDir == "" {
		logger.ErrorContext(ctx, spool.ErrDirectory.Error(), "class", "arguments")

		return 1
	}

	registry := r.Registry(options.spoolDir)
	if r.NewRegistry != nil {
		codexRegistry, registryErr := r.NewRegistry(options.codexHome, options.codexAccount)
		if registryErr != nil {
			logger.ErrorContext(ctx, "invalid command", "class", "arguments")

			return 1
		}

		registry = append(registry, codexRegistry...)
	}

	transcripts, transcriptErr := claude.RegisterTranscripts(options.claudeTranscripts, options.claudeAccount)
	if transcriptErr != nil {
		logger.ErrorContext(ctx, "invalid command", "class", "arguments")

		return 1
	}

	registry = append(registry, transcripts...)

	provider, err := r.NewProvider(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "setup failed", "class", capacity.ErrorClass(err))

		return 1
	}

	collector, err := export.New(provider.Meter("go.tbhb.dev/agent-capacity"), time.Now)
	if err == nil {
		ticker := time.NewTicker(options.interval)
		defer ticker.Stop()

		Serve(ctx, registry, collector, logger, ticker.C, time.Now)
	} else {
		logger.ErrorContext(ctx, "setup failed", "class", capacity.ErrorClass(err))
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	shutdownErr := provider.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		logger.ErrorContext(ctx, "shutdown failed", "class", capacity.ErrorClass(shutdownErr))
	}

	if errors.Join(err, shutdownErr) != nil {
		return 1
	}

	return 0
}

func runCommand(ctx context.Context, args []string, out io.Writer, logger *slog.Logger) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}

	switch args[0] {
	case "version":
		if len(args) != 1 {
			return 0, false
		}

		fmt.Fprintln(out, "agent-capacity 0.0.0-dev")

		return 0, true
	case "statusline":
		return statusline(ctx, args[1:], os.Stdin, logger), true
	case "login":
		return loginCommand(args[1:], os.Stdin, out, os.Stderr), true
	case "session":
		return sessionCommand(ctx, args[1:], out), true
	default:
		return 0, false
	}
}

type serveOptions struct {
	claudeTranscripts string
	claudeAccount     string
	spoolDir          string
	codexHome         string
	codexAccount      string
	interval          time.Duration
}

func parseServe(args []string) (serveOptions, error) {
	if len(args) == 0 || args[0] != "serve" {
		return serveOptions{}, errArguments
	}

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexHome := flags.String("codex-home", os.Getenv("CODEX_HOME"), "Codex rollout home")
	codexAccount := flags.String("codex-account", "primary", "Codex account alias")
	transcripts := flags.String(
		"claude-transcripts",
		os.Getenv("AGENT_CAPACITY_CLAUDE_TRANSCRIPTS"),
		"transcript files or directories, separated by commas",
	)
	claudeAccount := flags.String("claude-account", "primary", "Claude transcript account alias")
	dir := flags.String("spool-dir", os.Getenv("AGENT_CAPACITY_SPOOL_DIR"), "snapshot directory")
	interval := flags.Duration("interval", time.Minute, "source read interval")

	err := flags.Parse(args[1:])
	if err != nil {
		return serveOptions{}, errArguments
	}

	if flags.NArg() != 0 || *interval <= 0 {
		return serveOptions{}, errArguments
	}

	return serveOptions{
		interval:          *interval,
		spoolDir:          *dir,
		codexHome:         *codexHome,
		codexAccount:      *codexAccount,
		claudeTranscripts: *transcripts,
		claudeAccount:     *claudeAccount,
	}, nil
}

// Serve collects immediately and after each tick until the context is canceled.
// Failed sources contribute no readings. Other sources continue to be read.
func Serve(ctx context.Context, registry capacity.Registry, collector *export.Collector,
	logger *slog.Logger, ticks <-chan time.Time, now func() time.Time,
) {
	for {
		readings, failures := registry.Read(ctx, now())
		for _, failure := range failures {
			logger.ErrorContext(ctx, "source read failed", "source", failure.Source, "class", failure.Class)
		}
		// Registry validates each batch. Replace also guards direct callers.
		_ = collector.Replace(readings) //nolint:errcheck // Registry validates every reading before Replace.

		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

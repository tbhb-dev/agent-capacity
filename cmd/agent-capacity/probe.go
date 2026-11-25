// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/source/agy"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

// The usage command waits up to 100 seconds for its own answer.
const probeTimeout = 2 * time.Minute

// usageWaitDelay bounds the wait for the output pipe after the deadline kills the command.
// A child process of the command could otherwise hold the pipe open past the deadline.
const usageWaitDelay = 5 * time.Second

// usageCommand builds the request to the Antigravity command on PATH for quota, which neither starts an agent turn
// nor spends quota. It inherits the environment. The caller picks the account through the home directory.
func usageCommand(ctx context.Context) *exec.Cmd {
	command := exec.CommandContext(
		ctx, "agy", "-p", "/usage", "--output-format", "json", "--print-timeout", "100s",
	)
	command.WaitDelay = usageWaitDelay

	return command
}

// runUsage asks the Antigravity command for quota.
func runUsage(ctx context.Context) ([]byte, error) {
	return runCommand(ctx, usageCommand(ctx))
}

// runCommand keeps at most one byte past the size limit of the output, which the caller rejects.
// Errors keep their cause for logging: a missing command counts as a missing file,
// and a command killed at the deadline or on cancel reports the context's error
// instead of the signal.
func runCommand(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	output := &limitedBuffer{data: nil, limit: maxStatusline + 1}
	command.Stdout = output

	err := command.Run()

	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, fmt.Errorf("run usage command: %w", ctx.Err())
	case errors.Is(err, exec.ErrNotFound):
		return nil, fmt.Errorf("run usage command: %w: %w", fs.ErrNotExist, err)
	default:
		return nil, fmt.Errorf("run usage command: %w", err)
	}

	return output.data, nil
}

// limitedBuffer keeps the first limit bytes written to it and drops the rest without an error.
type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), b.limit-len(b.data))
	b.data = append(b.data, p[:keep]...)

	return len(p), nil
}

func (r Runtime) probe(ctx context.Context, args []string, logger *slog.Logger) int {
	err := r.probeUsage(ctx, args, time.Now())
	if err == nil {
		return 0
	}

	message := "probe failed"
	if errors.Is(err, spool.ErrDirectory) {
		message = spool.ErrDirectory.Error()
	}

	logger.ErrorContext(ctx, message, "class", capacity.ErrorClass(err))

	return 1
}

// probeUsage captures the selected vendor's usage for one account.
func (r Runtime) probeUsage(ctx context.Context, args []string, now time.Time) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("spool-dir", os.Getenv("AGENT_CAPACITY_SPOOL_DIR"), "snapshot directory")

	vendor := flags.String("vendor", "agy", "probe vendor")

	eventsFile := flags.String("events-file", "", "Claude rate limit events file")

	account := flags.String("account", "", "account alias")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !spool.ValidIdentity(*vendor, *account) {
		return capacity.ErrInvalidReading
	}

	if *vendor == "claude" {
		return r.probeClaude(ctx, *eventsFile)
	}

	if *dir == "" {
		return spool.ErrDirectory
	}

	if *vendor == "codex" {
		return probeCodex(ctx, *dir, *account, now)
	}

	if r.Usage == nil {
		return capacity.ErrInvalidReading
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	output, err := r.Usage(ctx)
	if err != nil {
		return err
	}

	var payload struct {
		Status  string `json:"status"`
		Command struct {
			Name string          `json:"name"`
			Data json.RawMessage `json:"data"`
		} `json:"command"`
	}
	if len(output) > maxStatusline || json.Unmarshal(output, &payload) != nil ||
		payload.Status != "SUCCESS" || payload.Command.Name != "usage" {
		return capacity.ErrInvalidReading
	}

	snapshot := spool.Snapshot{Vendor: "agy", Account: *account, ObservedAt: now, Data: payload.Command.Data}

	_, err = agy.Parse(snapshot)
	if err != nil {
		return fmt.Errorf("check usage data: %w", err)
	}

	err = spool.Write(*dir, snapshot)
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}

	return nil
}

func runClaudeUsage(ctx context.Context) ([]byte, error) {
	command := exec.CommandContext(
		ctx, "claude", "-p", "Reply with the single word ok.", "--model", "haiku", "--max-turns", "1",
		"--output-format", "stream-json", "--verbose",
	)
	command.WaitDelay = usageWaitDelay

	return runCommand(ctx, command)
}

func (r Runtime) probeClaude(ctx context.Context, path string) error {
	if path == "" || r.ClaudeUsage == nil {
		return capacity.ErrInvalidReading
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	output, err := r.ClaudeUsage(ctx)
	if err != nil {
		return err
	}

	if len(output) > maxStatusline {
		return capacity.ErrInvalidReading
	}

	var events []byte

	for line := range bytes.SplitSeq(output, []byte("\n")) {
		var event map[string]json.RawMessage
		if json.Unmarshal(line, &event) != nil {
			continue
		}

		var kind string
		if json.Unmarshal(event["type"], &kind) == nil && kind == "rate_limit_event" {
			events = append(events, line...)
			events = append(events, '\n')
		}
	}

	if len(events) == 0 {
		return capacity.ErrInvalidReading
	}

	return writeClaudeEvents(path, events, os.CreateTemp)
}

func writeClaudeEvents(path string, events []byte, create func(string, string) (*os.File, error)) error {
	file, err := create(filepath.Dir(path), ".events-*")
	if err != nil {
		return fmt.Errorf("create events file: %w", err)
	}
	defer os.Remove(file.Name())

	_, writeErr := file.Write(events)

	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return err
	}

	return errors.Join(os.Rename(file.Name(), path))
}

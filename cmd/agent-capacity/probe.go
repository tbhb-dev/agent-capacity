// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
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
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/source/agy"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

// The usage command waits up to 100 seconds for its own answer.
const probeTimeout = 2 * time.Minute

// runUsage asks the Antigravity command on PATH for quota, which neither starts an agent turn nor spends quota.
// It inherits the environment. The caller picks the account through the home directory.
// Errors keep their cause for logging: a missing command counts as a missing file,
// and a command killed at the deadline or on cancel reports the context's error
// instead of the signal.
func runUsage(ctx context.Context) ([]byte, error) {
	output, err := exec.CommandContext(
		ctx, "agy", "-p", "/usage", "--output-format", "json", "--print-timeout", "100s",
	).Output()

	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, fmt.Errorf("run usage command: %w", ctx.Err())
	case errors.Is(err, exec.ErrNotFound):
		return nil, fmt.Errorf("run usage command: %w: %w", fs.ErrNotExist, err)
	default:
		return nil, fmt.Errorf("run usage command: %w", err)
	}

	return output, nil
}

func probe(ctx context.Context, args []string, usage func(context.Context) ([]byte, error), logger *slog.Logger) int {
	err := probeUsage(ctx, args, usage, time.Now())
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

// probeUsage stores only the usage command's data object for one account. It
// keeps the last good snapshot when the new data doesn't parse.
func probeUsage(ctx context.Context, args []string, usage func(context.Context) ([]byte, error), now time.Time) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("spool-dir", os.Getenv("AGENT_CAPACITY_SPOOL_DIR"), "snapshot directory")

	account := flags.String("account", "", "account alias")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !spool.ValidIdentity("agy", *account) {
		return capacity.ErrInvalidReading
	}

	if *dir == "" {
		return spool.ErrDirectory
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	output, err := usage(ctx)
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

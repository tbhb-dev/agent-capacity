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
	"log/slog"
	"os"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

const maxStatusline = 1 << 20

func statusline(ctx context.Context, args []string, input io.Reader, logger *slog.Logger) int {
	err := capture(args, input, time.Now())
	if err != nil {
		message := "statusline capture failed"
		if errors.Is(err, spool.ErrDirectory) {
			message = spool.ErrDirectory.Error()
		}

		logger.ErrorContext(ctx, message, "class", capacity.ErrorClass(err))
	}

	return 0
}

func capture(args []string, input io.Reader, now time.Time) error {
	flags := flag.NewFlagSet("statusline", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("spool-dir", os.Getenv("AGENT_CAPACITY_SPOOL_DIR"), "snapshot directory")
	vendor := flags.String("vendor", "", "snapshot vendor")

	account := flags.String("account", "", "account alias")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !spool.ValidIdentity(*vendor, *account) {
		return capacity.ErrInvalidReading
	}

	if *dir == "" {
		return spool.ErrDirectory
	}

	data, err := io.ReadAll(io.LimitReader(input, maxStatusline+1))
	if err != nil {
		return fmt.Errorf("read statusline: %w", err)
	}

	if len(data) > maxStatusline {
		return capacity.ErrInvalidReading
	}

	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return capacity.ErrInvalidReading
	}

	key := "rate_limits"
	if *vendor == "agy" {
		key = "quota"
	}

	subtree, present := payload[key]
	if !present {
		return nil
	}

	err = spool.Write(
		*dir,
		spool.Snapshot{Vendor: *vendor, Account: *account, ObservedAt: now, Data: subtree},
	)
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}

	return nil
}

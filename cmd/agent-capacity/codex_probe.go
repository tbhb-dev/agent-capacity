// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/source/codex"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

const codexHandshake = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"agent-capacity","version":"0.0.0"}}}
{"method":"initialized"}
{"id":2,"method":"account/rateLimits/read"}
`

func probeCodex(ctx context.Context, dir, account string, now time.Time) error {
	data, err := runCodex(ctx)
	if err != nil {
		return err
	}
	snapshot := spool.Snapshot{Vendor: "codex", Account: account, ObservedAt: now, Data: data}
	if _, err = codex.Parse(snapshot); err != nil {
		return err
	}
	return spool.Write(dir, snapshot)
}

// runCodex keeps standard input open until the rate limit response arrives.
func runCodex(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	input, hold := io.Pipe()
	defer input.Close()
	defer hold.Close()
	command := exec.CommandContext(ctx, "codex", "app-server")
	command.Stdin = io.MultiReader(strings.NewReader(codexHandshake), input)
	command.WaitDelay = usageWaitDelay
	//nolint:errcheck // A new command has no output writer and has not started.
	output, _ := command.StdoutPipe()
	defer output.Close()
	if err := command.Start(); err != nil {
		return nil, err
	}
	data, readErr := readCodex(output)
	contextErr := ctx.Err()
	cancel()
	_ = hold.Close()
	runErr := command.Wait()
	if readErr != nil {
		return nil, errors.Join(readErr, runErr, contextErr)
	}
	return data, nil
}

func readCodex(reader io.Reader) ([]byte, error) {
	decoder := json.NewDecoder(reader)
	for {
		var response struct {
			Result struct {
				RateLimits json.RawMessage `json:"rateLimits"`
			} `json:"result"`
			ID int `json:"id"`
		}
		if err := decoder.Decode(&response); err != nil {
			return nil, capacity.ErrInvalidReading
		}
		if response.ID == 2 {
			return response.Result.RateLimits, nil
		}
	}
}

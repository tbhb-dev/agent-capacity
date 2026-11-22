// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
	"go.tbhb.dev/agent-capacity/internal/source/codex"
	"go.tbhb.dev/agent-capacity/internal/spool"
)

const codexProbeTimeout = 30 * time.Second

func probeCodex(ctx context.Context, dir, account string, now time.Time) error {
	data, err := runCodexUsage(ctx)
	if err != nil {
		return err
	}

	snapshot := spool.Snapshot{Vendor: "codex", Account: account, ObservedAt: now, Data: data}

	_, err = codex.Parse(snapshot)
	if err != nil {
		return fmt.Errorf("check Codex data: %w", err)
	}

	return errors.Join(spool.Write(dir, snapshot))
}

func runCodexUsage(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, codexProbeTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, "codex", "app-server")
	data, err := runCodexCommand(command)

	return data, errors.Join(err, ctx.Err())
}

func runCodexCommand(command *exec.Cmd) ([]byte, error) {
	command.WaitDelay = usageWaitDelay

	input, inputErr := command.StdinPipe()

	output, outputErr := command.StdoutPipe()

	err := errors.Join(inputErr, outputErr)
	if err != nil {
		return nil, err
	}

	defer input.Close()
	defer output.Close()

	err = command.Start()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("codex probe: %w: %w", fs.ErrNotExist, err)
	}

	if err != nil {
		return nil, fmt.Errorf("codex probe: %w", err)
	}

	defer func() {
		// The server stays alive until killed; its exit status is not a probe result.
		_ = command.Process.Kill() //nolint:errcheck // The process can already have exited.
		_ = command.Wait()         //nolint:errcheck // The process is killed after its answer.
	}()

	return codexExchange(input, json.NewDecoder(io.LimitReader(output, maxStatusline)))
}

func codexExchange(input io.Writer, decoder *json.Decoder) ([]byte, error) {
	requests := []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"agent-capacity","version":"0.0.0"}}}` + "\n",
		`{"method":"initialized"}` + "\n" + `{"id":2,"method":"account/rateLimits/read"}` + "\n",
	}

	var result json.RawMessage

	for index, request := range requests {
		_, err := io.WriteString(input, request)
		if err != nil {
			return nil, fmt.Errorf("codex probe: %w", err)
		}

		result, err = codexResult(decoder, index+1)
		if err != nil {
			return nil, fmt.Errorf("codex probe: %w", err)
		}
	}

	var payload struct {
		RateLimits json.RawMessage `json:"rateLimits"`
	}
	if json.Unmarshal(result, &payload) != nil {
		return nil, capacity.ErrInvalidReading
	}

	return payload.RateLimits, nil
}

func codexResult(decoder *json.Decoder, requestID int) (json.RawMessage, error) {
	for {
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
			ID     int             `json:"id"`
		}

		err := decoder.Decode(&response)
		if err != nil {
			return nil, fmt.Errorf("codex probe: %w", err)
		}

		if response.ID != requestID {
			continue
		}

		if response.Error != nil {
			return nil, capacity.ErrInvalidReading
		}

		return response.Result, nil
	}
}

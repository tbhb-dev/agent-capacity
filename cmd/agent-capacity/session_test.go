// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

const (
	runSubcommand    = "run"
	renderSubcommand = "render-settings"
)

func TestSessionCommandArguments(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		nil,
		{"unknown"},
		{runSubcommand},
		{runSubcommand, "--instance=bad"},
		{runSubcommand, "--instance=claude-../bad"},
		{runSubcommand, unknownFlag},
		{renderSubcommand},
		{renderSubcommand, unknownFlag},
		{renderSubcommand, "extra"},
	} {
		var out bytes.Buffer
		if sessionCommand(context.Background(), args, &out) != 2 {
			t.Fatalf("accepted arguments: %v", args)
		}
	}

	var out bytes.Buffer
	if loginCommand([]string{unknownFlag}, os.Stdin, &out, &out) != 2 {
		t.Fatal("login accepted arguments")
	}

	if loginCommand(nil, os.Stdin, &out, &out) != 2 {
		t.Fatal("login accepted missing paths")
	}
}

func TestRenderSettingsCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var out bytes.Buffer

	args := []string{
		"render-settings",
		"--vendor=claude",
		"--account=sample",
		"--binary=" + filepath.Join(dir, "binary"),
		"--spool-dir=" + dir,
	}
	if sessionCommand(context.Background(), args, &out) != 0 {
		t.Fatal("render failed")
	}

	if !bytes.Contains(out.Bytes(), []byte("CronCreate")) {
		t.Fatal("missing permissions")
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const (
	runSubcommand    = "run"
	renderSubcommand = "render-settings"
	instanceFlag     = "--instance=claude-sample"
	accountFlag      = "--account=sample"
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
		{runSubcommand, instanceFlag, extraArgument},
		{runSubcommand, instanceFlag, claudeFlag},
		{runSubcommand, instanceFlag, accountFlag},
		{renderSubcommand},
		{renderSubcommand, unknownFlag},
		{renderSubcommand, extraArgument},
	} {
		var out bytes.Buffer
		if sessionCommand(context.Background(), args, &out) != 2 {
			t.Fatalf("accepted arguments: %v", args)
		}
	}

	var out bytes.Buffer
	if loginCommand(context.Background(), []string{unknownFlag}, os.Stdin, &out, &out) != 2 {
		t.Fatal("login accepted arguments")
	}

	if loginCommand(context.Background(), []string{extraArgument}, os.Stdin, &out, &out) != 2 {
		t.Fatal("login accepted a positional argument")
	}

	if loginCommand(context.Background(), nil, os.Stdin, &out, &out) != 2 {
		t.Fatal("login accepted missing paths")
	}
}

func TestRenderSettingsCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var out bytes.Buffer

	args := []string{
		"render-settings",
		claudeFlag,
		accountFlag,
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

func TestSessionCommandRunsValidIdentity(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	args := []string{runSubcommand, instanceFlag, "--alias-file=/alias", "--alias-root=/root"}
	if sessionCommand(context.Background(), args, &out) != 1 {
		t.Fatal("run accepted a missing spool directory")
	}
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestRenderSettingsWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	args := []string{
		renderSubcommand,
		claudeFlag,
		accountFlag,
		"--binary=" + filepath.Join(dir, "binary"),
		"--spool-dir=" + dir,
	}
	if sessionCommand(context.Background(), args, failingWriter{}) != 1 {
		t.Fatal("render ignored a write failure")
	}
}

func TestRunCommandRoutesSessionCommands(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	for _, args := range [][]string{{"login", unknownFlag}, {"session"}} {
		code, handled := runCommand(context.Background(), args, io.Discard, logger)
		if !handled || code != 2 {
			t.Fatalf("command %v returned %d, %t", args, code, handled)
		}
	}
}

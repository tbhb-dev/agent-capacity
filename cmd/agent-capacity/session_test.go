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
	"slices"
	"testing"
)

const (
	runSubcommand    = "run"
	renderSubcommand = "render-settings"
	instanceFlag     = "--instance=claude-sample"
	accountFlag      = "--account=sample"
	unknownCommand   = "unknown"
	sessionRoute     = "session"
	aliasFileFlag    = "--alias-file=/alias"
	aliasRootFlag    = "--alias-root=/root"
)

func TestSessionCommandArguments(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		nil,
		{unknownCommand},
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

	args := []string{runSubcommand, instanceFlag, aliasFileFlag, aliasRootFlag}
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

	for _, args := range [][]string{{"login", unknownFlag}, {sessionRoute}} {
		code, handled := runCommand(context.Background(), args, io.Discard, logger)
		if !handled || code != 2 {
			t.Fatalf("command %v returned %d, %t", args, code, handled)
		}
	}
}

func TestAccountFlagsDiscardParseOutput(t *testing.T) {
	t.Parallel()

	flags, _ := accountFlags("sample")
	if flags.Output() != io.Discard {
		t.Fatal("flag errors reach the process streams")
	}
}

// loginFixture returns login flags for an approved alias. The alias has a work directory only when work is true.
func loginFixture(t *testing.T, work bool) []string {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	alias := filepath.Join(root, "claude-sample")

	err = os.Mkdir(alias, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	if work {
		err = os.Mkdir(filepath.Join(alias, "work"), 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	aliasFile := filepath.Join(root, "aliases")

	err = os.WriteFile(aliasFile, []byte("claude sample\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return []string{claudeFlag, accountFlag, "--alias-file=" + aliasFile, "--alias-root=" + root}
}

func TestLoginCommandDiagnostics(t *testing.T) {
	t.Parallel()

	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}

	defer input.Close()

	valid := loginFixture(t, true)

	for _, testCase := range []struct {
		name string
		want string
		args []string
	}{
		{"valid arguments", "login requires a terminal\n", valid},
		{"unknown flag", "login refused\n", append(slices.Clone(valid), unknownFlag)},
		{"positional argument", "login refused\n", append(slices.Clone(valid), extraArgument)},
	} {
		var out, diagnostic bytes.Buffer

		code := loginCommand(context.Background(), testCase.args, input, &out, &diagnostic)
		if code != 2 || diagnostic.String() != testCase.want {
			t.Errorf("%s returned %d with diagnostic %q", testCase.name, code, diagnostic.String())
		}
	}
}

func TestRunCommandPassesLoginArguments(t *testing.T) {
	t.Parallel()

	args := append([]string{"login"}, loginFixture(t, false)...)

	code, handled := runCommand(context.Background(), args, io.Discard, slog.New(slog.DiscardHandler))
	if !handled || code != 1 {
		t.Fatalf("login returned %d, %t", code, handled)
	}
}

func TestRunCommandPassesSessionArguments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var out bytes.Buffer

	args := []string{
		sessionRoute,
		renderSubcommand,
		claudeFlag,
		accountFlag,
		"--binary=" + filepath.Join(dir, "binary"),
		"--spool-dir=" + dir,
	}

	code, handled := runCommand(context.Background(), args, &out, slog.New(slog.DiscardHandler))
	if !handled || code != 0 || out.Len() == 0 {
		t.Fatalf("session returned %d, %t with %d bytes", code, handled, out.Len())
	}
}

func TestRunCommandLeavesOtherArgumentsUnhandled(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {versionCommand, extraArgument}, {unknownCommand}} {
		code, handled := runCommand(context.Background(), args, io.Discard, slog.New(slog.DiscardHandler))
		if handled || code != 0 {
			t.Fatalf("command %v returned %d, %t", args, code, handled)
		}
	}
}

func TestRuntimeRunReturnsHandledCode(t *testing.T) {
	t.Parallel()

	runner := Runtime{NewRegistry: nil, Registry: nil, NewProvider: nil}

	code := runner.Run(context.Background(), []string{sessionRoute}, io.Discard, slog.New(slog.DiscardHandler))
	if code != 2 {
		t.Fatalf("session returned %d", code)
	}
}

func TestSessionCommandRefusesInvalidCompanions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	companions := []string{aliasFileFlag, aliasRootFlag}
	renderArgs := []string{
		renderSubcommand, claudeFlag, accountFlag, "--binary=" + filepath.Join(dir, "binary"), "--spool-dir=" + dir,
	}

	for _, args := range [][]string{
		append([]string{unknownCommand, instanceFlag}, companions...),
		append([]string{runSubcommand, instanceFlag}, append(companions, unknownFlag)...),
		append([]string{runSubcommand, instanceFlag}, append(companions, extraArgument)...),
		append([]string{runSubcommand, instanceFlag, claudeFlag}, companions...),
		append([]string{runSubcommand, instanceFlag, accountFlag}, companions...),
		append(slices.Clone(renderArgs), unknownFlag),
		append(slices.Clone(renderArgs), extraArgument),
	} {
		var out bytes.Buffer
		if sessionCommand(context.Background(), args, &out) != 2 || out.Len() != 0 {
			t.Fatalf("accepted arguments: %v", args)
		}
	}

	var out bytes.Buffer

	args := []string{runSubcommand, instanceFlag, "--prompt=ping", aliasFileFlag, aliasRootFlag}
	if sessionCommand(context.Background(), args, &out) != 1 {
		t.Fatal("run refused the prompt flag")
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package session runs vendor processes without inspecting authentication data.
package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creack/pty"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	vendorClaude  = "claude"
	vendorCodex   = "codex"
	commandLogin  = "login"
	commandExec   = "exec"
	phaseRunning  = "running"
	phaseChecking = "checking"
	phaseStopped  = "stopped"
	phaseIdle     = "idle"
	reasonNone    = "none"
	refusalExit   = 2
	privateMode   = 0o600
)

// ErrArguments is safe to print without revealing configuration.
var ErrArguments = errors.New("invalid session arguments")

// Options requires explicit paths and an approved generic alias.
type Options struct {
	Vendor    string
	Account   string
	AliasFile string
	Root      string
	Binary    string
	SpoolDir  string
	Prompt    string
}

// Valid checks identity and configuration without touching vendor files.
func (options Options) Valid() bool {
	return (options.Vendor == vendorClaude || options.Vendor == vendorCodex) && spool.ValidAlias(options.Account) &&
		filepath.IsAbs(options.AliasFile) && filepath.IsAbs(options.Root) &&
		(options.Binary == "" || filepath.IsAbs(options.Binary))
}

// Approved reads only the explicitly supplied allow list.
func (options Options) Approved() (bool, error) {
	if !options.Valid() {
		return false, ErrArguments
	}

	file, err := os.Open(options.AliasFile)
	if err != nil {
		return false, ErrArguments
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == options.Vendor && fields[1] == options.Account {
			return true, nil
		}
	}

	if scanner.Err() != nil {
		return false, ErrArguments
	}

	return false, nil
}

func (options Options) directory() string {
	return filepath.Join(options.Root, options.Vendor+"-"+options.Account)
}

func ownedPrivate(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)

	return ok && info.IsDir() && info.Mode().Perm() == 0o700 && int(stat.Uid) == uid &&
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func (options Options) openAlias() (*os.Root, error) {
	return options.openAliasAs(os.Getuid())
}

func (options Options) openAliasAs(uid int) (*os.Root, error) {
	root, err := spool.SessionRoot(options.directory())
	if err != nil {
		return nil, ErrArguments
	}

	info, err := root.Stat(".")
	if err != nil || !ownedPrivate(info, uid) {
		_ = root.Close()

		return nil, ErrArguments
	}

	return root, nil
}

// workDirectory rejects a missing or linked work directory.
func workDirectory(alias *os.Root) error {
	info, err := alias.Lstat("work")
	if err != nil || !info.IsDir() {
		return ErrArguments
	}

	return nil
}

func (options Options) environment() []string {
	dir := options.directory()

	env := []string{"PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "HOME=" + filepath.Join(dir, "home")}
	if options.Vendor == vendorClaude {
		return append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "home", ".config"),
			"CLAUDE_CONFIG_DIR="+filepath.Join(dir, "config"), "CLAUDE_CODE_AUTO_COMPACT_WINDOW=100000")
	}

	return append(
		env,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "home", ".config"),
		"CODEX_HOME="+filepath.Join(dir, vendorCodex),
	)
}

func (options Options) command(args ...string) *exec.Cmd {
	binary := options.Binary
	if binary == "" {
		binary = options.Vendor
	}

	//nolint:noctx // Process groups use the runner's termination deadline.
	cmd := exec.Command(binary, args...)

	cmd.Dir = filepath.Join(options.directory(), "work")
	cmd.Env = options.environment()

	return cmd
}

// Login delegates all authentication to the vendor and forwards terminal streams. Refusals print one fixed line.
// It never captures vendor output.
func Login(options Options, input *os.File, output, diagnostic io.Writer) int {
	return loginAs(options, input, output, diagnostic, os.Getuid())
}

func loginAs(options Options, input *os.File, output, diagnostic io.Writer, uid int) int {
	approved, err := options.Approved()
	if err != nil || !approved {
		fmt.Fprintln(diagnostic, "login refused")

		return refusalExit
	}

	root, err := options.openAliasAs(uid)
	if err != nil {
		fmt.Fprintln(diagnostic, "login refused")

		return refusalExit
	}
	defer root.Close()

	_, err = pty.GetsizeFull(input)
	if err != nil {
		fmt.Fprintln(diagnostic, "login requires a terminal")

		return refusalExit
	}

	args := []string{"auth", commandLogin}
	if options.Vendor == vendorCodex {
		args = []string{commandLogin, "--device-auth"}
	}

	cmd := options.command(args...)
	cmd.Dir = options.directory()

	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, diagnostic
	if cmd.Run() != nil {
		return 1
	}

	fmt.Fprintf(output, "%s login finished\n", options.Account)

	return 0
}

func lockAlias(root *os.Root) (*os.File, error) {
	file, err := root.OpenFile(".runner.lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, privateMode)
	if err != nil {
		return nil, ErrArguments
	}

	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = file.Close()

		return nil, ErrArguments
	}

	return file, nil
}

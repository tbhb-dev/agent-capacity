// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package session runs vendor processes without inspecting authentication data.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	maxAllowListBytes = 64 << 10
	vendorClaude      = "claude"
	vendorCodex       = "codex"
	commandLogin      = "login"
	commandExec       = "exec"
	phaseRunning      = "running"
	phaseChecking     = "checking"
	phaseStopped      = "stopped"
	phaseIdle         = "idle"
	reasonNone        = "none"
	reasonInterrupted = "interrupted"
	reasonApproval    = "approval_unavailable"
	refusalExit       = 2
	privateMode       = 0o600
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
	Exporter  string
	SpoolDir  string
	Prompt    string
}

// Valid checks identity and configuration without touching vendor files.
func (options Options) Valid() bool {
	return (options.Vendor == vendorClaude || options.Vendor == vendorCodex) && spool.ValidAlias(options.Account) &&
		filepath.IsAbs(options.AliasFile) && filepath.IsAbs(options.Root) &&
		(options.Binary == "" || filepath.IsAbs(options.Binary)) &&
		(options.Exporter == "" || filepath.IsAbs(options.Exporter))
}

// Approved reads only the explicitly supplied allow list.
func (options Options) Approved() (bool, error) {
	if !options.Valid() {
		return false, ErrArguments
	}

	data, err := readAllowList(options.AliasFile)
	if err != nil {
		return false, ErrArguments
	}

	for line := range strings.Lines(string(data)) {
		// gomutants:disable-next-line STATEMENT_REMOVE reason="Fields skips the whitespace."
		line = strings.TrimSpace(line)
		// gomutants:disable-next-line BRANCH_IF reason="Never matches."
		if strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == options.Vendor && fields[1] == options.Account {
			return true, nil
		}
	}

	return false, nil
}

// readAllowList reads a regular file without following links or blocking on a pipe.
// The directory path itself is opened as given, so a symlinked directory is followed; only the file inside is
// confined to it. The operator creates the allow list directory, and the check accepts it as given.
func readAllowList(path string) ([]byte, error) {
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, ErrArguments
	}

	defer dir.Close()

	data, err := spool.ReadBounded(dir, filepath.Base(path), maxAllowListBytes)
	if err != nil {
		return nil, ErrArguments
	}

	return data, nil
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

func realDirectory(alias *os.Root, name string, optional bool) error {
	info, err := alias.Lstat(name)
	if optional && errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil || !info.IsDir() {
		return ErrArguments
	}

	return nil
}

func checkDirectories(alias *os.Root, vendor string) error {
	vendorDirectory := "config"
	if vendor == vendorCodex {
		vendorDirectory = vendorCodex
	}

	if realDirectory(alias, "work", false) != nil {
		return ErrArguments
	}

	for _, name := range []string{"home", vendorDirectory} {
		if realDirectory(alias, name, true) != nil {
			return ErrArguments
		}
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
func Login(ctx context.Context, options Options, input *os.File, output, diagnostic io.Writer) int {
	return loginAs(ctx, options, input, output, diagnostic, os.Getuid())
}

func loginAs(ctx context.Context, options Options, input *os.File, output, diagnostic io.Writer, uid int) int {
	// Approved returns false on every error, and a failed approval takes the same refusal as an unlisted alias.
	// The refusal prints the fixed diagnostic line and exits with the refusal code.
	approved, _ := options.Approved() //nolint:errcheck // The false result carries the error.
	if !approved {
		fmt.Fprintln(diagnostic, "login refused")

		return refusalExit
	}

	root, err := options.openAliasAs(uid)
	if err != nil {
		fmt.Fprintln(diagnostic, "login refused")

		return refusalExit
	}
	defer root.Close()

	if checkDirectories(root, options.Vendor) != nil {
		fmt.Fprintln(diagnostic, "login refused")

		return refusalExit
	}

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
	if waitLogin(ctx, cmd) != nil {
		return 1
	}

	fmt.Fprintf(output, "%s login finished\n", options.Account)

	return 0
}

func waitLogin(ctx context.Context, cmd *exec.Cmd) error {
	cmd.WaitDelay = terminationGrace

	if cmd.Start() != nil {
		return ErrArguments
	}

	// The child shares the caller's terminal, so a separate process group would stop it on terminal input. Its
	// descendants are listed for as long as it runs instead, and killed by process ID after a canceled login ends.
	// One listing at the cancel would miss the children of a child that the same terminal signal already ended.
	tracker := &process{
		cmd: cmd, done: nil, terminal: nil, drained: nil, ended: make(chan struct{}), swept: make(chan struct{}),
		cleaned: nil, members: nil, mutex: sync.Mutex{}, reaped: atomic.Bool{}, listFailed: atomic.Bool{},
		unread: map[int]bool{},
	}

	go tracker.sweepWhileRunning()

	// gomutants:disable-next-line INTEGER_DECREMENT,INTEGER_INCREMENT reason="Size is moot."
	done := make(chan error, 1)

	go func() {
		err := cmd.Wait()

		// The wait delay ends the copy of output a descendant still holds open, and reports it as an error even
		// though the child itself exited cleanly.
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}

		// From the reap on, a listing by the child's ID is dropped, because the ID may belong to another process.
		// The flag is set before any lock is taken, so a cancel that waits on a listing still sees the reap.
		// gomutants:disable-next-line STATEMENT_REMOVE reason="Only a reused process ID sees the flag."
		tracker.reaped.Store(true)

		close(tracker.ended)
		<-tracker.swept

		done <- err
	}()

	var err error

	select {
	case err = <-done:
	case <-ctx.Done():
		// One more listing catches a descendant started since the last one. The listing is dropped if the child has ended.
		tracker.refresh()

		//nolint:errcheck // The child may already have exited.
		_ = cmd.Process.Signal(syscall.SIGTERM)

		select {
		case <-done: //nolint:errcheck // Shutdown only needs process completion.
		case <-time.After(terminationGrace):
			//nolint:errcheck // The child may already have exited.
			_ = cmd.Process.Kill()

			<-done //nolint:errcheck // Shutdown only needs process completion.
		}

		// gomutants:disable-next-line STATEMENT_REMOVE reason="The settle step gives a canceled login the same error."
		err = ErrArguments
	}

	return settleLogin(ctx, tracker, err)
}

// settleLogin takes the result of the login child after it has ended.
func settleLogin(ctx context.Context, tracker *process, err error) error {
	// A terminal interrupt goes to the child and this process together, so the child may end first.
	if ctx.Err() != nil || err != nil {
		tracker.sweep()
	}

	// A child that traps the interrupt and exits cleanly is still an interrupted login.
	if ctx.Err() != nil && err == nil {
		return ErrArguments
	}

	return err
}

const lockName = ".runner.lock"

func lockAlias(root *os.Root) (*os.File, error) {
	file, err := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, privateMode)
	// gomutants:disable-next-line BRANCH_IF reason="Same error."
	if err != nil {
		return nil, ErrArguments
	}

	if verifyLock(root, lockName, file) != nil {
		_ = file.Close()

		return nil, ErrArguments
	}

	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = file.Close()

		return nil, ErrArguments
	}

	return file, nil
}

// verifyLock rejects a lock path that links elsewhere. The os.Root type follows links that resolve inside the
// root, so the O_NOFOLLOW flag alone doesn't stop a link to another file in the alias directory.
func verifyLock(root *os.Root, name string, file *os.File) error {
	linked, err := root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() {
		return ErrArguments
	}

	opened, err := file.Stat()
	// gomutants:disable-next-line EXPRESSION_REMOVE reason="Only a swap after the open can fail either operand."
	if err != nil || !os.SameFile(linked, opened) {
		return ErrArguments
	}

	return nil
}

// hasExporter reports whether the options fit the Claude status line. Codex has no status line.
func (options Options) hasExporter() bool {
	return options.Vendor != vendorClaude || options.Exporter != ""
}

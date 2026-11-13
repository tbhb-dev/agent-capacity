// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session //nolint:testpackage // Tests inject the clock and inspect private process boundaries.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

func TestOneRunnerPerAlias(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	cancel, _ := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) == 2 })

	checkRunnerExit(t, fixtureValue, 1)

	if len(fixtureValue.calls(t)) != 2 {
		t.Fatal("duplicate vendor")
	}

	cancel()
}

func TestOtherChildEndsRequestRestart(t *testing.T) {
	t.Parallel()

	for _, vendor := range []string{vendorClaude, vendorCodex} {
		t.Run(vendor, func(t *testing.T) {
			t.Parallel()
			fixtureValue := setup(t, vendor)
			fixtureValue.control(
				t,
				fakeControl{
					EndClaude:  true,
					ExecExit:   2,
					Status:     0,
					Warning:    false,
					Hold:       false,
					IgnoreTerm: false,
					HoldStatus: false,
					Grandchild: false,
				},
			)
			_, result := fixtureValue.run(t)
			fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == "child_exit" })

			if <-result != 1 {
				t.Fatal("exit")
			}

			if len(fixtureValue.status(t).Restarts) != 1 {
				t.Fatal("restart not counted")
			}
		})
	}
}

func TestStatusFailureAndTimeout(t *testing.T) {
	t.Parallel()

	for _, hold := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "timeout"}[hold], func(t *testing.T) {
			t.Parallel()
			fixtureValue := setup(t, vendorClaude)
			fixtureValue.control(
				t,
				fakeControl{
					Status:     2,
					HoldStatus: hold,
					Grandchild: false,
					Hold:       false,
					EndClaude:  false,
					IgnoreTerm: false,
					ExecExit:   0,
					Warning:    false,
				},
			)
			_, result := fixtureValue.run(t)
			fixtureValue.until(t, func() bool {
				return len(fixtureValue.calls(t)) == 1 && (!hold || fixtureValue.status(t).Phase == phaseChecking)
			})

			if hold {
				fixtureValue.clock.advance(checkTimeout)
			}

			fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == reasonStatusFailed })

			if <-result != 1 {
				t.Fatal("exit")
			}

			if len(fixtureValue.calls(t)) != 1 {
				t.Fatal("child started")
			}
		})
	}
}

func TestApprovalRevocationEndsChild(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	_, result := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) == 2 })
	pid := fixtureValue.calls(t)[1].PID
	must(t, os.WriteFile(fixtureValue.options.AliasFile, nil, 0o600))
	fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == "not_approved" })

	if <-result != 0 || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatal("revoked child survived")
	}
}

func TestRestartWindowExpires(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	old := fixtureValue.clock.Now().Add(-restartWindow - time.Second)
	must(
		t,
		spool.WriteSession(
			root,
			spool.SessionStatus{
				Time:     old,
				Alias:    fixtureAlias,
				Vendor:   vendorClaude,
				Phase:    phaseRunning,
				Reason:   reasonNone,
				Restarts: []time.Time{old, old, old, old, old}, LoginExpiring: false,
			},
		),
	)
	must(t, root.Close())
	cancel, _ := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) == 2 })

	if len(fixtureValue.status(t).Restarts) != 1 {
		t.Fatal("old restarts retained")
	}

	cancel()
}

func TestCodexDoesNotOverlapExec(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorCodex)
	fixtureValue.control(
		t,
		fakeControl{
			Hold:       true,
			EndClaude:  false,
			IgnoreTerm: false,
			HoldStatus: false,
			Grandchild: false,
			Status:     0,
			ExecExit:   0,
			Warning:    false,
		},
	)
	cancel, _ := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return fixtureValue.running(t, 2) })
	fixtureValue.clock.advance(2 * execInterval)
	fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) == 3 })

	for _, call := range fixtureValue.calls(t)[2:] {
		if call.Args[0] == commandExec {
			t.Fatal("overlapping execution")
		}
	}

	cancel()
}

func TestCleanupRejectsSessionLinks(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorCodex)
	target := t.TempDir()
	must(t, os.Symlink(target, filepath.Join(fixtureValue.options.directory(), vendorCodex, "sessions")))
	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	defer alias.Close()

	if cleanRollouts(alias, fixtureValue.clock.Now()) == nil {
		t.Fatal("session link accepted")
	}
}

func TestWarningAcrossChunks(t *testing.T) {
	t.Parallel()

	var flag atomic.Bool

	drain := warningDrain{flag: &flag, tail: ""}
	_, err := drain.Write([]byte("prefix Your login expires in "))
	must(t, err)

	if flag.Load() {
		t.Fatal("early warning")
	}

	_, err = drain.Write([]byte("3 days suffix"))
	must(t, err)

	if !flag.Load() {
		t.Fatal("split warning missed")
	}
}

func TestTerminationEscalatesAfterGrace(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorCodex)

	var control fakeControl

	control.Hold, control.IgnoreTerm = true, true
	fixtureValue.control(t, control)

	var warning atomic.Bool

	child, err := start(fixtureValue.options.command(commandExec), false, &warning)
	must(t, err)

	defer child.signalGroup(syscall.SIGKILL)

	ready := filepath.Join(fixtureValue.options.directory(), "work", "ready")
	fixtureValue.until(t, func() bool {
		_, statErr := os.Stat(ready)

		return statErr == nil
	})

	before := time.Now()

	child.terminate()

	if time.Since(before) < terminationGrace {
		t.Fatal("termination skipped the grace period")
	}

	if !errors.Is(syscall.Kill(child.cmd.Process.Pid, 0), syscall.ESRCH) {
		t.Fatal("child survived escalation")
	}
}

const maximumAttempts = 200

func TestRepeatedStatusFailuresReachRestartLimit(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)

	var control fakeControl

	control.Status = 2
	fixtureValue.control(t, control)

	for attempt := 0; len(fixtureValue.status(t).Restarts) < restartMaximum; attempt++ {
		if attempt == maximumAttempts {
			t.Fatal("restart limit not reached after", maximumAttempts, "attempts")
		}

		before := len(fixtureValue.status(t).Restarts)
		_, result := fixtureValue.run(t)
		fixtureValue.until(t, func() bool { return len(result) == 1 })

		got := <-result
		// A forked child of another parallel test can still hold the alias lock. The runner then refuses the run.
		if len(fixtureValue.status(t).Restarts) == before {
			time.Sleep(10 * time.Millisecond)

			continue
		}

		want := 1
		if before == restartMaximum-1 {
			want = 0
		}

		if got != want {
			t.Fatalf("exit = %d, want %d", got, want)
		}
	}

	if fixtureValue.status(t).Reason != reasonRestartLimit {
		t.Fatal("restart limit was not published")
	}
}

var errPlain = errors.New("an error without an exit code")

type codeError int

func (c codeError) Error() string { return "exit code" }
func (c codeError) ExitCode() int { return int(c) }

type unknownOwner struct{ os.FileInfo }

func (unknownOwner) Sys() any { return nil }

// stateFor builds a runner state for calling its steps directly.
func stateFor(t *testing.T, fixtureValue fixture) *running {
	t.Helper()

	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	state := new(running)
	state.options, state.clock, state.root, state.alias = fixtureValue.options, fixtureValue.clock, root, alias
	state.status.Vendor, state.status.Alias = fixtureValue.options.Vendor, fixtureValue.options.Account

	t.Cleanup(func() {
		state.stopChildren()
		must(t, errors.Join(root.Close(), alias.Close()))
	})

	return state
}

// finished returns a process whose wait result is already available.
func finished(err error) *process {
	done := make(chan error, 1)
	done <- err

	return &process{cmd: nil, done: done, terminal: nil, drained: nil}
}

// breakStatus makes every later status write fail by putting a directory where the status file belongs.
func breakStatus(t *testing.T, fixtureValue fixture) {
	t.Helper()

	name := fixtureValue.options.Vendor + "-" + fixtureValue.options.Account + ".json"
	must(t, os.MkdirAll(filepath.Join(fixtureValue.options.SpoolDir, "status", name), 0o700))
}

// wantStopped checks the result of a runner step that ends the runner and records a reason.
func (fixtureValue fixture) wantStopped(t *testing.T, code int, done bool, wantCode int, reason string) {
	t.Helper()

	if code != wantCode || !done || fixtureValue.status(t).Reason != reason {
		t.Fatal(code, done, fixtureValue.status(t))
	}
}

// denyAccess removes all permissions from a path until the test ends.
func denyAccess(t *testing.T, path string, mode os.FileMode) {
	t.Helper()

	must(t, os.Chmod(path, mode))
	t.Cleanup(func() {
		os.Chmod(path, 0o700) //nolint:errcheck,gosec // The test may already have removed the path.
	})
}

func TestOptionsValidation(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	valid := fixtureValue.options

	if !valid.Valid() || !valid.validRun() {
		t.Fatal("fixture options rejected")
	}

	valid.Vendor, valid.Binary = vendorCodex, ""
	if !valid.Valid() {
		t.Fatal("codex without a binary rejected")
	}

	for name, mutate := range map[string]func(*Options){
		"vendor":     func(o *Options) { o.Vendor = "other" },
		"account":    func(o *Options) { o.Account = badAccount },
		"alias file": func(o *Options) { o.AliasFile = "approved" },
		"root":       func(o *Options) { o.Root = "relative" },
		"binary":     func(o *Options) { o.Binary = "relative" },
	} {
		options := fixtureValue.options
		mutate(&options)

		if options.Valid() || options.validRun() {
			t.Fatal(name, "accepted")
		}

		approved, err := options.Approved()
		if approved || !errors.Is(err, ErrArguments) {
			t.Fatal(name, "approved")
		}

		if code := (Runner{Options: options, Clock: fixtureValue.clock}).Run(context.Background()); code != 1 {
			t.Fatal(name, "ran with exit", code)
		}
	}

	for name, mutate := range map[string]func(*Options){
		"spool":    func(o *Options) { o.SpoolDir = "spool" },
		"empty":    func(o *Options) { o.Prompt = "" },
		"words":    func(o *Options) { o.Prompt = "two words" },
		"control":  func(o *Options) { o.Prompt = "a\x00b" },
		"option":   func(o *Options) { o.Prompt = "-p" },
		"linefeed": func(o *Options) { o.Prompt = "a\nb" },
	} {
		options := fixtureValue.options
		mutate(&options)

		if options.validRun() {
			t.Fatal(name, "accepted")
		}
	}
}

func TestApprovedReadFailures(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	options := fixtureValue.options

	must(t, os.WriteFile(options.AliasFile, []byte(strings.Repeat("a", 70000)), 0o600))

	approved, err := options.Approved()
	if approved || !errors.Is(err, ErrArguments) {
		t.Fatal("oversized line accepted")
	}

	must(t, os.Remove(options.AliasFile))

	approved, err = options.Approved()
	if approved || !errors.Is(err, ErrArguments) {
		t.Fatal("missing allow list accepted")
	}
}

func TestOwnedPrivateRejectsOtherShapes(t *testing.T) {
	t.Parallel()

	dir := canonicalTempDir(t)
	uid := os.Getuid()

	private := filepath.Join(dir, "private")
	sticky := filepath.Join(dir, "sticky")
	file := filepath.Join(dir, "file")

	must(t, os.Mkdir(private, 0o700))
	must(t, os.Mkdir(sticky, 0o700))
	must(t, os.Chmod(sticky, 0o700|os.ModeSticky))
	must(t, os.WriteFile(file, nil, 0o600))

	for path, want := range map[string]bool{private: true, sticky: false, file: false} {
		info, err := os.Stat(path)
		must(t, err)

		if ownedPrivate(info, uid) != want {
			t.Fatal(path, "wrong verdict")
		}

		if ownedPrivate(info, uid+1) {
			t.Fatal(path, "accepted for another owner")
		}
	}

	info, err := os.Stat(private)
	must(t, err)

	if ownedPrivate(unknownOwner{info}, uid) {
		t.Fatal("unknown owner accepted")
	}
}

func TestOpenAliasRejects(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	options := fixtureValue.options

	denyAccess(t, options.directory(), 0o400)

	_, err := options.openAlias()
	if !errors.Is(err, ErrArguments) {
		t.Fatal("unsearchable directory accepted")
	}

	fixtureValue = setup(t, vendorClaude)
	options = fixtureValue.options
	must(t, os.RemoveAll(options.directory()))

	_, err = options.openAlias()
	if !errors.Is(err, ErrArguments) {
		t.Fatal("missing directory accepted")
	}
}

func TestCommandDefaultsToVendorName(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	options := fixtureValue.options
	options.Binary = ""

	if got := options.command(commandExec).Args[0]; got != vendorCodex {
		t.Fatal(got)
	}
}

func TestLoginRefusalAndVendorFailure(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	invalid := fixtureValue.options
	invalid.Account = badAccount

	var out, diagnostic strings.Builder

	refused := Login(context.Background(), invalid, os.Stdin, &out, &diagnostic)
	if refused != refusalExit || diagnostic.String() != "login refused\n" {
		t.Fatal("invalid options not refused")
	}

	var control fakeControl

	control.ExecExit = 3
	fixtureValue.control(t, control)

	master, slave, err := pty.Open()
	must(t, err)

	defer master.Close()
	defer slave.Close()

	out.Reset()

	exit := Login(context.Background(), fixtureValue.options, slave, &out, &diagnostic)
	if exit != 1 || strings.Contains(out.String(), "login finished") {
		t.Fatal("vendor failure reported as success")
	}
}

func TestProcessStartFailures(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	options := fixtureValue.options
	options.Binary = filepath.Join(fixtureValue.base, "missing")

	var warning atomic.Bool

	for _, terminal := range []bool{true, false} {
		child, err := start(options.command(commandExec), terminal, &warning)
		if child != nil || !errors.Is(err, ErrArguments) {
			t.Fatal("missing binary started, terminal:", terminal)
		}
	}
}

func TestWarningShortWrite(t *testing.T) {
	t.Parallel()

	var flag atomic.Bool

	drain := warningDrain{flag: &flag, tail: ""}

	_, err := drain.Write([]byte("hi"))
	must(t, err)

	if flag.Load() || drain.tail != "hi" {
		t.Fatal("short write mishandled")
	}
}

func TestRunRefusals(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		prepare func(*fixture) error
		reason  string
	}{
		"missing spool": {func(f *fixture) error {
			f.options.SpoolDir = filepath.Join(f.base, "none")

			return nil
		}, noStatus},
		"missing directory": {func(f *fixture) error { return os.RemoveAll(f.options.directory()) }, noStatus},
		"missing work": {
			func(f *fixture) error { return os.Remove(filepath.Join(f.options.directory(), "work")) }, noStatus,
		},
		"linked work": {func(f *fixture) error {
			work := filepath.Join(f.options.directory(), "work")

			return errors.Join(os.Remove(work), os.Symlink(f.base, work))
		}, noStatus},
		"linked lock": {func(f *fixture) error {
			return os.Symlink(f.base, filepath.Join(f.options.directory(), ".runner.lock"))
		}, noStatus},
		"corrupt status": {func(f *fixture) error {
			status := filepath.Join(f.options.SpoolDir, "status")

			file := filepath.Join(status, "claude-sample.json")

			return errors.Join(os.Mkdir(status, 0o700), os.WriteFile(file, []byte("{"), 0o600))
		}, "invalid_state"},
		"missing allow list": {func(f *fixture) error { return os.Remove(f.options.AliasFile) }, "approval_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, vendorClaude)
			must(t, test.prepare(&fixtureValue))
			checkRunnerExit(t, fixtureValue, 1)

			if test.reason == noStatus {
				fixtureValue.wantNoStatus(t)
			} else if fixtureValue.status(t).Reason != test.reason {
				t.Fatal(fixtureValue.status(t))
			}
		})
	}
}

func TestRunWithoutClockStopsOnCancel(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := (Runner{Options: fixtureValue.options, Clock: nil}).Run(ctx); got != 1 {
		t.Fatal(got)
	}

	if fixtureValue.status(t).Reason != "interrupted" {
		t.Fatal(fixtureValue.status(t))
	}
}

func TestLoadRestartsCountsCheckingPhase(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	var previous spool.SessionStatus

	previous.Time, previous.Vendor, previous.Alias = fixtureValue.clock.Now(), vendorClaude, fixtureAlias
	previous.Phase, previous.Reason = phaseChecking, reasonNone
	must(t, spool.WriteSession(root, previous))
	must(t, root.Close())

	state := stateFor(t, fixtureValue)
	must(t, state.loadRestarts())

	if len(state.status.Restarts) != 1 {
		t.Fatal("interrupted check not counted")
	}
}

func TestInvalidAccountStopsStep(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	state := stateFor(t, fixtureValue)
	state.options.Account = badAccount

	if code, done := state.initialApproval(); code != 1 || !done {
		t.Fatal("initial approval accepted a bad account")
	}

	if code, done := state.step(context.Background()); code != 1 || !done || state.status.Reason != "invalid_state" {
		t.Fatal(code, done, state.status.Reason)
	}
}

func TestPublishFailuresEndTheRunner(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		run    func(*testing.T, fixture, *running) (int, bool)
		vendor string
	}{
		"finish": {
			func(_ *testing.T, _ fixture, s *running) (int, bool) { return s.finish(0, reasonSignedOut), true },
			vendorClaude,
		},
		"periodic status": {func(_ *testing.T, _ fixture, s *running) (int, bool) {
			s.nextCheck = s.clock.Now().Add(time.Hour)

			return s.step(context.Background())
		}, vendorClaude},
		"operator stop": {func(t *testing.T, f fixture, s *running) (int, bool) {
			t.Helper()
			must(t, os.WriteFile(filepath.Join(f.options.SpoolDir, "stop"), nil, 0o600))

			return s.step(context.Background())
		}, vendorClaude},
		"status check": {func(_ *testing.T, _ fixture, s *running) (int, bool) { return s.statusCheck() }, vendorClaude},
		"cleanup idle": {func(_ *testing.T, _ fixture, s *running) (int, bool) {
			s.child = finished(nil)

			return s.childEnded(nil)
		}, vendorCodex},
		"scheduled restart": {func(_ *testing.T, f fixture, s *running) (int, bool) {
			s.started = s.clock.Now()
			f.clock.advance(sessionLifetime)
			s.readingAt = s.clock.Now()

			return s.considerRestart()
		}, vendorClaude},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, test.vendor)
			state := stateFor(t, fixtureValue)
			breakStatus(t, fixtureValue)

			if code, done := test.run(t, fixtureValue, state); code != 1 || !done {
				t.Fatal(code, done)
			}
		})
	}
}

func TestStatusCheckStartFailure(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	state := stateFor(t, fixtureValue)
	state.options.Binary = filepath.Join(fixtureValue.base, "missing")

	code, done := state.statusCheck()
	fixtureValue.wantStopped(t, code, done, 1, reasonStatusFailed)
}

func TestCodexExecDueStartsStatusCheck(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	state := stateFor(t, fixtureValue)
	state.nextCheck = state.clock.Now().Add(time.Hour)

	if code, done := state.statusCheck(); code != 0 || done || state.check == nil {
		t.Fatal(code, done)
	}
}

func TestStatusCheckResults(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	state := stateFor(t, fixtureValue)
	state.nextExec = state.clock.Now().Add(time.Hour)
	state.check = finished(nil)

	if code, done := state.pollStatusCheck(); code != 0 || done || state.child != nil {
		t.Fatal("child launched before the next execution time")
	}

	state.child, state.check = finished(nil), finished(nil)

	if code, done := state.pollStatusCheck(); code != 0 || done {
		t.Fatal("status check ended a running child")
	}

	state.child = nil
	state.nextExec = time.Time{}
	state.options.Binary = filepath.Join(fixtureValue.base, "missing")
	state.check = finished(nil)

	code, done := state.pollStatusCheck()
	fixtureValue.wantStopped(t, code, done, 1, "start_failed")

	state.check = finished(errPlain)
	code, done = state.pollStatusCheck()
	fixtureValue.wantStopped(t, code, done, 1, reasonStatusFailed)

	state.check = finished(codeError(1))
	code, done = state.pollStatusCheck()
	fixtureValue.wantStopped(t, code, done, 0, reasonSignedOut)
}

func TestExitCode(t *testing.T) {
	t.Parallel()

	if exitCode(codeError(7)) != 7 || exitCode(errPlain) != -1 {
		t.Fatal("exit code mismatch")
	}
}

func TestRestartLimitEndsRunner(t *testing.T) {
	t.Parallel()

	for name, scheduled := range map[string]bool{"child exit": false, "scheduled restart": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, vendorClaude)
			state := stateFor(t, fixtureValue)
			state.started = state.clock.Now()

			if scheduled {
				fixtureValue.clock.advance(sessionLifetime)
				state.readingAt = state.clock.Now()
			}

			for range restartMaximum - 1 {
				state.status.Restarts = append(state.status.Restarts, state.clock.Now())
			}

			var (
				code int
				done bool
			)

			if !scheduled {
				state.child = finished(nil)
				code, done = state.childEnded(nil)
			} else {
				code, done = state.considerRestart()
			}

			fixtureValue.wantStopped(t, code, done, 0, reasonRestartLimit)
		})
	}
}

func TestCodexCleanupFailureEndsRunner(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	must(t, os.Symlink(t.TempDir(), filepath.Join(fixtureValue.options.directory(), vendorCodex, "sessions")))

	state := stateFor(t, fixtureValue)
	state.child = finished(nil)

	code, done := state.childEnded(nil)
	fixtureValue.wantStopped(t, code, done, 1, "cleanup_failed")
}

func TestSnapshotHandling(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	state := stateFor(t, fixtureValue)
	state.started, state.readingAt = state.clock.Now(), state.clock.Now()

	path := filepath.Join(fixtureValue.options.SpoolDir, "claude-sample.json")
	older := state.clock.Now().Add(-time.Hour)

	must(t, os.WriteFile(path, nil, 0o600))
	must(t, os.Chtimes(path, older, older))

	if code, done := state.considerRestart(); code != 0 || done || !state.snapshotAt.Equal(older) {
		t.Fatal(code, done, state.snapshotAt)
	}

	if !state.readingAt.Equal(state.clock.Now()) {
		t.Fatal("old snapshot moved the reading time")
	}

	must(t, os.Remove(path))
	must(t, os.Mkdir(path, 0o700))

	code, done := state.considerRestart()
	fixtureValue.wantStopped(t, code, done, 1, "invalid_snapshot")
}

func TestCleanupRolloutFailures(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	codex := filepath.Join(fixtureValue.options.directory(), vendorCodex)
	sessions := filepath.Join(codex, "sessions")
	now := fixtureValue.clock.Now()

	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	defer alias.Close()

	if cleanRollouts(alias, now) != nil {
		t.Fatal("missing sessions directory rejected")
	}

	must(t, os.Mkdir(sessions, 0o700))

	locked := filepath.Join(sessions, "locked")
	must(t, os.Mkdir(locked, 0o700))
	denyAccess(t, locked, 0)

	if !errors.Is(cleanRollouts(alias, now), ErrArguments) {
		t.Fatal("unreadable subdirectory accepted")
	}

	denyAccess(t, sessions, 0)

	if !errors.Is(cleanRollouts(alias, now), ErrArguments) {
		t.Fatal("unopenable sessions directory accepted")
	}

	denyAccess(t, codex, 0)

	if !errors.Is(cleanRollouts(alias, now), ErrArguments) {
		t.Fatal("unsearchable codex directory accepted")
	}
}

func TestCleanupCodexDirectoryShapes(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	codex := filepath.Join(fixtureValue.options.directory(), vendorCodex)
	now := fixtureValue.clock.Now()

	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	defer alias.Close()

	must(t, os.Remove(codex))

	if cleanRollouts(alias, now) != nil {
		t.Fatal("missing codex directory rejected")
	}

	must(t, os.WriteFile(codex, nil, 0o600))

	if !errors.Is(cleanRollouts(alias, now), ErrArguments) {
		t.Fatal("codex file accepted")
	}
}

func TestRemoveOldRolloutFailures(t *testing.T) {
	t.Parallel()

	dir := canonicalTempDir(t)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	must(t, os.Mkdir(filepath.Join(dir, "directory"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "old"), nil, 0o600))
	must(t, os.Chtimes(filepath.Join(dir, "old"), now.Add(-72*time.Hour), now.Add(-72*time.Hour)))

	root, err := os.OpenRoot(dir)
	must(t, err)

	defer root.Close()

	if !errors.Is(removeOldRollout(root, "missing", now), ErrArguments) {
		t.Fatal("missing file accepted")
	}

	if removeOldRollout(root, "directory", now) != nil {
		t.Fatal("directory rejected")
	}

	denyAccess(t, dir, 0o500)

	if !errors.Is(removeOldRollout(root, "old", now), ErrArguments) {
		t.Fatal("failed removal accepted")
	}
}

func TestFinishAtRestartLimit(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	state := stateFor(t, fixtureValue)

	for range restartMaximum - 1 {
		state.status.Restarts = append(state.status.Restarts, state.clock.Now())
	}

	fixtureValue.wantStopped(t, state.finish(1, reasonStatusFailed), true, 0, reasonRestartLimit)
}

// writeSettings renders the safe settings, lets change edit the permissions and stores the result.
func writeSettings(t *testing.T, fixtureValue fixture, change func(permissions map[string]any)) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(fixtureValue.options.directory(), settingsPath))
	must(t, err)

	var settings map[string]any
	must(t, json.Unmarshal(data, &settings))

	permissions, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatal("rendered settings have no permissions")
	}

	change(permissions)

	data, err = json.Marshal(settings)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(fixtureValue.options.directory(), settingsPath), data, 0o600))
}

// wantRefusedLaunch checks that a status check result ends the runner without starting the child.
func wantRefusedLaunch(t *testing.T, fixtureValue fixture) {
	t.Helper()

	state := stateFor(t, fixtureValue)
	state.check = finished(nil)

	code, done := state.pollStatusCheck()
	fixtureValue.wantStopped(t, code, done, 1, "start_failed")

	if state.child != nil || len(fixtureValue.calls(t)) != 0 {
		t.Fatal("child started with unsafe settings")
	}
}

func TestLaunchRefusesUnsafePermissions(t *testing.T) {
	t.Parallel()

	tests := map[string]func(permissions map[string]any){
		"bypass not disabled": func(permissions map[string]any) { permissions["disableBypassPermissionsMode"] = "allow" },
		"default mode wrong":  func(permissions map[string]any) { permissions["defaultMode"] = "acceptEdits" },
		"deny entry missing":  func(permissions map[string]any) { permissions["deny"] = denyList()[1:] },
		"deny not a list":     func(permissions map[string]any) { permissions["deny"] = "Bash" },
		"no permissions":      func(permissions map[string]any) { clear(permissions) },
	}

	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, vendorClaude)
			writeSettings(t, fixtureValue, change)
			wantRefusedLaunch(t, fixtureValue)
		})
	}
}

func TestLaunchRefusesUnsafeSettingsFile(t *testing.T) {
	t.Parallel()

	tests := map[string]func(path string) error{
		"missing":      os.Remove,
		"invalid json": func(path string) error { return os.WriteFile(path, []byte("{"), 0o600) },
		"unreadable":   func(path string) error { return os.Chmod(path, 0) },
		"directory": func(path string) error {
			return errors.Join(os.Remove(path), os.Mkdir(path, 0o700))
		},
		"symlink": func(path string) error {
			return errors.Join(os.Rename(path, path+".real"), os.Symlink("settings.json.real", path))
		},
	}

	for name, damage := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, vendorClaude)
			path := filepath.Join(fixtureValue.options.directory(), settingsPath)

			must(t, damage(path))
			t.Cleanup(func() {
				os.Chmod(path, 0o600) //nolint:errcheck,gosec // The path may be gone or linked.
			})
			wantRefusedLaunch(t, fixtureValue)
		})
	}
}

func TestLaunchAcceptsRenderedSettingsWithExtraDeny(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	writeSettings(t, fixtureValue, func(permissions map[string]any) {
		permissions["deny"] = append(denyList(), "Extra")
	})

	state := stateFor(t, fixtureValue)

	err := state.launch()
	if err != nil || state.child == nil {
		t.Fatal("launch refused safe settings", err)
	}
}

func TestCodexLaunchSkipsClaudeSettings(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	state := stateFor(t, fixtureValue)

	err := state.launch()
	if err != nil || state.child == nil {
		t.Fatal("codex launch needs no Claude settings", err)
	}
}

func TestRunRejectsLinkedSubdirectories(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ vendor, name string }{
		{vendorClaude, directoryWork},
		{vendorClaude, directoryHome},
		{vendorClaude, "config"},
		{vendorCodex, directoryWork},
		{vendorCodex, directoryHome},
		{vendorCodex, vendorCodex},
	} {
		t.Run(test.vendor+"/"+test.name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, test.vendor)
			target := filepath.Join(fixtureValue.options.directory(), test.name)
			must(t, os.RemoveAll(target))
			must(t, os.Symlink(fixtureValue.base, target))
			checkRunnerExit(t, fixtureValue, 1)
			fixtureValue.wantNoStatus(t)
		})
	}
}

func TestCheckDirectoriesAllowsMissingOptionalDirectories(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorCodex)
	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	defer alias.Close()

	must(t, os.Remove(filepath.Join(fixtureValue.options.directory(), directoryHome)))
	must(t, os.Remove(filepath.Join(fixtureValue.options.directory(), vendorCodex)))

	if checkDirectories(alias, vendorCodex) != nil {
		t.Fatal("missing optional directories refused")
	}

	file := filepath.Join(fixtureValue.options.directory(), directoryHome)
	must(t, os.WriteFile(file, nil, 0o600))

	if checkDirectories(alias, vendorCodex) == nil {
		t.Fatal("regular file accepted as home")
	}
}

func writePreviousStatus(t *testing.T, fixtureValue fixture) spool.SessionStatus {
	t.Helper()

	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	var previous spool.SessionStatus

	now := fixtureValue.clock.Now()
	previous.Time, previous.Vendor, previous.Alias = now, vendorClaude, fixtureAlias
	previous.Phase, previous.Reason = phaseStopped, reasonScheduled
	previous.Restarts = []time.Time{
		now.Add(-30 * time.Minute), now.Add(-20 * time.Minute), now.Add(-10 * time.Minute),
	}
	must(t, spool.WriteSession(root, previous))
	must(t, root.Close())

	return previous
}

func (fixtureValue fixture) statusFile() string {
	name := fixtureValue.options.Vendor + "-" + fixtureValue.options.Account + ".json"

	return filepath.Join(fixtureValue.options.SpoolDir, "status", name)
}

func (fixtureValue fixture) wantNoStatus(t *testing.T) {
	t.Helper()

	_, err := os.Lstat(fixtureValue.statusFile())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("status file written", err)
	}
}

func TestEarlyFailuresKeepRestartHistory(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	previous := writePreviousStatus(t, fixtureValue)
	must(t, os.Remove(fixtureValue.options.AliasFile))
	checkRunnerExit(t, fixtureValue, 1)

	got := fixtureValue.status(t)
	if got.Reason != "approval_unavailable" || len(got.Restarts) != len(previous.Restarts)+1 ||
		!slices.Equal(got.Restarts[:len(previous.Restarts)], previous.Restarts) {
		t.Fatal(got)
	}
}

func TestPreLockFailuresLeaveStatusUntouched(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	writePreviousStatus(t, fixtureValue)

	before, err := os.ReadFile(fixtureValue.statusFile())
	must(t, err)
	must(t, os.RemoveAll(fixtureValue.options.directory()))
	checkRunnerExit(t, fixtureValue, 1)

	after, err := os.ReadFile(fixtureValue.statusFile())
	must(t, err)

	if !bytes.Equal(before, after) {
		t.Fatal("pre-lock failure changed the published status")
	}
}

func TestSecondRunnerLeavesPublishedStatusUntouched(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	writePreviousStatus(t, fixtureValue)

	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	defer alias.Close()

	lock, err := lockAlias(alias)
	must(t, err)

	defer lock.Close()

	before, err := os.ReadFile(fixtureValue.statusFile())
	must(t, err)
	checkRunnerExit(t, fixtureValue, 1)

	after, err := os.ReadFile(fixtureValue.statusFile())
	must(t, err)

	if !bytes.Equal(before, after) || len(fixtureValue.calls(t)) != 0 {
		t.Fatal("second runner touched the published status")
	}
}

func TestLoginEndsWhenContextEnds(t *testing.T) {
	t.Parallel()

	for name, ignoreTerm := range map[string]bool{"terminated": false, "killed": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, vendorClaude)

			var control fakeControl

			control.Hold, control.IgnoreTerm = true, ignoreTerm
			fixtureValue.control(t, control)

			master, slave, err := pty.Open()
			must(t, err)

			defer master.Close()
			defer slave.Close()

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan int, 1)

			var out, diagnostic bytes.Buffer

			go func() { result <- Login(ctx, fixtureValue.options, slave, &out, &diagnostic) }()

			fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) == 1 })

			if ignoreTerm {
				ready := filepath.Join(fixtureValue.options.directory(), "ready")
				fixtureValue.until(t, func() bool {
					_, statErr := os.Stat(ready)

					return statErr == nil
				})
			}

			pid := fixtureValue.calls(t)[0].PID

			cancel()

			if got := <-result; got != 1 || strings.Contains(out.String(), "login finished") {
				t.Fatal(got, out.String())
			}

			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("login child survived cancellation")
			}
		})
	}
}

func TestLoginStartFailure(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	options := fixtureValue.options
	options.Binary = filepath.Join(fixtureValue.base, "missing")

	master, slave, err := pty.Open()
	must(t, err)

	defer master.Close()
	defer slave.Close()

	var out, diagnostic bytes.Buffer
	if Login(context.Background(), options, slave, &out, &diagnostic) != 1 {
		t.Fatal("missing binary reported as success")
	}
}

func TestLoginRejectsLinkedSubdirectories(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ vendor, name string }{
		{vendorClaude, directoryWork},
		{vendorClaude, directoryHome},
		{vendorClaude, "config"},
		{vendorCodex, directoryWork},
		{vendorCodex, directoryHome},
		{vendorCodex, vendorCodex},
	} {
		t.Run(test.vendor+"/"+test.name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, test.vendor)
			target := filepath.Join(fixtureValue.options.directory(), test.name)
			must(t, os.RemoveAll(target))
			must(t, os.Symlink(fixtureValue.base, target))

			master, slave, err := pty.Open()
			must(t, err)

			defer master.Close()
			defer slave.Close()

			var out, diagnostic bytes.Buffer

			got := Login(context.Background(), fixtureValue.options, slave, &out, &diagnostic)
			if got != 1 || strings.Contains(out.String(), "login finished") || len(fixtureValue.calls(t)) != 0 {
				t.Fatal(got, out.String())
			}
		})
	}
}

func TestLaunchRefusesAmbiguousSettingsKeys(t *testing.T) {
	t.Parallel()

	const deny = `["Agent","Task","Bash","Read","Write","Edit","MultiEdit","Glob","Grep","LS","NotebookEdit",` +
		`"WebFetch","WebSearch","TodoWrite","Skill","AskUserQuestion","EnterPlanMode","ExitPlanMode",` +
		`"EnterWorktree","ExitWorktree","ToolSearch","mcp__*"]`

	safe := `"defaultMode":"dontAsk","disableBypassPermissionsMode":"disable","deny":` + deny
	bad := `"defaultMode":"acceptEdits","deny":[]`

	tests := map[string]string{
		"duplicate exact key":      `{"permissions":{` + bad + `},"permissions":{` + safe + `}}`,
		"permissions case variant": `{"permissions":{` + bad + `},"Permissions":{` + safe + `}}`,
		"defaultMode variant":      `{"permissions":{` + safe + `,"DefaultMode":"acceptEdits"}}`,
		"deny variant":             `{"permissions":{` + safe + `,"Deny":[]}}`,
		"nested duplicate":         `{"permissions":{` + safe + `},"other":[{"a":1,"A":2}]}`,
		"non string key":           `{1:2}`,
		"bad nested value":         `{"permissions":}`,
		"bad array value":          `{"other":[}`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixtureValue := setup(t, vendorClaude)
			path := filepath.Join(fixtureValue.options.directory(), settingsPath)
			must(t, os.WriteFile(path, []byte(content), 0o600))
			wantRefusedLaunch(t, fixtureValue)
		})
	}
}

func TestLaunchAcceptsSettingsWithDistinctKeysAndNestedObjects(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)
	writeSettings(t, fixtureValue, func(permissions map[string]any) {
		permissions["extra"] = []any{map[string]any{"a": 1, "b": []any{}}}
	})

	state := stateFor(t, fixtureValue)

	err := state.launch()
	if err != nil || state.child == nil {
		t.Fatal("launch refused distinct keys", err)
	}
}

func TestLoginEndsWhenGrandchildHoldsOutput(t *testing.T) {
	t.Parallel()

	fixtureValue := setup(t, vendorClaude)

	var control fakeControl

	control.Hold, control.IgnoreTerm, control.Grandchild = true, true, true
	fixtureValue.control(t, control)

	master, slave, err := pty.Open()
	must(t, err)

	defer master.Close()
	defer slave.Close()

	grandchildFile := filepath.Join(fixtureValue.options.directory(), "grandchild")

	t.Cleanup(func() {
		data, readErr := os.ReadFile(grandchildFile)
		pid, parseErr := strconv.Atoi(string(data))

		if readErr == nil && parseErr == nil {
			//nolint:errcheck // The process may have ended.
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)

	var out, diagnostic bytes.Buffer

	go func() { result <- Login(ctx, fixtureValue.options, slave, &out, &diagnostic) }()

	fixtureValue.until(t, func() bool {
		_, statErr := os.Stat(grandchildFile)

		return statErr == nil
	})
	cancel()

	select {
	case got := <-result:
		if got != 1 || strings.Contains(out.String(), "login finished") {
			t.Fatal(got, out.String())
		}
	case <-time.After(5 * terminationGrace):
		t.Fatal("login blocked on the output pipe after cancellation")
	}
}

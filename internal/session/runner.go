// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	statusInterval  = 30 * time.Second
	checkTimeout    = 30 * time.Second
	readingInterval = 15 * time.Minute
	sessionLifetime = 6 * 24 * time.Hour
	execInterval    = 5 * time.Minute
	restartWindow   = time.Hour
	restartMaximum  = 5
)

// Clock lets tests advance scheduling without running real vendor sessions.
type Clock interface {
	Now() time.Time
	After(delay time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                             { return time.Now() }
func (wallClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }

// Runner runs one alias and publishes only fixed status fields. The alias lock file records the lock.
type Runner struct {
	Clock   Clock
	Options Options
}

type running struct {
	started        time.Time
	nextStatus     time.Time
	snapshotAt     time.Time
	readingAt      time.Time
	nextExec       time.Time
	checkAt        time.Time
	nextCheck      time.Time
	clock          Clock
	check          *process
	child          *process
	alias          *os.Root
	root           *os.Root
	options        Options
	status         spool.SessionStatus
	warning        atomic.Bool
	readingRestart bool
}

// Run returns zero only for durable stay-down reasons, and one for other ends.
func (r Runner) Run(ctx context.Context) int {
	options := r.Options
	if !options.validRun() {
		return 1
	}

	root, err := spool.SessionRoot(options.SpoolDir)
	if err != nil {
		return 1
	}
	defer root.Close()

	clock := r.Clock
	if clock == nil {
		clock = wallClock{}
	}

	state := new(running)
	state.options, state.clock, state.root = options, clock, root
	state.status.Alias, state.status.Vendor = options.Account, options.Vendor

	state.status.Phase, state.status.Reason = phaseChecking, reasonNone
	if code, done := state.initialApproval(); done {
		return code
	}

	alias, err := options.openAlias()
	if err != nil {
		return state.finish(1, "invalid_directory")
	}
	defer alias.Close()

	state.alias = alias

	if workDirectory(alias) != nil {
		return state.finish(1, "invalid_directory")
	}

	lock, err := lockAlias(alias)
	if err != nil {
		return 1
	}
	defer lock.Close()

	if state.loadRestarts() != nil {
		return state.finish(1, "invalid_state")
	}
	defer state.stopChildren()

	return state.loop(ctx)
}

func (options Options) validRun() bool {
	return options.Valid() && filepath.IsAbs(options.SpoolDir) && options.Prompt != "" &&
		len(strings.Fields(options.Prompt)) == 1 && !strings.ContainsAny(options.Prompt, "\x00\r\n\t ") &&
		!strings.HasPrefix(options.Prompt, "-")
}

func (s *running) initialApproval() (int, bool) {
	stopped, err := spool.Stopped(s.root, s.options.Account)
	if err != nil {
		return 1, true
	}

	if stopped {
		return 0, false
	}

	return s.checkApproval()
}

func (s *running) checkApproval() (int, bool) {
	approved, err := s.options.Approved()
	if err != nil {
		return s.finish(1, "approval_unavailable"), true
	}

	if !approved {
		s.stopChildren()

		return s.finish(0, "not_approved"), true
	}

	return 0, false
}

func (s *running) loadRestarts() error {
	previous, err := spool.ReadSession(s.root, s.options.Vendor, s.options.Account)
	if err != nil {
		return ErrArguments
	}

	s.status.Restarts = previous.Restarts
	if previous.Phase == phaseRunning || previous.Phase == phaseChecking {
		s.status.Restarts = append(s.status.Restarts, s.clock.Now())
	}

	return nil
}

func (s *running) loop(ctx context.Context) int {
	for {
		code, done := s.step(ctx)
		if done {
			return code
		}

		select {
		case <-ctx.Done():
			return s.finish(1, "interrupted")
		case <-s.clock.After(time.Second):
		}
	}
}

func (s *running) publish(phase, reason string) error {
	s.status.Time = s.clock.Now().UTC()
	s.status.Phase, s.status.Reason = phase, reason
	s.status.LoginExpiring = s.warning.Load()
	s.nextStatus = s.clock.Now().Add(statusInterval)

	if spool.WriteSession(s.root, s.status) != nil {
		return ErrArguments
	}

	return nil
}

func (s *running) finish(code int, reason string) int {
	if code == 1 {
		if reason != "child_exit" {
			s.status.Restarts = append(s.status.Restarts, s.clock.Now())
		}

		if s.limitReached() {
			code, reason = 0, "restart_limit"
		}
	}

	if s.publish(phaseStopped, reason) != nil {
		return 1
	}

	return code
}

func (s *running) stopChildren() {
	s.child.terminate()
	s.child = nil
	s.check.terminate()
	s.check = nil
}

func (s *running) step(ctx context.Context) (int, bool) {
	stopped, err := spool.Stopped(s.root, s.options.Account)
	if err != nil {
		return s.finish(1, "invalid_state"), true
	}

	if stopped {
		return s.idleStop()
	}

	if ctx.Err() != nil {
		return s.finish(1, "interrupted"), true
	}

	code, done := s.checkApproval()
	if done {
		return code, true
	}

	if s.limitReached() {
		s.stopChildren()

		return s.finish(0, "restart_limit"), true
	}

	if s.status.Reason == "operator_stop" {
		s.nextCheck = time.Time{}
		s.nextExec = time.Time{}
	}

	code, done = s.statusCheck()
	if done {
		return code, true
	}

	if s.child != nil {
		code, done = s.observeChild()
		if done {
			return code, true
		}
	}

	if !s.clock.Now().Before(s.nextStatus) || s.status.LoginExpiring != s.warning.Load() {
		if s.publish(s.status.Phase, s.status.Reason) != nil {
			return 1, true
		}
	}

	return 0, false
}

func (s *running) idleStop() (int, bool) {
	s.stopChildren()

	if s.status.Reason == "operator_stop" && s.clock.Now().Before(s.nextStatus) {
		return 0, false
	}

	return 1, s.publish(phaseIdle, "operator_stop") != nil
}

func (s *running) statusCheck() (int, bool) {
	if s.check != nil {
		return s.pollStatusCheck()
	}

	due := !s.clock.Now().Before(s.nextCheck)

	execDue := s.options.Vendor == vendorCodex && s.child == nil && !s.clock.Now().Before(s.nextExec)
	if !due && !execDue {
		return 0, false
	}

	args := []string{"auth", "status"}
	if s.options.Vendor == vendorCodex {
		args = []string{commandLogin, "status"}
	}

	child, err := start(s.options.command(args...), false, &s.warning)
	if err != nil {
		return s.finish(1, "status_failed"), true
	}

	s.check = child

	s.checkAt = s.clock.Now()
	if s.child == nil && s.publish(phaseChecking, reasonNone) != nil {
		return 1, true
	}

	return 0, false
}

func (s *running) pollStatusCheck() (int, bool) {
	select {
	case err := <-s.check.done:
		s.check.closeTerminal()

		s.check = nil
		if err != nil {
			return s.statusFailure(err)
		}

		s.nextCheck = s.clock.Now().Add(statusInterval)
		if s.child == nil && !s.clock.Now().Before(s.nextExec) && s.launch() != nil {
			return s.finish(1, "start_failed"), true
		}
	default:
		if s.clock.Now().Sub(s.checkAt) >= checkTimeout {
			s.stopChildren()

			return s.finish(1, "status_failed"), true
		}
	}

	return 0, false
}

func (s *running) statusFailure(err error) (int, bool) {
	s.stopChildren()

	if exitCode(err) == 1 {
		return s.finish(0, "signed_out"), true
	}

	return s.finish(1, "status_failed"), true
}

func (s *running) limitReached() bool {
	cutoff := s.clock.Now().Add(-restartWindow)

	var recent []time.Time

	for _, at := range s.status.Restarts {
		if !at.Before(cutoff) {
			recent = append(recent, at)
		}
	}

	s.status.Restarts = recent

	return len(recent) >= restartMaximum
}

func (s *running) launch() error {
	if s.options.Vendor == vendorClaude && checkSettings(s.alias) != nil {
		return ErrArguments
	}

	args := []string{"--model", "claude-haiku-5-5", "/loop 5m " + s.options.Prompt}
	if s.options.Vendor == vendorCodex {
		args = []string{commandExec, "-m", "gpt-6-luna", s.options.Prompt}
	}

	child, err := start(s.options.command(args...), s.options.Vendor == vendorClaude, &s.warning)
	if err != nil {
		return err
	}

	s.child = child
	s.started = s.clock.Now()

	s.nextExec = s.started.Add(execInterval)
	if s.readingAt.IsZero() {
		s.readingAt = s.started
	}

	return s.publish(phaseRunning, reasonNone)
}

func (s *running) observeChild() (int, bool) {
	select {
	case err := <-s.child.done:
		return s.childEnded(err)
	default:
	}

	if s.options.Vendor != vendorClaude {
		return 0, false
	}

	return s.considerRestart()
}

func (s *running) childEnded(err error) (int, bool) {
	s.child.closeTerminal()

	s.child = nil
	if s.options.Vendor == vendorClaude || err != nil {
		s.status.Restarts = append(s.status.Restarts, s.clock.Now())
		if s.limitReached() {
			return s.finish(0, "restart_limit"), true
		}

		return s.finish(1, "child_exit"), true
	}

	if cleanRollouts(s.alias, s.clock.Now()) != nil {
		return s.finish(1, "cleanup_failed"), true
	}

	return 1, s.publish(phaseIdle, reasonNone) != nil
}

func (s *running) considerRestart() (int, bool) {
	observed, err := spool.SnapshotTime(s.root, s.options.Vendor, s.options.Account)
	if err != nil {
		return s.finish(1, "invalid_snapshot"), true
	}

	if observed.After(s.snapshotAt) && observed.After(s.readingAt) {
		s.readingAt = observed
		s.readingRestart = false
	}

	if observed.After(s.snapshotAt) {
		s.snapshotAt = observed
	}

	reason := ""
	if s.clock.Now().Sub(s.started) >= sessionLifetime {
		reason = "scheduled_restart"
	}

	if s.clock.Now().Sub(s.readingAt) >= readingInterval {
		if s.readingRestart {
			s.stopChildren()

			return s.finish(0, "reading_unavailable"), true
		}

		s.readingRestart = true
		s.readingAt = s.clock.Now()
		reason = "reading_restart"
	}

	if reason != "" {
		s.stopChildren()
		s.status.Restarts = append(s.status.Restarts, s.clock.Now())

		s.nextCheck = time.Time{}
		if s.limitReached() {
			return s.finish(0, "restart_limit"), true
		}

		if s.publish(phaseChecking, reason) != nil {
			return 1, true
		}
	}

	return 0, false
}

// exitCoder avoids exposing the process error, which can contain paths.
type exitCoder interface{ ExitCode() int }

func exitCode(err error) int {
	var coder exitCoder
	if errors.As(err, &coder) {
		return coder.ExitCode()
	}

	return -1
}

func cleanRollouts(alias *os.Root, now time.Time) error {
	for _, name := range []string{vendorCodex, "codex/sessions"} {
		info, err := alias.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		if err != nil || !info.IsDir() {
			return ErrArguments
		}
	}

	root, err := alias.OpenRoot("codex/sessions")
	if err != nil {
		return ErrArguments
	}
	defer root.Close()

	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrArguments
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		return removeOldRollout(root, name, now)
	})
	if err != nil {
		return ErrArguments
	}

	return nil
}

func removeOldRollout(root *os.Root, name string, now time.Time) error {
	info, err := root.Lstat(name)
	if err != nil {
		return ErrArguments
	}

	if !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= 2*24*time.Hour {
		return nil
	}

	if root.Remove(name) != nil {
		return ErrArguments
	}

	return nil
}

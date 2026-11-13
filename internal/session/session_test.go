// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session //nolint:testpackage // Tests inject the clock and inspect private process boundaries.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	fixtureAlias       = "sample"
	badAccount         = "Bad Name"
	reasonRestartLimit = "restart_limit"
	reasonSignedOut    = "signed_out"
	reasonStatusFailed = "status_failed"
	reasonInvalidDir   = "invalid_directory"
	reasonScheduled    = "scheduled_restart"
)

type fakeControl struct {
	Status     int  `json:"status"`
	ExecExit   int  `json:"execExit"`
	Warning    bool `json:"warning"`
	Hold       bool `json:"hold"`
	EndClaude  bool `json:"endClaude"`
	IgnoreTerm bool `json:"ignoreTerm"`
	HoldStatus bool `json:"holdStatus"`
}
type fakeCall struct {
	Dir  string   `json:"dir"`
	Args []string `json:"args"`
	Env  []string `json:"env"`
	PID  int      `json:"pid"`
}

// TestMain also supplies a fake executable built from this test source.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "fake-vendor" {
		exitAsFakeVendor()
	}

	os.Exit(m.Run())
}

// exitAsFakeVendor exits outside TestMain because the coverage tool rewrites os.Exit calls in TestMain.
func exitAsFakeVendor() { os.Exit(fakeVendor()) }

func fakeVendor() int {
	control, err := fakeSetup()
	if err != nil {
		return 9
	}

	if control.IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)

		if os.WriteFile("ready", nil, privateMode) != nil {
			return 9
		}
	}

	fmt.Fprintln(os.Stdout, "discarded child output")
	fmt.Fprintln(os.Stderr, "discarded child diagnostic")

	if strings.Join(os.Args[1:], " ") == "auth status" || strings.Join(os.Args[1:], " ") == "login status" {
		fakeHold(control.HoldStatus)

		return control.Status
	}

	if control.Warning {
		fmt.Fprint(os.Stdout, "Your login expires in ")
		fmt.Fprintln(os.Stdout, "3 days")
	}

	fakeHold((len(os.Args) > 1 && os.Args[1] == "--model" && !control.EndClaude) || control.Hold)

	return control.ExecExit
}

func fakeHold(hold bool) {
	for hold {
		time.Sleep(time.Hour)
	}
}

func fakeSetup() (fakeControl, error) {
	var control fakeControl

	dir, err := os.Getwd()
	if err != nil {
		return control, ErrArguments
	}

	base := dir
	if filepath.Base(dir) == "work" {
		base = filepath.Dir(dir)
	}

	file, err := os.OpenFile(filepath.Join(base, "calls"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return control, ErrArguments
	}

	err = json.NewEncoder(file).Encode(fakeCall{Args: os.Args[1:], Env: os.Environ(), Dir: dir, PID: os.Getpid()})
	if errors.Join(err, file.Close()) != nil {
		return control, ErrArguments
	}

	data, err := os.ReadFile(filepath.Join(base, "fake-control.json"))
	if err != nil || json.Unmarshal(data, &control) != nil {
		return control, ErrArguments
	}

	return control, nil
}

type fixture struct {
	clock   *testClock
	options Options
	base    string
}

func setup(t *testing.T, vendor string) fixture {
	t.Helper()
	dir := canonicalTempDir(t)

	options := Options{
		Vendor: vendor, Account: fixtureAlias, Root: dir, AliasFile: filepath.Join(dir, "approved"),
		Binary: filepath.Join(dir, "fake-vendor"), SpoolDir: filepath.Join(dir, "spool"), Prompt: "ping",
	}
	for _, sub := range []string{
		options.directory(), filepath.Join(options.directory(), "work"), filepath.Join(options.directory(), "home"),
		filepath.Join(options.directory(), "config"), filepath.Join(options.directory(), vendorCodex), options.SpoolDir,
	} {
		must(t, os.Mkdir(sub, 0o700))
	}

	if vendor == vendorClaude {
		rendered, err := RenderSettings(vendorClaude, fixtureAlias, options.Binary, options.SpoolDir)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(options.directory(), settingsPath), rendered, 0o600))
	}

	must(t, os.WriteFile(options.AliasFile, []byte("# approved aliases\n\n"+vendor+" sample ignored\n"), 0o600))

	executable, err := os.Executable()
	must(t, err)
	// The link keeps the test binary closed for writing. Copying it would leave a writable descriptor open,
	// and a parallel test's fork could inherit that descriptor. Linux then refuses to run the copy (ETXTBSY).
	must(t, os.Symlink(executable, options.Binary))

	fixtureValue := fixture{
		options: options,
		base:    dir,
		clock: &testClock{
			now:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			ticks: make(chan time.Time, 1),
			mu:    sync.Mutex{},
		},
	}

	var control fakeControl
	fixtureValue.control(t, control)

	return fixtureValue
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func (fixtureValue fixture) control(t *testing.T, c fakeControl) {
	t.Helper()

	data, err := json.Marshal(c)
	must(t, err)

	path := filepath.Join(fixtureValue.options.directory(), "fake-control.json")
	must(t, os.WriteFile(path+".tmp", data, 0o600))
	must(t, os.Rename(path+".tmp", path))
}

func (fixtureValue fixture) calls(t *testing.T) []fakeCall {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(fixtureValue.options.directory(), "calls"))
	if os.IsNotExist(err) {
		return nil
	}

	must(t, err)

	var calls []fakeCall

	decoder := json.NewDecoder(bytes.NewReader(data))

	for {
		var call fakeCall
		if decoder.Decode(&call) != nil {
			break
		}

		calls = append(calls, call)
	}

	return calls
}

func (fixtureValue fixture) status(t *testing.T) spool.SessionStatus {
	t.Helper()

	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	defer root.Close()

	// A read can fall between the runner's rename and its own open, so it retries.
	var status spool.SessionStatus

	for range 100 {
		status, err = spool.ReadSession(root, fixtureValue.options.Vendor, fixtureValue.options.Account)
		if err == nil {
			return status
		}

		time.Sleep(time.Millisecond)
	}

	must(t, err)

	return status
}

type testClock struct {
	now   time.Time
	ticks chan time.Time
	mu    sync.Mutex
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}
func (c *testClock) After(time.Duration) <-chan time.Time { return c.ticks }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	c.tick()
}

func (c *testClock) tick() {
	select {
	case c.ticks <- c.Now():
	default:
	}
}

func (fixtureValue fixture) until(t *testing.T, predicate func() bool) {
	t.Helper()

	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for !predicate() {
		select {
		case <-deadline.C:
			t.Fatal("condition timed out")
		case <-ticker.C:
			fixtureValue.clock.tick()
		}
	}
}

// running reports that the child started and the runner recorded its start time.
// Advancing the clock earlier would move the start time instead of the clock reading.
func (fixtureValue fixture) running(t *testing.T, calls int) bool {
	t.Helper()

	return len(fixtureValue.calls(t)) == calls && fixtureValue.status(t).Phase == phaseRunning
}

func (fixtureValue fixture) run(t *testing.T) (context.CancelFunc, <-chan int) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)

	finished := make(chan struct{})
	go func() {
		defer close(finished)

		result <- (Runner{Options: fixtureValue.options, Clock: fixtureValue.clock}).Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-finished:
		case <-time.After(60 * time.Second):
			t.Error("runner did not exit")
		}
	})

	return cancel, result
}

func TestSignedOutNeverStartsChild(t *testing.T) {
	t.Parallel()

	for _, vendor := range []string{vendorClaude, vendorCodex} {
		t.Run(vendor, func(t *testing.T) {
			t.Parallel()
			fixtureValue := setup(t, vendor)
			fixtureValue.control(
				t,
				fakeControl{
					Status:     1,
					ExecExit:   0,
					Warning:    false,
					Hold:       false,
					EndClaude:  false,
					IgnoreTerm: false,
					HoldStatus: false,
				},
			)
			_, result := fixtureValue.run(t)
			fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == reasonSignedOut })

			if got := <-result; got != 0 {
				t.Fatal(got)
			}

			if len(fixtureValue.calls(t)) != 1 {
				t.Fatal("child started")
			}
		})
	}
}

func TestStopWinsAndTerminatesChild(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"stop", "sample.stop"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixtureValue := setup(t, vendorClaude)
			cancel, _ := fixtureValue.run(t)
			fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) == 2 })
			calls := len(fixtureValue.calls(t))
			fixtureValue.control(
				t,
				fakeControl{
					Status:     1,
					Warning:    false,
					Hold:       false,
					EndClaude:  false,
					IgnoreTerm: false,
					HoldStatus: false,
					ExecExit:   0,
				},
			)

			before := time.Now()

			must(t, os.WriteFile(filepath.Join(fixtureValue.options.SpoolDir, name), nil, 0o600))
			fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == "operator_stop" })

			if !errors.Is(syscall.Kill(fixtureValue.calls(t)[1].PID, 0), syscall.ESRCH) {
				t.Fatal("child survived stop")
			}

			if time.Since(before) > 2*time.Second {
				t.Fatal("stop took too long")
			}

			fixtureValue.clock.advance(time.Minute)
			fixtureValue.until(t, func() bool { return fixtureValue.status(t).Time.Equal(fixtureValue.clock.Now()) })

			if len(fixtureValue.calls(t)) != calls {
				t.Fatal("status check ran while stopped")
			}

			must(t, os.Remove(filepath.Join(fixtureValue.options.SpoolDir, name)))
			fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == reasonSignedOut })
			cancel()
		})
	}
}

func TestInitiallyStoppedSkipsStatus(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorCodex)
	fixtureValue.control(
		t,
		fakeControl{
			Status:     1,
			EndClaude:  false,
			IgnoreTerm: false,
			HoldStatus: false,
			ExecExit:   0,
			Warning:    false,
			Hold:       false,
		},
	)
	must(t, os.WriteFile(filepath.Join(fixtureValue.options.SpoolDir, "stop"), nil, 0o600))
	cancel, _ := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == "operator_stop" })

	if len(fixtureValue.calls(t)) != 0 {
		t.Fatal("vendor ran")
	}

	cancel()
}

func TestUnapprovedNeverRunsVendor(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	must(t, os.WriteFile(fixtureValue.options.AliasFile, []byte("codex sample\nclaude another\n"), 0o600))

	var out bytes.Buffer
	if Login(fixtureValue.options, os.Stdin, &out, &out) != 2 {
		t.Fatal("login accepted")
	}

	if strings.Count(out.String(), "\n") != 1 {
		t.Fatal("refusal must be one line")
	}

	checkRunnerExit(t, fixtureValue, 0)

	if fixtureValue.status(t).Reason != "not_approved" || len(fixtureValue.calls(t)) != 0 {
		t.Fatal("unapproved vendor ran")
	}
}

func TestLoginDirectoryAndTerminal(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)

	var out bytes.Buffer

	input, err := os.Open(os.DevNull)
	must(t, err)

	defer input.Close()

	if Login(fixtureValue.options, input, &out, &out) != 2 {
		t.Fatal("accepted no terminal")
	}

	setMode(t, fixtureValue.options.directory(), 0o755)

	if Login(fixtureValue.options, input, &out, &out) != 2 {
		t.Fatal("accepted public directory")
	}

	if len(fixtureValue.calls(t)) != 0 {
		t.Fatal("vendor ran")
	}

	info, err := os.Stat(fixtureValue.options.directory())
	must(t, err)

	if ownedPrivate(info, os.Getuid()) {
		t.Fatal("public metadata accepted")
	}

	setMode(t, fixtureValue.options.directory(), 0o700)
	info, err = os.Stat(fixtureValue.options.directory())
	must(t, err)

	if ownedPrivate(info, os.Getuid()+1) {
		t.Fatal("foreign ownership accepted")
	}

	if loginAs(fixtureValue.options, input, &out, &out, os.Getuid()+1) != 2 {
		t.Fatal("login accepted foreign ownership")
	}

	if len(fixtureValue.calls(t)) != 0 {
		t.Fatal("vendor ran for foreign ownership")
	}
}

func TestLoginForwardsVendorAndUsesAliasEnvironment(t *testing.T) {
	t.Parallel()

	for _, vendor := range []string{vendorClaude, vendorCodex} {
		t.Run(vendor, func(t *testing.T) {
			t.Parallel()
			fixtureValue := setup(t, vendor)
			master, slave, err := pty.Open()
			must(t, err)

			defer master.Close()
			defer slave.Close()

			var out, diagnostic bytes.Buffer
			if Login(fixtureValue.options, slave, &out, &diagnostic) != 0 {
				t.Fatal("login failed")
			}

			if !strings.HasSuffix(out.String(), "sample login finished\n") {
				t.Fatal(out.String())
			}

			checkLoginCall(t, fixtureValue, vendor)
		})
	}
}

func checkLoginCall(t *testing.T, fixtureValue fixture, vendor string) {
	t.Helper()
	call := fixtureValue.calls(t)[0]

	want := []string{"auth", commandLogin}
	if vendor == vendorCodex {
		want = []string{commandLogin, "--device-auth"}
	}

	if !reflect.DeepEqual(want, call.Args) || call.Dir != fixtureValue.options.directory() {
		t.Fatal(call)
	}

	if !reflect.DeepEqual(fixtureValue.options.environment(), call.Env) {
		t.Fatal("unexpected environment")
	}
}

func TestWarningFlagAndNoOutputStored(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	fixtureValue.control(
		t,
		fakeControl{
			Warning:    true,
			Hold:       false,
			EndClaude:  false,
			IgnoreTerm: false,
			HoldStatus: false,
			Status:     0,
			ExecExit:   0,
		},
	)
	cancel, _ := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return fixtureValue.status(t).LoginExpiring })
	cancel()

	root, err := os.OpenRoot(fixtureValue.options.directory())
	must(t, err)

	defer root.Close()

	must(t, fs.WalkDir(root.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if e.IsDir() {
			return nil
		}

		data, err := root.ReadFile(path)
		if err != nil {
			return ErrArguments
		}

		if bytes.Contains(data, []byte(loginWarning)) || bytes.Contains(data, []byte("discarded child output")) {
			t.Fatal("child output stored")
		}

		return nil
	}))
	data, err := os.ReadFile(filepath.Join(fixtureValue.options.SpoolDir, "status", "claude-sample.json"))
	must(t, err)

	if bytes.Contains(data, []byte(loginWarning)) {
		t.Fatal("warning stored")
	}

	info, err := os.Stat(filepath.Join(fixtureValue.options.SpoolDir, "status", "claude-sample.json"))
	must(t, err)

	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

func TestReadingRestartThenStayDown(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	_, result := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return fixtureValue.running(t, 2) })
	fixtureValue.clock.advance(readingInterval)
	fixtureValue.until(t, func() bool { return len(fixtureValue.status(t).Restarts) == 1 })
	fixtureValue.until(
		t,
		func() bool { return fixtureValue.status(t).Phase == phaseRunning && len(fixtureValue.calls(t)) >= 4 },
	)
	fixtureValue.clock.advance(readingInterval)
	fixtureValue.until(t, func() bool { return fixtureValue.status(t).Reason == "reading_unavailable" })

	if <-result != 0 {
		t.Fatal("exit")
	}
}

func TestSixDayRestartWithFreshSnapshot(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	state := new(running)
	state.options, state.clock = fixtureValue.options, fixtureValue.clock
	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	defer root.Close()

	alias, err := fixtureValue.options.openAlias()
	must(t, err)

	defer alias.Close()

	state.root, state.alias = root, alias

	state.status.Vendor, state.status.Alias = vendorClaude, fixtureAlias
	defer state.stopChildren()

	must(t, state.launch())

	path := filepath.Join(fixtureValue.options.SpoolDir, "claude-sample.json")
	must(t, os.WriteFile(path, nil, 0o600))

	future := fixtureValue.clock.Now().Add(sessionLifetime)
	must(t, os.Chtimes(path, future, future))
	fixtureValue.clock.advance(sessionLifetime)

	code, done := state.considerRestart()
	if code != 0 || done || state.status.Reason != reasonScheduled || len(state.status.Restarts) != 1 {
		t.Fatal("scheduled restart was not requested")
	}

	must(t, state.launch())
}

func TestRestartLimitPersists(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorClaude)
	root, err := spool.SessionRoot(fixtureValue.options.SpoolDir)
	must(t, err)

	restarts := make([]time.Time, 5)
	for i := range restarts {
		restarts[i] = fixtureValue.clock.Now()
	}

	must(
		t,
		spool.WriteSession(
			root,
			spool.SessionStatus{
				Time:     fixtureValue.clock.Now(),
				Vendor:   vendorClaude,
				Alias:    fixtureAlias,
				Phase:    phaseStopped,
				Reason:   "child_exit",
				Restarts: restarts, LoginExpiring: false,
			},
		),
	)
	must(t, root.Close())

	checkRunnerExit(t, fixtureValue, 0)

	if fixtureValue.status(t).Reason != reasonRestartLimit || len(fixtureValue.calls(t)) != 0 {
		t.Fatal("limit ignored")
	}
}

func TestCodexTimerAndRolloutCleanup(t *testing.T) {
	t.Parallel()
	fixtureValue := setup(t, vendorCodex)
	sessions := filepath.Join(fixtureValue.options.directory(), vendorCodex, "sessions")
	must(t, os.Mkdir(sessions, 0o700))
	old := filepath.Join(sessions, "old")
	fresh := filepath.Join(sessions, "fresh")

	target := filepath.Join(fixtureValue.base, "target")
	for _, path := range []string{old, fresh, target} {
		must(t, os.WriteFile(path, nil, 0o600))
	}

	before := fixtureValue.clock.Now().Add(-3 * 24 * time.Hour)
	must(t, os.Chtimes(old, before, before))
	must(t, os.Chtimes(target, before, before))
	must(t, os.Chtimes(fresh, fixtureValue.clock.Now(), fixtureValue.clock.Now()))
	must(t, os.Symlink(target, filepath.Join(sessions, "link")))
	must(t, os.Mkdir(filepath.Join(sessions, "directory"), 0o700))
	must(t, syscall.Mkfifo(filepath.Join(sessions, "pipe"), 0o600))
	cancel, _ := fixtureValue.run(t)
	fixtureValue.until(t, func() bool { return fixtureValue.status(t).Phase == phaseIdle })

	_, err := os.Lstat(old)
	if !os.IsNotExist(err) {
		t.Fatal("old file retained")
	}

	names := make([]string, 0, 5)

	names = append(names, fresh, target)
	for _, name := range []string{"link", "directory", "pipe"} {
		names = append(names, filepath.Join(sessions, name))
	}

	for _, name := range names {
		_, err = os.Lstat(name)
		must(t, err)
	}

	fixtureValue.clock.advance(execInterval - time.Second)
	fixtureValue.until(t, func() bool { return len(fixtureValue.calls(t)) >= 3 })

	for _, call := range fixtureValue.calls(t)[2:] {
		if call.Args[0] == commandExec {
			t.Fatal("early execution")
		}
	}

	fixtureValue.clock.advance(time.Second)
	fixtureValue.until(t, func() bool {
		count := 0

		for _, call := range fixtureValue.calls(t) {
			if call.Args[0] == commandExec {
				count++
			}
		}

		return count == 2
	})
	cancel()
}

func TestSettingsAndHardening(t *testing.T) {
	t.Parallel()
	base := canonicalTempDir(t)
	binary := filepath.Join(base, "binary with ' quote")
	dir := filepath.Join(base, "spool with ' quote")
	data, err := RenderSettings(vendorClaude, fixtureAlias, binary, dir)
	must(t, err)

	var settings map[string]json.RawMessage
	must(t, json.Unmarshal(data, &settings))

	var line struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Padding int    `json:"padding"`
	}
	must(t, json.Unmarshal(settings["statusLine"], &line))

	if line.Type != "command" || line.Padding != 0 || !strings.Contains(line.Command, shellQuote(binary)) {
		t.Fatal(line)
	}

	var permissions struct {
		DefaultMode   string   `json:"defaultMode"`
		DisableBypass string   `json:"disableBypassPermissionsMode"`
		Allow         []string `json:"allow"`
		Deny          []string `json:"deny"`
	}
	must(t, json.Unmarshal(settings["permissions"], &permissions))

	if !reflect.DeepEqual(permissions.Allow, []string{"CronCreate", "CronDelete", "CronList"}) ||
		permissions.DefaultMode != "dontAsk" || permissions.DisableBypass != "disable" {
		t.Fatal(permissions)
	}

	wantDeny := []string{
		"Agent", "Task", "Bash", "Read", "Write", "Edit", "MultiEdit", "Glob", "Grep", "LS", "NotebookEdit",
		"WebFetch", "WebSearch", "TodoWrite", "Skill", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode",
		"EnterWorktree", "ExitWorktree", "ToolSearch", "mcp__*",
	}
	if !reflect.DeepEqual(permissions.Deny, wantDeny) {
		t.Fatal(permissions.Deny)
	}

	var statusLine map[string]json.RawMessage
	must(t, json.Unmarshal(settings["statusLine"], &statusLine))

	if _, found := statusLine["refreshInterval"]; found || bytes.Contains(data, []byte("refreshInterval")) {
		t.Fatal("status line refresh interval set")
	}

	data, err = RenderSettings(vendorCodex, fixtureAlias, binary, dir)
	must(t, err)

	if string(data) != "cli_auth_credentials_store = \"file\"\nmodel = \"gpt-6-luna\"\n" {
		t.Fatal(string(data))
	}

	for _, alias := range []string{"../escape", "bad/name", "", "Upper", strings.Repeat("a", 33)} {
		_, err = RenderSettings(vendorClaude, alias, binary, dir)
		if err == nil {
			t.Fatal("unsafe alias accepted")
		}
	}

	for _, arguments := range [][]string{
		{vendorClaude, "relative-binary", dir},
		{vendorClaude, binary, "relative-dir"},
		{"other", binary, dir},
	} {
		_, err = RenderSettings(arguments[0], fixtureAlias, arguments[1], arguments[2])
		if !errors.Is(err, ErrArguments) {
			t.Fatal("unsafe arguments accepted:", arguments)
		}
	}
}

// This test sets parent environment variables, so it runs alone.
func TestChildEnvironmentOmitsAPIKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-anthropic")
	t.Setenv("OPENAI_API_KEY", "synthetic-openai")
	t.Setenv("EXAMPLE_API_KEY", "synthetic-example")

	for _, vendor := range []string{vendorClaude, vendorCodex} {
		fixtureValue := setup(t, vendor)

		for _, entry := range fixtureValue.options.command(commandExec).Env {
			name, value, _ := strings.Cut(entry, "=")
			if strings.HasSuffix(name, "_API_KEY") || strings.HasPrefix(value, "synthetic-") {
				t.Fatal(vendor, "child environment holds", name)
			}
		}
	}
}

func checkRunnerExit(t *testing.T, fixtureValue fixture, want int) {
	t.Helper()

	got := (Runner{Options: fixtureValue.options, Clock: fixtureValue.clock}).Run(context.Background())
	if got != want {
		t.Fatalf("exit = %d, want %d", got, want)
	}
}

func setMode(t *testing.T, path string, mode os.FileMode) { t.Helper(); must(t, os.Chmod(path, mode)) }

// canonicalTempDir returns a temporary directory without symbolic link components.
// macOS places t.TempDir under /var, which links to /private/var.
func canonicalTempDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

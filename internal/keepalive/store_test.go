// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//nolint:testpackage // Fault fixtures exercise internal filesystem errors and pure guards.
package keepalive

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func spoolDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	return dir
}

func TestValidation(t *testing.T) {
	t.Parallel()

	for _, alias := range []string{fixtureA, "0", "work_2-a", strings.Repeat(fixtureA, 32)} {
		assert.True(t, validAlias(alias))
	}

	for _, alias := range []string{"", "@", "UPPER", "-work", "a.b", "../work", "a b", strings.Repeat(fixtureA, 33)} {
		assert.False(t, validAlias(alias))
	}

	for _, vendor := range []string{fixtureClaude, fixtureAgy, fixtureCodex} {
		value := enrollment()
		value.Vendor = vendor
		assert.True(t, validEnrollment(value, []string{fixtureWork}))
	}

	for _, window := range []string{"5h", "7d", "7d_opus", "7d_sonnet", fixtureOther} {
		value := enrollment()
		value.Windows = []string{window}
		assert.True(t, validEnrollment(value, []string{fixtureWork}))
	}

	for _, change := range []func(*Enrollment){
		func(e *Enrollment) { e.Alias = "@" },
		func(e *Enrollment) { e.Vendor = fixtureUnknown },
		func(e *Enrollment) { e.Windows = nil },
		func(e *Enrollment) { e.Windows = []string{fixtureUnknown} },
		func(e *Enrollment) { e.Windows = []string{"5h", "5h"} },
		func(e *Enrollment) { e.DebitAllowance = math.NaN() },
	} {
		value := enrollment()
		change(&value)
		assert.False(t, validEnrollment(value, []string{value.Alias}))
	}

	for _, change := range []func(*Status){
		func(s *Status) { s.Alias = "@" },
		func(s *Status) { s.At = time.Time{} },
		func(s *Status) { s.Vendor = fixtureUnknown },
		func(s *Status) { s.Reason = fixtureUnknown },
	} {
		value := initialState().Status
		change(&value)
		assert.False(t, validStatus(value))
	}
}

func TestLedgerValidation(t *testing.T) {
	t.Parallel()

	assert.True(t, validState(initialState(), enrollment(), instant()))

	for _, fixture := range []struct {
		change func(*State)
		name   string
	}{
		{func(s *State) { s.Status.Phase = "invalid" }, fixtureStatus},
		{func(s *State) { s.Enrollment.Alias = fixturePersonal }, "enrollment"},
		{func(s *State) { s.Status.Alias = fixturePersonal }, "status alias"},
		{func(s *State) { s.Status.Vendor = fixtureCodex }, "status vendor"},
		{func(s *State) { s.Enrollment.Vendor = fixtureCodex }, "ledger vendor"},
		{func(s *State) { s.Enrollment.DebitAllowance = 0.4 }, "allowance"},
		{func(s *State) { s.Enrollment.Windows = []string{"5h"} }, "windows"},
		{func(s *State) {
			s.LastAt = time.Time{}
			s.Status.At = time.Time{}.Add(-time.Second)
			s.Attempts = nil
			s.InputCount = 0
			s.PendingAt = time.Time{}
		}, "zero checkpoint"},
		{func(s *State) { s.LastAt = instant().Add(time.Nanosecond) }, "future checkpoint"},
		{func(s *State) { s.Status.At = instant().Add(time.Nanosecond) }, "future status"},
		{func(s *State) { s.PendingAt = instant().Add(time.Nanosecond) }, "future pending"},
		{func(s *State) { s.Retry = -1 }, "negative retry"},
		{func(s *State) { s.Retry = 4 }, "excess retry"},
		{func(s *State) { s.InputCount = 1 }, "count mismatch"},
		{func(s *State) { s.Attempts[0].At = time.Time{} }, "zero attempt"},
		{func(s *State) { s.Attempts[1].At = instant().Add(-2 * time.Hour); s.PendingAt = time.Time{} }, "unordered attempt"},
		{func(s *State) { s.Attempts[1].At = instant().Add(time.Nanosecond); s.PendingAt = time.Time{} }, "future attempt"},
		{func(s *State) { s.Attempts[0].Debit = -1 }, "invalid debit"},
		{func(s *State) { s.PendingAt = instant().Add(-time.Second) }, "pending mismatch"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			state := initialState()
			state.Attempts = []Attempt{{At: instant().Add(-time.Hour), Debit: 0}, {At: instant(), Debit: 1}}
			state.InputCount = 2
			state.PendingAt = instant()
			assert.True(t, validState(state, enrollment(), instant()))
			fixture.change(&state)
			assert.False(t, validState(state, enrollment(), instant()))
		})
	}
}

func TestStatusSpool(t *testing.T) {
	t.Parallel()

	dir := spoolDir(t)
	store := Store{Dir: dir}
	status := initialState().Status
	require.NoError(t, store.WriteStatus(status))

	got, err := store.ReadStatus(fixtureWork)
	require.NoError(t, err)
	assert.Equal(t, status, got)

	status.Phase, status.Reason = fixtureStuck, "login_required"
	require.NoError(t, store.WriteStatus(status))

	got, err = store.ReadStatus(fixtureWork)
	require.NoError(t, err)
	assert.Equal(t, status, got)

	entries, err := os.ReadDir(store.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "work.status", entries[0].Name())

	info, err := entries[0].Info()
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())

	data, err := os.ReadFile(filepath.Join(dir, "work.status"))
	require.NoError(t, err)
	assert.JSONEq(
		t,
		`{"Alias":"work","Vendor":"claude","Phase":"stuck","Reason":"login_required","At":"1970-01-24T03:33:20Z"}`,
		string(data),
	)

	for _, alias := range []string{"../work", fixturePersonal} {
		_, err = store.ReadStatus(alias)
		require.ErrorIs(t, err, ErrStateInvalid)
	}

	status.Reason = fixtureUnknown
	require.ErrorIs(t, store.WriteStatus(status), ErrStateInvalid)

	badStore := Store{Dir: ""}
	_, err = badStore.ReadStatus(fixtureWork)
	require.ErrorIs(t, err, ErrStateInvalid)
	require.ErrorIs(t, badStore.WriteStatus(initialState().Status), ErrStateInvalid)

	root, err := openSpool(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	for _, content := range []string{
		"{", "{}", `{"Alias":"work","Extra":1}`, `{} {}`, `{} false`,
		string(data) + " {}", string(data) + " broken",
		strings.Replace(string(data), `"claude"`, `"unknown"`, 1),
		strings.Replace(string(data), `"login_required"`, `"unknown"`, 1),
		strings.TrimSuffix(string(data), "}") + `,"Extra":1}`,
		`{"Alias":"personal","Vendor":"claude","Phase":"running","Reason":"none","At":"1970-01-24T03:33:20Z"}`,
	} {
		require.NoError(t, root.WriteFile("work.status", []byte(content), 0o600))

		_, err = store.ReadStatus(fixtureWork)
		require.ErrorIs(t, err, ErrStateInvalid)
	}
}

func TestSpoolBoundaries(t *testing.T) {
	t.Parallel()

	dir := spoolDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0o700))
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "linked")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixtureFile), nil, 0o600))

	for _, name := range []string{
		"", ".", "relative", filepath.Join(dir, "absent"), filepath.Join(dir, "linked"), filepath.Join(dir, fixtureFile),
	} {
		root, err := openSpool(name)
		require.ErrorIs(t, err, ErrStateInvalid)
		assert.Nil(t, root)
	}

	root, err := openSpool(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	for _, name := range []string{"../file", "real/file", ".", filepath.Join(dir, fixtureFile)} {
		_, err = regular(root, name)
		require.ErrorIs(t, err, ErrStateInvalid)
		require.ErrorIs(t, write(root, name, initialState().Status, false), ErrStateInvalid)
		require.ErrorIs(t, read(root, name, new(Status)), ErrStateInvalid)
	}

	outside := spoolDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(outside, fixtureUntouched), []byte("safe"), 0o600))

	for _, target := range []string{fixtureFile, "absent", filepath.Join(outside, fixtureUntouched)} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			t.Parallel()

			store := Store{Dir: spoolDir(t)}

			for _, name := range []string{"work.status", "work.ledger", fixtureStop, "work.stop"} {
				require.NoError(t, os.Symlink(target, filepath.Join(store.Dir, name)))
			}

			_, readErr := store.ReadStatus(fixtureWork)
			require.ErrorIs(t, readErr, ErrStateInvalid)
			require.ErrorIs(t, store.WriteStatus(initialState().Status), ErrStateInvalid)

			_, readErr = store.load(enrollment(), instant())
			require.ErrorIs(t, readErr, ErrStateInvalid)
			require.ErrorIs(t, store.save(initialState(), true), ErrStateInvalid)

			stopped, stopErr := store.stopped(fixtureWork)
			require.ErrorIs(t, stopErr, ErrStateInvalid)
			assert.False(t, stopped)
			require.NoError(t, os.Remove(filepath.Join(store.Dir, fixtureStop)))

			stopped, stopErr = store.stopped(fixtureWork)
			require.ErrorIs(t, stopErr, ErrStateInvalid)
			assert.False(t, stopped)
		})
	}

	data, err := os.ReadFile(filepath.Join(outside, fixtureUntouched))
	require.NoError(t, err)
	assert.Equal(t, "safe", string(data))
}

const fixtureOpenErrorWithFile = "open error with file"

type faultySpool struct {
	*os.Root

	file  *os.File
	fault string
}

//nolint:wrapcheck // The fake preserves filesystem errors for the store to classify.
func (root faultySpool) Lstat(name string) (fs.FileInfo, error) {
	if root.fault == fixtureStat {
		return nil, fs.ErrPermission
	}

	return root.Root.Lstat(name)
}

//nolint:wrapcheck // The fake preserves filesystem errors for the store to classify.
func (root faultySpool) Open(name string) (*os.File, error) {
	if root.fault == fixtureOpen || root.fault == "directory open" && name == "." {
		return nil, fs.ErrPermission
	}

	if root.fault == fixtureDifferentFile {
		return root.Root.Open(fixtureOther)
	}

	if root.fault == fixtureOpenErrorWithFile {
		return root.file, fs.ErrPermission
	}

	file, err := root.Root.Open(name)

	if root.fault == fixtureClosedRead || root.fault == "directory sync" && name == "." {
		return file, errors.Join(err, file.Close())
	}

	return file, err
}

//nolint:wrapcheck // The fake preserves filesystem errors for the store to classify.
func (root faultySpool) OpenFile(name string, flags int, mode fs.FileMode) (*os.File, error) {
	if root.fault == "create" {
		return nil, fs.ErrPermission
	}

	file, err := root.Root.OpenFile(name, flags, mode)

	if root.fault == "closed write" {
		return file, errors.Join(err, file.Close())
	}

	return file, err
}

//nolint:wrapcheck // Each filesystem operation has an independent failure fixture.
func (root faultySpool) Link(oldName, newName string) error {
	if root.fault == fixtureLink {
		return fs.ErrPermission
	}

	return root.Root.Link(oldName, newName)
}

//nolint:wrapcheck // Each filesystem operation has an independent failure fixture.
func (root faultySpool) Rename(oldName, newName string) error {
	if root.fault == "rename" {
		return fs.ErrPermission
	}

	return root.Root.Rename(oldName, newName)
}

func TestAtomicFailures(t *testing.T) {
	t.Parallel()

	for _, fault := range []string{
		fixtureStat, fixtureOpen, fixtureOpenErrorWithFile, fixtureClosedRead, fixtureDifferentFile, "create", "closed write",
		fixtureLink, "rename", "directory open", "directory sync",
	} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()

			dir := spoolDir(t)
			root, err := openSpool(dir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			require.NoError(t, write(root, fixtureStatus, initialState().Status, false))
			require.NoError(t, write(root, fixtureOther, initialState().Status, false))

			fake := faultySpool{Root: root, fault: fault, file: nil}

			if fault == fixtureOpenErrorWithFile {
				fake.file, err = root.Open(fixtureStatus)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, fake.file.Close()) })
			}

			switch fault {
			case fixtureStat, fixtureOpen, fixtureOpenErrorWithFile, fixtureClosedRead, fixtureDifferentFile:
				require.ErrorIs(t, read(fake, fixtureStatus, new(Status)), ErrStateInvalid)
			default:
				require.ErrorIs(
					t,
					write(fake, fixtureStatus, initialState().Status, fault == fixtureLink),
					ErrStateInvalid,
				)
			}

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, entries, 2)
			assert.Equal(t, fixtureOther, entries[0].Name())
			assert.Equal(t, fixtureStatus, entries[1].Name())
		})
	}

	root, err := openSpool(spoolDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	require.ErrorIs(t, write(root, fixtureStatus, math.NaN(), false), ErrStateInvalid)
}

func TestMissingSpool(t *testing.T) {
	t.Parallel()

	store := Store{Dir: ""}
	_, err := store.load(enrollment(), instant())
	require.ErrorIs(t, err, ErrStateInvalid)
	require.ErrorIs(t, store.save(initialState(), false), ErrStateInvalid)

	stopped, stopErr := store.stopped(fixtureWork)
	require.ErrorIs(t, stopErr, ErrStateInvalid)
	assert.False(t, stopped)

	store.Dir = spoolDir(t)
	require.ErrorIs(t, store.save(initialState(), false), ErrStateInvalid)
	require.NoError(t, store.save(initialState(), true))

	state, err := store.load(enrollment(), instant())
	require.NoError(t, err)
	assert.Equal(t, initialState(), state)

	data, err := os.ReadFile(filepath.Join(store.Dir, "work.ledger"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(store.Dir, "work.status")))
	require.ErrorIs(t, store.save(initialState(), true), ErrStateInvalid)

	after, err := os.ReadFile(filepath.Join(store.Dir, "work.ledger"))
	require.NoError(t, err)
	assert.Equal(t, data, after)

	state.InputCount = 1
	require.ErrorIs(t, store.save(state, false), ErrStateInvalid)

	var ledger map[string]any
	require.NoError(t, json.Unmarshal(data, &ledger))

	ledger["InputCount"] = state.InputCount

	corrupt, err := json.Marshal(ledger)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir, "work.ledger"), corrupt, 0o600))

	_, err = store.load(enrollment(), instant())
	require.ErrorIs(t, err, ErrStateInvalid)
}

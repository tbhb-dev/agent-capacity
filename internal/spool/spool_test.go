// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool //nolint:testpackage // Exercise temporary file failures through the private writer.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	claudeVendor = "claude"
	invalidClass = "invalid"
	windowsOS    = "windows"
)

func sample() Snapshot {
	return Snapshot{
		Vendor:     claudeVendor,
		Account:    "work",
		ObservedAt: time.Unix(100000, 0).UTC(),
		Data:       json.RawMessage(`{"five_hour":null}`),
	}
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	for _, vendor := range []string{claudeVendor, "agy"} {
		for _, account := range []string{"a", "0", "work_2-a", strings.Repeat("a", 32)} {
			assert.True(t, ValidIdentity(vendor, account))
		}
	}

	for _, account := range []string{
		"", "@", "work@", "UPPER", "-work", "_work", "a.b", "a b", "é", "../work",
		strings.Repeat("a", 33),
	} {
		assert.False(t, ValidIdentity(claudeVendor, account))
	}

	assert.False(t, ValidIdentity("codex", "work"))
}

func TestWriteAtomic(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "spool")
	snapshot := sample()
	snapshot.ObservedAt = snapshot.ObservedAt.In(time.FixedZone("offset", 3600))
	require.NoError(t, Write(dir, snapshot))
	path := filepath.Join(dir, "claude-work.json")
	snapshot.Data = json.RawMessage(`{"seven_day":null}`)

	if runtime.GOOS != windowsOS {
		old, err := os.Open(path)
		require.NoError(t, err)

		defer old.Close()

		require.NoError(t, Write(dir, snapshot))

		var previous Snapshot
		require.NoError(t, json.NewDecoder(old).Decode(&previous))
		assert.JSONEq(t, `{"five_hour":null}`, string(previous.Data))
	} else {
		// Windows can replace an existing snapshot when no reader holds it open.
		require.NoError(t, Write(dir, snapshot))
	}

	got, failures, err := Read(dir, claudeVendor)
	require.NoError(t, err)
	assert.Empty(t, failures)

	snapshot.ObservedAt = snapshot.ObservedAt.UTC()
	assert.Equal(t, []Snapshot{snapshot}, got)

	info, err := os.Stat(path)
	require.NoError(t, err)

	if runtime.GOOS != windowsOS {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	info, err = os.Stat(dir)
	require.NoError(t, err)

	if runtime.GOOS != windowsOS {
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "claude-work.json", entries[0].Name())
}

func TestWriteHeldSnapshotWindows(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != windowsOS {
		t.Skip("Windows refuses replacement while a reader holds the snapshot open")
	}

	dir := t.TempDir()
	require.NoError(t, Write(dir, sample()))
	old, err := os.Open(filepath.Join(dir, "claude-work.json"))
	require.NoError(t, err)

	defer old.Close()

	snapshot := sample()
	snapshot.Data = json.RawMessage(`{"seven_day":null}`)
	err = Write(dir, snapshot)
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Equal(t, "permission", capacity.ErrorClass(err))

	var previous Snapshot
	require.NoError(t, json.NewDecoder(old).Decode(&previous))
	assert.Equal(t, sample(), previous)

	got, failures, err := Read(dir, claudeVendor)
	require.NoError(t, err)
	assert.Empty(t, failures)
	assert.Equal(t, []Snapshot{sample()}, got)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "claude-work.json", entries[0].Name())
	require.NoError(t, old.Close())
	require.NoError(t, Write(dir, snapshot))
	got, failures, err = Read(dir, claudeVendor)
	require.NoError(t, err)
	assert.Empty(t, failures)
	assert.Equal(t, []Snapshot{snapshot}, got)
}

func TestWriteFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.ErrorIs(t, Write("", sample()), ErrDirectory)
	invalid := sample()
	invalid.Account = "@"
	require.ErrorIs(t, Write(dir, invalid), capacity.ErrInvalidReading)
	invalid = sample()
	invalid.ObservedAt = time.Time{}
	require.ErrorIs(t, Write(dir, invalid), capacity.ErrInvalidReading)
	invalid = sample()
	invalid.Data = json.RawMessage(`{`)
	require.ErrorIs(t, Write(dir, invalid), capacity.ErrInvalidReading)
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.Error(t, write(file, sample(), func(string, string) (*os.File, error) {
		assert.Fail(t, "temporary file attempted after directory failure")

		return nil, errors.ErrUnsupported
	}))
	require.ErrorIs(t, write(dir, sample(), func(string, string) (*os.File, error) {
		return nil, errors.ErrUnsupported
	}), errors.ErrUnsupported)
	require.Error(t, write(dir, sample(), func(dir, pattern string) (*os.File, error) {
		closed, err := os.CreateTemp(dir, pattern)
		require.NoError(t, err)
		require.NoError(t, closed.Close())

		return closed, nil
	}))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "claude-work.json"), 0o700))
	require.Error(t, Write(dir, sample()))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, Write(dir, sample()))

	for _, name := range []string{"other.json", "claude-work.tmp", ".snapshot-unused"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(`{`), 0o600))
	}

	for _, data := range []string{
		`{`, `null`, `{"vendor":"claude","account":"@"}`,
		`{"vendor":"agy","account":"work","observed_at":"1970-01-02T03:46:40Z","data":{}}`,
		`{"vendor":"claude","account":"other","observed_at":"1970-01-02T03:46:40Z","data":{}}`,
		`{"vendor":"claude","account":"bad","data":{}}`,
		`{"vendor":"claude","account":"bad","observed_at":"1970-01-02T03:46:40Z"}`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-bad.json"), []byte(data), 0o600))
		got, failures, err := Read(dir, claudeVendor)
		require.NoError(t, err)
		assert.Equal(t, []Snapshot{sample()}, got)
		assert.Equal(t, []string{invalidClass}, failures)
	}

	require.NoError(t, os.Mkdir(filepath.Join(dir, "claude-directory.json"), 0o700))
	_, failures, err := Read(dir, claudeVendor)
	require.NoError(t, err)

	assert.Equal(t, []string{invalidClass, "read"}, failures)
	got, failures, err := Read(filepath.Join(dir, "missing"), claudeVendor)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Nil(t, failures)

	_, _, err = Read("", claudeVendor)
	require.ErrorIs(t, err, ErrDirectory)
	_, _, err = Read(filepath.Join(dir, "other.json"), claudeVendor)
	require.Error(t, err)
}

func TestReadOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-z.json"), []byte(`{`), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "claude-a.json"), 0o700))
	got, failures, err := Read(dir, claudeVendor)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Equal(t, []string{"read", invalidClass}, failures)
}

func TestReadIdentity(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct{ name, vendor, account string }{
		{"claude-@.json", claudeVendor, "@"},
		{"claude-bad-work.json", "claude-bad", "work"},
	} {
		dir := t.TempDir()
		snapshot := sample()
		snapshot.Vendor = testCase.vendor
		snapshot.Account = testCase.account
		data, err := json.Marshal(snapshot)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, testCase.name), data, 0o600))
		got, failures, err := Read(dir, claudeVendor)
		require.NoError(t, err)
		assert.Nil(t, got)
		assert.Equal(t, []string{invalidClass}, failures)
	}
}

func TestFilesystemErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	var cause *os.PathError

	err := Write(file, sample())
	require.ErrorAs(t, err, &cause)
	_, _, err = Read(file, claudeVendor)
	require.ErrorAs(t, err, &cause)
	assert.Equal(t, "read", capacity.ErrorClass(err))
	_, _, err = Read(filepath.Join(dir, "invalid\x00"), claudeVendor)
	require.ErrorAs(t, err, &cause)
	assert.Equal(t, "read", capacity.ErrorClass(err))
	_, err = readFile(dir, "missing.json")
	require.ErrorIs(t, err, os.ErrNotExist)
}

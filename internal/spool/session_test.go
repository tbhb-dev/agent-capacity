// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool //nolint:testpackage // Exercise the rooted status and metadata boundaries.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	sessionAlias  = "sample"
	sessionVendor = "codex"
)

func testSessionRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := canonicalTempDir(t)
	root, err := SessionRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	return dir, root
}

func TestSessionStatusAndBoundaries(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)
	status := SessionStatus{
		Time:          time.Now().UTC(),
		Vendor:        sessionVendor,
		Alias:         sessionAlias,
		Phase:         "running",
		Reason:        "none",
		Restarts:      []time.Time{time.Now().UTC()},
		LoginExpiring: false,
	}
	require.NoError(t, WriteSession(root, status))
	got, err := ReadSession(root, sessionVendor, sessionAlias)
	require.NoError(t, err)
	require.Equal(t, status, got)

	info, err := os.Stat(filepath.Join(dir, "status", "codex-sample.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	_, err = SessionRoot("relative")
	require.Error(t, err)
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	_, err = SessionRoot(link)
	require.Error(t, err)
}

func TestSessionAliases(t *testing.T) {
	t.Parallel()
	_, root := testSessionRoot(t)

	var status SessionStatus
	for _, account := range []string{"../outside", "bad/name", "", "Upper"} {
		status.Alias = account
		require.Error(t, WriteSession(root, status))
		_, err := ReadSession(root, claudeVendor, account)
		require.Error(t, err)
		_, err = Stopped(root, account)
		require.Error(t, err)
		_, err = SnapshotTime(root, claudeVendor, account)
		require.Error(t, err)
	}
}

func TestSessionRejectsLinksAndOnlyInspectsOwnSnapshot(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-other.json"), []byte("invalid"), 0o600))

	observed, err := SnapshotTime(root, claudeVendor, sessionAlias)
	require.NoError(t, err)
	require.True(t, observed.IsZero())
	require.NoError(t, os.Symlink("claude-other.json", filepath.Join(dir, "claude-sample.json")))

	_, err = SnapshotTime(root, claudeVendor, sessionAlias)
	require.Error(t, err)
	require.NoError(t, os.Symlink("claude-other.json", filepath.Join(dir, "sample.stop")))

	stopped, err := Stopped(root, sessionAlias)
	require.NoError(t, err)
	require.True(t, stopped)
	require.NoError(t, os.Symlink(dir, filepath.Join(dir, "status")))

	var status SessionStatus

	status.Time, status.Vendor, status.Alias = time.Now(), claudeVendor, sessionAlias
	require.Error(t, WriteSession(root, status))
}

func restrictMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { require.NoError(t, os.Chmod(path, sessionDirectoryMode)) })
}

func validStatus() SessionStatus {
	return SessionStatus{
		Time: time.Now().UTC(), Alias: sessionAlias, Vendor: sessionVendor,
		Phase: "running", Reason: "none", Restarts: nil, LoginExpiring: false,
	}
}

func TestSessionRootRejectsUnusableDirectories(t *testing.T) {
	t.Parallel()
	dir := canonicalTempDir(t)

	_, err := SessionRoot(filepath.Join(dir, "missing"))
	require.Error(t, err)

	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, sessionFileMode))
	_, err = SessionRoot(file)
	require.Error(t, err)
}

func TestStoppedRejectsUnreadableRoot(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)
	restrictMode(t, dir, 0)

	_, err := Stopped(root, sessionAlias)
	require.Error(t, err)
}

func TestStoppedWithoutControls(t *testing.T) {
	t.Parallel()
	_, root := testSessionRoot(t)

	stopped, err := Stopped(root, sessionAlias)
	require.NoError(t, err)
	require.False(t, stopped)
}

func TestReadSessionWithoutStatus(t *testing.T) {
	t.Parallel()
	_, root := testSessionRoot(t)

	got, err := ReadSession(root, sessionVendor, sessionAlias)
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestSnapshotTimeReadsRegularFiles(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)
	path := filepath.Join(dir, "codex-sample.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), sessionFileMode))

	modified := time.Unix(100000, 0)
	require.NoError(t, os.Chtimes(path, modified, modified))

	observed, err := SnapshotTime(root, sessionVendor, sessionAlias)
	require.NoError(t, err)
	require.True(t, observed.Equal(modified))

	restrictMode(t, dir, 0)

	_, err = SnapshotTime(root, sessionVendor, sessionAlias)
	require.Error(t, err)
}

func TestSessionStatusDirectoryFailures(t *testing.T) {
	t.Parallel()

	for name, mode := range map[string]os.FileMode{
		"closed status directory": 0,
		"unsearchable status":     0o400,
		"read only status":        0o500,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, root := testSessionRoot(t)
			status := filepath.Join(dir, "status")
			require.NoError(t, os.Mkdir(status, sessionDirectoryMode))
			restrictMode(t, status, mode)

			require.Error(t, WriteSession(root, validStatus()))
		})
	}

	t.Run("root without write access", func(t *testing.T) {
		t.Parallel()
		dir, root := testSessionRoot(t)
		restrictMode(t, dir, 0)

		require.Error(t, WriteSession(root, validStatus()))

		_, err := ReadSession(root, sessionVendor, sessionAlias)
		require.Error(t, err)
	})
}

func TestWriteSessionValidatesStatus(t *testing.T) {
	t.Parallel()
	_, root := testSessionRoot(t)

	status := validStatus()
	status.Time = time.Time{}

	require.Error(t, WriteSession(root, status))

	status.Time = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	require.Error(t, WriteSession(root, status))
}

func TestWriteSessionReplacesRegularFilesOnly(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)

	status := validStatus()
	require.NoError(t, WriteSession(root, status))

	status.Phase = "stopped"
	require.NoError(t, WriteSession(root, status))

	got, err := ReadSession(root, sessionVendor, sessionAlias)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Phase)

	other := validStatus()
	other.Alias = "other"

	require.NoError(t, os.Mkdir(filepath.Join(dir, "status", "codex-other.json"), sessionDirectoryMode))
	require.Error(t, WriteSession(root, other))

	_, err = ReadSession(root, sessionVendor, "other")
	require.Error(t, err)
}

//nolint:paralleltest // The file size limit applies to the whole process.
func TestWriteSessionReportsWriteFailure(t *testing.T) {
	_, root := testSessionRoot(t)

	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit))
	t.Cleanup(func() { require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit)) })

	small := limit
	small.Cur = 1

	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &small))
	require.Error(t, WriteSession(root, validStatus()))
}

func TestReadSessionRejectsUnusableStatus(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)
	require.NoError(t, WriteSession(root, validStatus()))

	path := filepath.Join(dir, "status", "codex-sample.json")

	require.NoError(t, os.Chmod(path, 0))

	_, err := ReadSession(root, sessionVendor, sessionAlias)
	require.Error(t, err)

	require.NoError(t, os.Chmod(path, sessionFileMode))
	require.NoError(t, os.WriteFile(path, []byte("invalid"), sessionFileMode))

	_, err = ReadSession(root, sessionVendor, sessionAlias)
	require.Error(t, err)

	vendorMismatch, aliasMismatch := validStatus(), validStatus()
	vendorMismatch.Vendor, aliasMismatch.Alias = claudeVendor, "other"

	for _, mismatch := range []SessionStatus{vendorMismatch, aliasMismatch} {
		data, marshalErr := json.Marshal(mismatch)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(path, data, sessionFileMode))

		_, err = ReadSession(root, sessionVendor, sessionAlias)
		require.Error(t, err)
	}
}

func swapHooks(t *testing.T) {
	t.Helper()

	origLstat, origStat, origRename, origFstat, origSync := lstat, stat, rename, fstat, syncDir

	t.Cleanup(func() { lstat, stat, rename, fstat, syncDir = origLstat, origStat, origRename, origFstat, origSync })
}

func lstatFailing(target string) func(*os.Root, string) (fs.FileInfo, error) {
	passthrough := lstat

	return func(root *os.Root, name string) (fs.FileInfo, error) {
		if name == target {
			return nil, fs.ErrPermission
		}

		return passthrough(root, name)
	}
}

func otherDirInfo(t *testing.T) fs.FileInfo {
	t.Helper()

	info, err := os.Stat(canonicalTempDir(t))
	require.NoError(t, err)

	return info
}

//nolint:paralleltest // Swapped hooks are package variables.
func TestStatusRootRejectsChangedDirectory(t *testing.T) {
	_, root := testSessionRoot(t)
	swapHooks(t)

	lstat = lstatFailing("status")
	_, err := statusRoot(root)
	require.ErrorIs(t, err, errSessionState)

	lstat = (*os.Root).Lstat
	other := otherDirInfo(t)
	stat = func(*os.Root, string) (fs.FileInfo, error) { return other, nil }
	_, err = statusRoot(root)
	require.ErrorIs(t, err, errSessionState)
}

//nolint:paralleltest // Parallel tests would race on the hooks.
func TestWriteSessionRejectsStatusFileChanges(t *testing.T) {
	dir, root := testSessionRoot(t)
	swapHooks(t)

	lstat = lstatFailing("codex-sample.json")

	require.ErrorIs(t, WriteSession(root, validStatus()), errSessionState)

	lstat = (*os.Root).Lstat
	rename = func(*os.Root, string, string) error { return fs.ErrPermission }

	require.ErrorIs(t, WriteSession(root, validStatus()), errSessionState)

	entries, err := os.ReadDir(filepath.Join(dir, "status"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

//nolint:paralleltest // Swapped hooks are package variables.
func TestWriteSessionSyncsStatusDirectory(t *testing.T) {
	_, root := testSessionRoot(t)
	swapHooks(t)

	synced := 0
	passthrough := syncDir
	syncDir = func(dir *os.Root) error {
		synced++

		return passthrough(dir)
	}

	require.NoError(t, WriteSession(root, validStatus()))
	require.Equal(t, 1, synced)

	syncDir = func(*os.Root) error { return fs.ErrPermission }

	require.ErrorIs(t, WriteSession(root, validStatus()), errSessionState)
}

func TestSyncRootReportsOpenFailure(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(canonicalTempDir(t))
	require.NoError(t, err)
	require.NoError(t, syncRoot(root))
	require.NoError(t, root.Close())
	require.Error(t, syncRoot(root))
}

//nolint:paralleltest // Every test shares the hook variables.
func TestReadSessionRejectsStatusFileChanges(t *testing.T) {
	_, root := testSessionRoot(t)
	require.NoError(t, WriteSession(root, validStatus()))
	swapHooks(t)

	lstat = lstatFailing("codex-sample.json")
	_, err := ReadSession(root, sessionVendor, sessionAlias)
	require.ErrorIs(t, err, errSessionState)

	lstat = (*os.Root).Lstat
	fstat = func(*os.File) (fs.FileInfo, error) { return nil, fs.ErrPermission }
	_, err = ReadSession(root, sessionVendor, sessionAlias)
	require.ErrorIs(t, err, errSessionState)

	other := otherDirInfo(t)
	fstat = func(*os.File) (fs.FileInfo, error) { return other, nil }
	_, err = ReadSession(root, sessionVendor, sessionAlias)
	require.ErrorIs(t, err, errSessionState)
}

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

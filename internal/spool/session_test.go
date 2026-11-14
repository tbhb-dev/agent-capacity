// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool //nolint:testpackage // Exercise the rooted status and metadata boundaries.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	_, err = SessionRoot(".")
	require.Error(t, err)
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	_, err = SessionRoot(link)
	require.Error(t, err)
}

func TestSessionAliases(t *testing.T) {
	t.Parallel()
	_, root := testSessionRoot(t)

	status := validStatus()
	for _, account := range []string{"../outside", "bad/name", "", "Upper"} {
		status.Alias = account
		require.Error(t, WriteSession(root, status))
		_, err := ReadSession(root, claudeVendor, account)
		require.Error(t, err)

		stopped, err := Stopped(root, account)
		require.Error(t, err)
		require.False(t, stopped)

		_, err = SnapshotTime(root, claudeVendor, account)
		require.Error(t, err)
	}
}

func TestSessionRejectsUnknownVendors(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)

	for _, vendor := range []string{"gemini", "", "Claude"} {
		status := validStatus()
		status.Vendor = vendor
		require.ErrorIs(t, WriteSession(root, status), errSessionState)
		_, err := ReadSession(root, vendor, sessionAlias)
		require.ErrorIs(t, err, errSessionState)
		_, err = SnapshotTime(root, vendor, sessionAlias)
		require.ErrorIs(t, err, errSessionState)
	}

	require.NoDirExists(t, filepath.Join(dir, "status"))
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

	stopped, err := Stopped(root, sessionAlias)
	require.Error(t, err)
	require.False(t, stopped)
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

	status = validStatus()
	status.Phase = ""

	require.ErrorIs(t, WriteSession(root, status), errSessionState)
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

	linked := validStatus()
	linked.Alias = "linked"

	require.NoError(t, os.Symlink("codex-sample.json", filepath.Join(dir, "status", "codex-linked.json")))
	require.Error(t, WriteSession(root, linked))

	linkInfo, err := os.Lstat(filepath.Join(dir, "status", "codex-linked.json"))
	require.NoError(t, err)
	require.NotZero(t, linkInfo.Mode()&os.ModeSymlink)

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

	for _, partial := range []string{
		`{"time":"2026-01-02T03:04:05Z","vendor":"codex","alias":"sample"}`,
		`{"time":"2026-01-02T03:04:05Z","vendor":"codex","alias":"sample","phase":null}`,
	} {
		require.NoError(t, os.WriteFile(path, []byte(partial), sessionFileMode))

		_, err = ReadSession(root, sessionVendor, sessionAlias)
		require.ErrorIs(t, err, errSessionState)
	}

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

//nolint:paralleltest // Hooks are package variables.
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

//nolint:paralleltest // Replacing the stat hook mutates package state.
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
	require.Equal(t, 2, synced)

	synced = 0

	require.NoError(t, WriteSession(root, validStatus()))
	require.Equal(t, 2, synced)

	syncDir = func(*os.Root) error { return fs.ErrPermission }

	require.ErrorIs(t, WriteSession(root, validStatus()), errSessionState)
	_, err := root.Lstat("status")
	require.NoError(t, err)

	syncDir = func(dir *os.Root) error {
		if dir == root {
			return nil
		}

		return fs.ErrPermission
	}

	require.ErrorIs(t, WriteSession(root, validStatus()), errSessionState)
}

func TestSyncRootReportsOpenFailure(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(canonicalTempDir(t))
	require.NoError(t, err)
	require.NoError(t, syncRoot(root))
	require.NoError(t, root.Close())
	require.ErrorIs(t, syncRoot(root), errSessionState)
}

//nolint:paralleltest // All tests share the hook variables.
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

type foreignInfo struct {
	fs.FileInfo

	sys any
}

func (i foreignInfo) Sys() any { return i.sys }

type modeCase struct {
	mode    os.FileMode
	private bool
}

func modeCases() []modeCase {
	return []modeCase{
		{0o600, true},
		{0o644, true},
		{0o640, true},
		{0o641, true},
		{0o666, false},
		{0o660, false},
		{0o606, false},
		{0o620, false},
		{0o602, false},
	}
}

func TestPrivateEntryChecksOwnerAndWriteBits(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "entry")
	require.NoError(t, os.WriteFile(file, nil, sessionFileMode))

	info, err := os.Stat(file)
	require.NoError(t, err)
	require.True(t, privateEntry(info))

	for _, test := range modeCases() {
		require.NoError(t, os.Chmod(file, test.mode))

		info, err = os.Stat(file)
		require.NoError(t, err)
		require.Equal(t, test.private, privateEntry(info), test.mode.String())
	}

	require.NoError(t, os.Chmod(file, sessionFileMode))

	info, err = os.Stat(file)
	require.NoError(t, err)

	var other syscall.Stat_t

	owner, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)

	other = *owner
	other.Uid++

	require.False(t, privateEntry(foreignInfo{info, &other}))
	require.False(t, privateEntry(foreignInfo{info, nil}))
}

func writeRawStatus(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()

	status := filepath.Join(dir, "status")
	require.NoError(t, os.MkdirAll(status, sessionDirectoryMode))

	path := filepath.Join(status, "codex-sample.json")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))

	return path
}

func statusJSON(t *testing.T, restarts int, extra string) string {
	t.Helper()

	status := validStatus()
	for range restarts {
		status.Restarts = append(status.Restarts, time.Now().UTC())
	}

	data, err := json.Marshal(status)
	require.NoError(t, err)

	return strings.TrimSuffix(string(data), "}") + extra + "}"
}

func TestReadSessionDecodesStrictlyAndBounded(t *testing.T) {
	t.Parallel()

	valid := statusJSON(t, 1, "")
	exact := valid + strings.Repeat(" ", 8192-len(valid))

	for name, test := range map[string]struct {
		content string
		valid   bool
	}{
		"plain":                  {valid, true},
		"exactly the bound":      {exact, true},
		"one byte over":          {exact + " ", false},
		"oversized":              {valid + strings.Repeat(" ", 2*maxStatusBytes), false},
		"trailing value":         {valid + " {}", false},
		"trailing garbage":       {valid + "x", false},
		"duplicate key":          {statusJSON(t, 0, `,"phase":"stopped"`), false},
		"case variant key":       {statusJSON(t, 0, `,"PHASE":"stopped"`), false},
		"duplicate nested key":   {statusJSON(t, 0, `,"x":{"a":1,"a":2}`), false},
		"duplicate in array":     {statusJSON(t, 0, `,"x":[1,{"a":1,"a":2}]`), false},
		"distinct nested keys":   {statusJSON(t, 0, `,"x":[1,{"a":1,"b":2}]`), true},
		"unterminated":           {strings.TrimSuffix(valid, "}"), false},
		"unterminated nested":    {statusJSON(t, 0, `,"x":[1`), false},
		"not an object":          {"1", false},
		"non-string key":         {"{1:2}", false},
		"at the history limit":   {statusJSON(t, 20, ""), true},
		"over the history limit": {statusJSON(t, 21, ""), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, root := testSessionRoot(t)
			path := writeRawStatus(t, dir, test.content, sessionFileMode)

			_, err := ReadSession(root, sessionVendor, sessionAlias)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errSessionState)
			}

			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, test.content, string(after))
		})
	}
}

func TestReadSessionRejectsPipeWithoutBlocking(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)
	status := filepath.Join(dir, "status")
	require.NoError(t, os.Mkdir(status, sessionDirectoryMode))
	require.NoError(t, syscall.Mkfifo(filepath.Join(status, "codex-sample.json"), sessionFileMode))

	result := make(chan error, 1)

	go func() {
		_, err := ReadSession(root, sessionVendor, sessionAlias)
		result <- err
	}()

	select {
	case err := <-result:
		require.ErrorIs(t, err, errSessionState)
	case <-time.After(5 * time.Second):
		require.Fail(t, "read blocked on a pipe")
	}
}

func TestReadBoundedRejectsUnusableFiles(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain"), []byte("abc"), sessionFileMode))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "directory"), sessionDirectoryMode))
	require.NoError(t, os.Symlink("plain", filepath.Join(dir, "link")))

	data, err := ReadBounded(root, "plain", 3)
	require.NoError(t, err)
	require.Equal(t, "abc", string(data))

	_, err = ReadBounded(root, "plain", 2)
	require.ErrorIs(t, err, errSessionState)

	_, err = ReadBounded(root, "missing", 3)
	require.ErrorIs(t, err, fs.ErrNotExist)

	for _, name := range []string{"directory", "link"} {
		_, err = ReadBounded(root, name, 3)
		require.ErrorIs(t, err, errSessionState, name)
	}
}

func TestReadSessionRejectsWritableStatusEntries(t *testing.T) {
	t.Parallel()

	for _, test := range modeCases() {
		t.Run(test.mode.String(), func(t *testing.T) {
			t.Parallel()
			dir, root := testSessionRoot(t)
			writeRawStatus(t, dir, statusJSON(t, 0, ""), test.mode)

			_, err := ReadSession(root, sessionVendor, sessionAlias)
			if test.private {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errSessionState)
			}
		})
	}

	for _, test := range modeCases() {
		t.Run("directory "+test.mode.String(), func(t *testing.T) {
			t.Parallel()
			dir, root := testSessionRoot(t)
			writeRawStatus(t, dir, statusJSON(t, 0, ""), sessionFileMode)
			require.NoError(t, os.Chmod(filepath.Join(dir, "status"), test.mode|0o100))

			_, err := ReadSession(root, sessionVendor, sessionAlias)
			if test.private {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errSessionState)
			}
		})
	}
}

func TestReadSessionRejectsIdentityOnlyState(t *testing.T) {
	t.Parallel()
	dir, root := testSessionRoot(t)

	_, err := ReadSession(root, claudeVendor, sessionAlias)
	require.NoError(t, err)
	require.NoError(t, WriteSession(root, validStatus()))

	path := filepath.Join(dir, "status", claudeVendor+"-"+sessionAlias+".json")
	identity := []byte(`{"vendor":"` + claudeVendor + `","alias":"` + sessionAlias + `"}`)
	require.NoError(t, os.WriteFile(path, identity, 0o600))

	_, err = ReadSession(root, claudeVendor, sessionAlias)
	require.ErrorIs(t, err, errSessionState)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, identity, data)
}

func TestFoldKeyMatchesEncodingJSON(t *testing.T) {
	t.Parallel()

	longS := strings.Replace("restarts", "s", "\u017f", 1)
	kelvin := strings.Replace("kelvin", "k", "\u212a", 1)
	pairs := [][2]string{{"restarts", "RESTARTS"}, {"restarts", longS}, {"kelvin", kelvin}}

	for _, pair := range pairs {
		if FoldKey(pair[0]) != FoldKey(pair[1]) {
			t.Fatal("keys that encoding/json matches fold apart", pair)
		}
	}

	if FoldKey("phase") == FoldKey("alias") {
		t.Fatal("different keys fold together")
	}
}

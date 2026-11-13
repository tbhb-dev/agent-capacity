// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool //nolint:testpackage // Exercise the rooted status and metadata boundaries.

import (
	"os"
	"path/filepath"
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

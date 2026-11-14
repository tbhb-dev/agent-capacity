// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool //nolint:testpackage // Exercise the rooted status boundaries with swapped hooks.

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// Mode bits depend on the process file mask, so this test clears it first.
//
//nolint:paralleltest // Umask changes affect the whole process.
func TestWriteSessionCreatesPrivateDirectory(t *testing.T) {
	dir, root := testSessionRoot(t)
	previous := syscall.Umask(0)

	t.Cleanup(func() { syscall.Umask(previous) })

	require.NoError(t, WriteSession(root, validStatus()))

	info, err := os.Stat(filepath.Join(dir, "status"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// Directory info that isn't a directory must fail the status root check.
//
//nolint:paralleltest // Status calls get faked through package variables.
func TestStatusRootRejectsNonDirectoryInfo(t *testing.T) {
	dir, root := testSessionRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "status"), sessionDirectoryMode))

	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, sessionFileMode))

	info, err := os.Stat(file)
	require.NoError(t, err)
	swapHooks(t)

	lstat = func(*os.Root, string) (fs.FileInfo, error) { return info, nil }
	stat = func(*os.Root, string) (fs.FileInfo, error) { return info, nil }
	_, err = statusRoot(root)
	require.ErrorIs(t, err, errSessionState)
}

// Handle leaks would exhaust the descriptor limit that this test lowers.
//
//nolint:paralleltest // Descriptor limits and collector settings affect the whole process.
func TestSyncRootReleasesDirectoryHandles(t *testing.T) {
	root, err := os.OpenRoot(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	probe, err := os.Open(os.DevNull)
	require.NoError(t, err)

	free := probe.Fd()
	require.NoError(t, probe.Close())

	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit))
	t.Cleanup(func() { require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit)) })

	collector := debug.SetGCPercent(-1)

	t.Cleanup(func() { debug.SetGCPercent(collector) })

	small := limit
	small.Cur = uint64(free) + 4

	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &small))

	for range 16 {
		require.NoError(t, syncRoot(root))
	}
}

// Regular files only: a swapped Stat result must fail the read.
//
//nolint:paralleltest // Faked file info swaps package hooks.
func TestReadSessionRequiresRegularStatusFile(t *testing.T) {
	_, root := testSessionRoot(t)
	require.NoError(t, WriteSession(root, validStatus()))
	swapHooks(t)

	other := otherDirInfo(t)
	passthrough := lstat
	lstat = func(root *os.Root, name string) (fs.FileInfo, error) {
		if name == "codex-sample.json" {
			return other, nil
		}

		return passthrough(root, name)
	}
	fstat = func(*os.File) (fs.FileInfo, error) { return other, nil }
	_, err := ReadSession(root, sessionVendor, sessionAlias)
	require.ErrorIs(t, err, errSessionState)
}

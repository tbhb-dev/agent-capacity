//go:build linux || darwin

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//nolint:testpackage // The FIFO fixture exercises the internal read guard.
package keeper

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadRejectsFIFO(t *testing.T) {
	t.Parallel()

	dir := spoolDir(t)

	err := syscall.Mkfifo(filepath.Join(dir, fixtureStatus), privateFileMode)
	if errors.Is(err, syscall.ENOSYS) {
		t.Skip("The platform lacks FIFO support.")
	}

	require.NoError(t, err)

	root, err := openSpool(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	result := make(chan error, 1)

	go func() { result <- read(root, fixtureStatus, new(Status)) }()

	select {
	case readErr := <-result:
		require.ErrorIs(t, readErr, ErrStateInvalid)
	case <-time.After(time.Second):
		t.Fatal("read blocked on a FIFO")
	}
}

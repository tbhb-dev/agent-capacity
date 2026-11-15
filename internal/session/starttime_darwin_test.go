// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session //nolint:testpackage // Tests read the private start time of a process.

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

func TestStartKeyIsStableForALiveProcessAndEmptyForAMissingOne(t *testing.T) {
	t.Parallel()

	first := startKey(os.Getpid())
	if first == "" || first != startKey(os.Getpid()) {
		t.Fatal("unstable or empty start time", first)
	}

	_, parent, err := stamp(os.Getpid())
	if err != nil || parent != os.Getppid() {
		t.Fatal("wrong parent", parent, err)
	}

	started, _, err := stamp(2000000000)
	if started != "" || err != nil {
		t.Fatal("a missing process has a start time or an error", started, err)
	}
}

func TestEndedOnDarwinMapsOnlyTheEmptyAnswer(t *testing.T) {
	t.Parallel()

	if !errors.Is(endedOnDarwin(syscall.EIO), fs.ErrNotExist) {
		t.Fatal("the empty answer is not an ended process")
	}

	if !errors.Is(endedOnDarwin(syscall.EMFILE), syscall.EMFILE) {
		t.Fatal("another error was mapped")
	}
}

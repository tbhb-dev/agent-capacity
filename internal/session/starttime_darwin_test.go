// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session //nolint:testpackage // Tests read the private start time of a process.

import (
	"os"
	"testing"
)

func TestStartKeyIsStableForALiveProcessAndEmptyForAMissingOne(t *testing.T) {
	t.Parallel()

	first := startKey(os.Getpid())
	if first == "" || first != startKey(os.Getpid()) {
		t.Fatal("unstable or empty start time", first)
	}

	if startKey(2000000000) != "" {
		t.Fatal("a missing process has a start time")
	}
}

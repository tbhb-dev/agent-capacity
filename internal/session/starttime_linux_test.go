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

func TestParseStartTicksCountsFromTheLastParenthesis(t *testing.T) {
	t.Parallel()

	stat := "12 (a b) c) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22"
	if got := parseStartTicks(stat); got != "19" {
		t.Fatal(got)
	}

	if parseStartTicks("12 (short) S 1 2") != "" {
		t.Fatal("a short stat file gave a start time")
	}
}

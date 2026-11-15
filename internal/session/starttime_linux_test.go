// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session //nolint:testpackage // Tests read the private start time of a process.

import (
	"errors"
	"os"
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

func TestParseStatCountsFromTheLastParenthesis(t *testing.T) {
	t.Parallel()

	stat := "12 (a b) c) S 7 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22"
	started, parent, err := parseStat(stat)

	if started != "19" || parent != 7 || err != nil {
		t.Fatal(started, parent, err)
	}

	badParent := "12 (x) S p 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22"

	for _, bad := range []string{"12 (short) S 1 2", badParent} {
		_, _, badErr := parseStat(bad)
		if !errors.Is(badErr, errBadStat) {
			t.Fatal("a bad stat file was accepted", bad)
		}
	}
}

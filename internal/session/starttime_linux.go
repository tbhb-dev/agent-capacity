// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// The positions of the parent and the start time among the fields that follow the command name in a stat file.
const (
	parentField = 1
	startField  = 19
)

// errBadStat marks a stat file that doesn't have the expected fields.
var errBadStat = errors.New("unreadable stat file")

// stamp returns the start time of a process in clock ticks since boot with its parent, read together. The start
// time is empty and the error nil when the process has ended. Any other error leaves the process unknown.
func stamp(pid int) (string, int, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, unlessGone(err)
	}

	return parseStat(string(data))
}

// parseStat reads the start time and the parent from a stat file. The command name is in parentheses and may hold
// spaces. Counting starts at the last closing parenthesis.
func parseStat(stat string) (string, int, error) {
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	if len(fields) <= startField {
		return "", 0, errBadStat
	}

	parent, err := strconv.Atoi(fields[parentField])
	if err != nil {
		return "", 0, errBadStat
	}

	return fields[startField], parent, nil
}

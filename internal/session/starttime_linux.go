// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"os"
	"strconv"
	"strings"
)

// startField is the position of the start time among the fields that follow the command name in a stat file.
const startField = 19

// startKey returns the start time of a process in clock ticks since boot, or an empty string when it has ended.
func startKey(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}

	return parseStartTicks(string(data))
}

// parseStartTicks reads the start time from a stat file. The command name is in parentheses and may hold spaces.
// Counting starts at the last closing parenthesis.
func parseStartTicks(stat string) string {
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	if len(fields) <= startField {
		return ""
	}

	return fields[startField]
}

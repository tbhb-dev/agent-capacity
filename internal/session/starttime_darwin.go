// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// startKey returns the start time of a process to the microsecond, or an empty string when it has ended.
func startKey(pid int) string {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ""
	}

	return fmt.Sprintf("%d.%06d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec)
}

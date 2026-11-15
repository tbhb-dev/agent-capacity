// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// stamp returns the start time of a process to the microsecond with its parent, read together. The start time is
// empty when the process has ended.
func stamp(pid int) (string, int) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", 0
	}

	return fmt.Sprintf("%d.%06d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), int(info.Eproc.Ppid)
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sys/unix"
)

// stamp returns the start time of a process to the microsecond with its parent, read together. The start time is
// empty and the error nil when the process has ended. Any other error leaves the process unknown.
func stamp(pid int) (string, int, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", 0, unlessGone(endedOnDarwin(err))
	}

	return fmt.Sprintf("%d.%06d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), int(info.Eproc.Ppid), nil
}

// endedOnDarwin maps the error that the kernel gives for a process that has ended, which is an I/O error from an
// empty answer, to the not-exist error.
func endedOnDarwin(err error) error {
	if errors.Is(err, unix.EIO) {
		return fs.ErrNotExist
	}

	return err
}

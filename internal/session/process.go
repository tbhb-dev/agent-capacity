// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	loginWarning      = "Your login expires in 3 days"
	terminalDrainWait = 100 * time.Millisecond
	terminationGrace  = 2 * time.Second
)

// exited runs after a process result is queued on done. Tests replace it to wait for that point.
//
//nolint:gochecknoglobals // A package-level hook is the only way to reach a process the runner starts.
var exited = func(int) {}

type warningDrain struct {
	flag *atomic.Bool
	tail string
}

func (w *warningDrain) Write(data []byte) (int, error) {
	text := w.tail + string(data)
	if strings.Contains(text, loginWarning) {
		w.flag.Store(true)
	}

	if len(text) >= len(loginWarning) {
		text = text[len(text)-len(loginWarning)+1:]
	}

	w.tail = text

	return len(data), nil
}

type process struct {
	cmd      *exec.Cmd
	done     chan error
	terminal *os.File
	drained  chan struct{}
}

func start(cmd *exec.Cmd, terminal bool, warning *atomic.Bool) (*process, error) {
	child := &process{cmd: cmd, done: make(chan error, 1), terminal: nil, drained: make(chan struct{})}
	if terminal {
		file, err := pty.Start(cmd)
		if err != nil {
			return nil, ErrArguments
		}

		child.terminal = file
		go func() {
			//nolint:errcheck // Closing the terminal ends draining without retaining errors.
			_, _ = io.Copy(&warningDrain{flag: warning, tail: ""}, file)

			close(child.drained)
		}()
	} else {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
		cmd.SysProcAttr.Setpgid = true

		// Output goes to the null device, so Wait can't block on a descendant's open pipe. The group signal
		// below then follows the reap at once.
		if cmd.Start() != nil {
			return nil, ErrArguments
		}

		close(child.drained)
	}

	go func() {
		err := cmd.Wait()

		if child.terminal == nil {
			child.signalGroup(syscall.SIGKILL)
		}

		child.done <- err

		exited(cmd.Process.Pid)
	}()

	return child, nil
}

// signalGroup signals the process group of the leader. It also runs after the leader exits and cmd.Wait
// reaps it, because children left behind by a dead leader still need the signal. In that window the kernel
// could reuse the process ID for an unrelated group. The waiter signals right after the reap. The window lasts
// microseconds, and macOS offers Go no wait-without-reap call to close it. The kills by process ID in the stop
// path share that small risk. The window is accepted since skipping the signal would leave those orphaned
// children running.
func (p *process) signalGroup(signal syscall.Signal) {
	//nolint:errcheck // The process group may already have exited.
	_ = syscall.Kill(-p.cmd.Process.Pid, signal)
}

func (p *process) closeTerminal() {
	if p.terminal == nil {
		return
	}

	p.signalGroup(syscall.SIGKILL)

	select {
	case <-p.drained:
	case <-time.After(terminalDrainWait):
	}

	_ = p.terminal.Close()

	// Closing the master normally ends the reader at once. The bound keeps shutdown finite if it doesn't.
	select {
	case <-p.drained:
	case <-time.After(terminationGrace):
	}
}

// descendants lists the processes below a process ID, found from the parent links that ps reports. A process in
// a new session has left the group but keeps its parent link until the parent ends.
func descendants(pid int) []int {
	//nolint:errcheck,noctx // A failed listing leaves no output. The listing is short and needs no deadline.
	out, _ := exec.Command("/bin/ps", "-A", "-o", "pid=,ppid=").Output()

	children := map[int][]int{}

	for line := range strings.SplitSeq(string(out), "\n") {
		var child, parent int

		_, scanErr := fmt.Sscan(line, &child, &parent)
		if scanErr == nil {
			children[parent] = append(children[parent], child)
		}
	}

	var found []int

	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		found = append(found, children[queue[0]]...)
		queue = append(queue, children[queue[0]]...)
	}

	return found
}

func killAll(ids []int) {
	for _, pid := range ids {
		//nolint:errcheck // The process may already have exited.
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (p *process) terminate() {
	if p == nil {
		return
	}

	below := descendants(p.cmd.Process.Pid)

	p.signalGroup(syscall.SIGTERM)

	timer := time.NewTimer(terminationGrace)
	defer timer.Stop()

	select {
	case <-p.done: //nolint:errcheck // Shutdown only needs process completion.
	case <-timer.C:
		p.signalGroup(syscall.SIGKILL)
		<-p.done //nolint:errcheck // Shutdown only needs process completion.
	}

	// gomutants:disable-next-line STATEMENT_REMOVE reason="Earlier steps already end the group."
	p.signalGroup(syscall.SIGKILL)

	killAll(below)
	p.closeTerminal()
}

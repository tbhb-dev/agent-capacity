// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	loginWarning      = "Your login expires in 3 days"
	terminalDrainWait = 100 * time.Millisecond
	terminationGrace  = 2 * time.Second
	// sweepInterval is how often a running child's descendants are listed, so that a descendant that left the
	// process group is known when the child ends on its own.
	sweepInterval = 500 * time.Millisecond
)

// exited runs after a process result is queued on done. Tests replace it to wait for that point.
//
//nolint:gochecknoglobals // A package-level hook is the only way to reach a process the runner starts.
var exited = func(int) {}

// Tests replace the signal function to see which signals a stop sends.
//
//nolint:gochecknoglobals // A package-level hook is the only way to observe signals sent to real processes.
var sendSignal = syscall.Kill

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
	// ended closes when cmd.Wait returns, and swept closes once the periodic listing has stopped.
	ended, swept chan struct{}
	members      []member
	mutex        sync.Mutex
	// reaped is set once cmd.Wait has reaped the leader and the waiter has sent its last group signal. From then
	// on the process ID may belong to an unrelated process, so nothing signals it again.
	reaped atomic.Bool
}

// member is a descendant with the start time recorded at listing. A process ID that later shows another start
// time belongs to another process.
type member struct {
	started string
	pid     int
}

func start(cmd *exec.Cmd, terminal bool, warning *atomic.Bool) (*process, error) {
	child := &process{
		cmd: cmd, done: make(chan error, 1), terminal: nil, drained: make(chan struct{}),
		ended: make(chan struct{}), swept: make(chan struct{}), members: nil,
		mutex: sync.Mutex{}, reaped: atomic.Bool{},
	}
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
		defer close(child.swept)

		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				child.refresh()
			case <-child.ended:
				return
			}
		}
	}()

	go func() {
		err := cmd.Wait()

		close(child.ended)
		<-child.swept

		// Descendants that left the group are stopped from the list kept while the child lived, each after a
		// check of its start time.
		child.signalGroup(syscall.SIGKILL)
		child.sweep()
		child.reaped.Store(true)

		child.done <- err

		exited(cmd.Process.Pid)
	}()

	return child, nil
}

// signalGroup signals the process group of the leader. The waiter sends one last SIGKILL right after cmd.Wait
// reaps the leader, which stops children a dead leader left behind. That signal runs in a window of
// microseconds in which the kernel could already have reused the process ID, and macOS offers Go no
// wait-without-reap call to close it. Every later call returns without signaling, because by then the
// process ID may belong to an unrelated group. The flag check and the signal are two steps, so a leader that
// exits between them leaves the same window of microseconds.
func (p *process) signalGroup(signal syscall.Signal) {
	if p.reaped.Load() {
		return
	}

	//nolint:errcheck // The process group may already have exited.
	_ = sendSignal(-p.cmd.Process.Pid, signal)
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

// descendants lists the processes below a process ID, found from the parent links that ps reports, each with its
// start time. A process in a new session has left the group but keeps its parent link until the parent ends.
func descendants(pid int) []member {
	//nolint:errcheck,noctx // A failed listing leaves no output. The listing is short and needs no deadline.
	out, _ := exec.Command("/bin/ps", "-A", "-o", "pid=,ppid=,lstart=").Output()

	children := parseProcesses(string(out))

	var found []member

	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			found = append(found, child)
			queue = append(queue, child.pid)
		}
	}

	return found
}

// psFields is the fewest fields in a listing line: the ID, the parent ID and one word of the start time.
const psFields = 3

// parseProcesses groups the members of a listing by parent ID and skips any line that doesn't parse.
func parseProcesses(listing string) map[int][]member {
	children := map[int][]member{}

	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < psFields {
			continue
		}

		child, childErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])

		if childErr != nil || parentErr != nil {
			continue
		}

		children[parent] = append(children[parent], member{pid: child, started: strings.Join(fields[2:], " ")})
	}

	return children
}

// startTime returns the start time that ps reports for a process, or an empty string when it has ended.
func startTime(pid int) string {
	//nolint:errcheck,noctx,gosec // A failed listing means the process is gone. The ID is an integer.
	out, _ := exec.Command("/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()

	return strings.Join(strings.Fields(string(out)), " ")
}

// killAll stops each listed process that still shows its recorded start time. A process that has
// ended, or whose ID another process now holds, is skipped. The lookup and the signal are two steps, so a
// process that ends between them leaves a window of microseconds.
func killAll(listed []member, lookup func(int) string) {
	for _, entry := range listed {
		if started := lookup(entry.pid); started == "" || started != entry.started {
			continue
		}

		//nolint:errcheck // The process may already have exited.
		_ = sendSignal(entry.pid, syscall.SIGKILL)
	}
}

// refresh adds the current descendants to the list. It lists nothing once the child has been reaped, because the
// process ID may then belong to another process.
func (p *process) refresh() {
	if p.reaped.Load() {
		return
	}

	found := descendants(p.cmd.Process.Pid)

	p.mutex.Lock()
	defer p.mutex.Unlock()

	for _, entry := range found {
		if !slices.Contains(p.members, entry) {
			p.members = append(p.members, entry)
		}
	}
}

// sweep stops every listed descendant that still shows its recorded start time.
func (p *process) sweep() {
	p.mutex.Lock()
	listed := slices.Clone(p.members)
	p.mutex.Unlock()

	killAll(listed, startTime)
}

func (p *process) terminate() {
	if p == nil {
		return
	}

	// A leader reaped before the stop has nothing left to signal, and its process ID may be reused.
	if p.reaped.Load() {
		p.closeTerminal()

		return
	}

	p.refresh()
	p.signalGroup(syscall.SIGTERM)

	timer := time.NewTimer(terminationGrace)
	defer timer.Stop()

	select {
	case <-p.done: //nolint:errcheck // Shutdown only needs process completion.
	case <-timer.C:
		p.signalGroup(syscall.SIGKILL)
		<-p.done //nolint:errcheck // Shutdown only needs process completion.
	}

	// The waiter has already signaled the group and swept the listed descendants before it queued the result.
	p.closeTerminal()
}

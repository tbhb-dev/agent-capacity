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

// psTimeout bounds one process listing, so a stuck ps can't hold up the exit sweep.
const psTimeout = 5 * time.Second

// psFields is the fewest fields in a listing line: the ID, the parent ID and one word of the start time.
const psFields = 3

// snapshot is one process listing: the members by parent ID and the start time of each process ID.
type snapshot struct {
	children map[int][]member
	started  map[int]string
}

// takeSnapshot lists every process with one ps run. A failed or late listing yields an empty snapshot.
func takeSnapshot() snapshot {
	var out strings.Builder

	//nolint:noctx // A timer stops a stuck listing, which keeps the callers free of a context parameter.
	cmd := exec.Command("/bin/ps", "-A", "-o", "pid=,ppid=,lstart=")
	cmd.Stdout = &out

	// A failed start makes Wait return at once, before the timer can fire. The timer never meets a missing process.
	//nolint:errcheck // A failed listing leaves no output.
	_ = cmd.Start()

	stuck := time.AfterFunc(psTimeout, func() {
		//nolint:errcheck // The listing may have ended already.
		_ = cmd.Process.Kill()
	})

	//nolint:errcheck // A failed or stopped listing leaves partial output, which parses line by line.
	_ = cmd.Wait()

	stuck.Stop()

	return parseProcesses(out.String())
}

// parseProcesses reads a listing and skips any line that doesn't parse.
func parseProcesses(listing string) snapshot {
	snap := snapshot{children: map[int][]member{}, started: map[int]string{}}

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

		started := strings.Join(fields[2:], " ")
		snap.children[parent] = append(snap.children[parent], member{pid: child, started: started})
		snap.started[child] = started
	}

	return snap
}

// below lists the processes under a process ID, found from the parent links. A process in a new session has left
// the group but keeps its parent link until the parent ends.
func (s snapshot) below(pid int) []member {
	var found []member

	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		for _, child := range s.children[queue[0]] {
			found = append(found, child)
			queue = append(queue, child.pid)
		}
	}

	return found
}

// lookup returns the start time of a process ID, or an empty string when the listing doesn't show it.
func (s snapshot) lookup(pid int) string {
	return s.started[pid]
}

// descendants lists the processes below a process ID with their start times.
func descendants(pid int) []member {
	return takeSnapshot().below(pid)
}

// killAll stops each listed process that still shows its recorded start time. A process that has
// ended, or whose ID another process now holds, is skipped. The lookup comes from one listing, so a process that
// ends between that listing and its signal leaves a window of milliseconds for the later entries.
func killAll(listed []member, lookup func(int) string) {
	for _, entry := range listed {
		if started := lookup(entry.pid); started == "" || started != entry.started {
			continue
		}

		//nolint:errcheck // The process may already have exited.
		_ = sendSignal(entry.pid, syscall.SIGKILL)
	}
}

// refresh adds the current descendants to the list and drops each member that the listing no longer shows with
// its recorded start time. It lists nothing once the child has been reaped, because the process ID may then
// belong to another process.
func (p *process) refresh() {
	if p.reaped.Load() {
		return
	}

	snap := takeSnapshot()
	found := snap.below(p.cmd.Process.Pid)

	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.members = slices.DeleteFunc(p.members, func(entry member) bool { return snap.lookup(entry.pid) != entry.started })

	for _, entry := range found {
		if !slices.Contains(p.members, entry) {
			p.members = append(p.members, entry)
		}
	}
}

// sweep stops every listed descendant that still shows its recorded start time, from one listing.
func (p *process) sweep() {
	p.mutex.Lock()
	listed := slices.Clone(p.members)
	p.mutex.Unlock()

	killAll(listed, takeSnapshot().lookup)
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

// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"errors"
	"io"
	"io/fs"
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
	// The first listings of a new child come at the short delay below, until the window after it has passed.
	firstSweep  = 100 * time.Millisecond
	earlyWindow = 2 * time.Second
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
	// cleaned closes after the waiter has swept the recorded descendants.
	cleaned chan struct{}
	members []member
	mutex   sync.Mutex
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
		ended: make(chan struct{}), swept: make(chan struct{}), cleaned: make(chan struct{}), members: nil,
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

	go child.sweepWhileRunning()

	go func() {
		err := cmd.Wait()

		// One last group signal stops children the dead leader left behind. It runs right after the reap, in
		// the window that the group-signal method describes. From the flag on, nothing signals or lists by the
		// leader's ID.
		//nolint:errcheck // The process group may already have exited.
		_ = sendSignal(-cmd.Process.Pid, syscall.SIGKILL)

		// The flag is set before any lock is taken, so a stop that comes in while a listing holds the lock still
		// sees the reap. The sweep below takes the lock too, so it waits for a listing that began earlier.
		child.reaped.Store(true)

		close(child.ended)
		<-child.swept

		// Descendants that left the group are stopped from the list kept while the child lived, each after a
		// check of its start time. A member that couldn't be read turns a clean result into a failure.
		err = child.cleanedResult(err, stamp)
		close(child.cleaned)

		child.done <- err

		exited(cmd.Process.Pid)
	}()

	return child, nil
}

// signalGroup signals the process group of the leader and then the leader itself. The waiter sends one last
// SIGKILL right after cmd.Wait reaps the leader, which stops children a dead leader left behind. That signal runs
// in a window of microseconds in which the kernel could already have reused the process ID, and macOS offers Go
// no wait-without-reap call to close it. The waiter then sets the flag, before any cleanup, and every later call
// returns without signaling, because by then the process ID may belong to an unrelated group. The flag check and
// the signal are two steps, so a leader that exits between them leaves the same window of microseconds.
func (p *process) signalGroup(signal syscall.Signal) {
	if p.reaped.Load() {
		return
	}

	// A leader that moved to another group leaves its first group empty. The leader gets the signal itself too.
	//nolint:errcheck // The process group may already have exited.
	_ = sendSignal(-p.cmd.Process.Pid, signal)
	//nolint:errcheck // The leader may already have exited.
	_ = sendSignal(p.cmd.Process.Pid, signal)
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

// sweepWhileRunning lists the descendants until the leader ends. A new child gets frequent early listings, so a
// descendant that a quick child starts and leaves behind is seen within a tenth of a second.
func (p *process) sweepWhileRunning() {
	defer close(p.swept)

	began := time.Now()
	timer := time.NewTimer(firstSweep)

	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			p.refresh()
			timer.Reset(sweepDelay(time.Since(began)))
		case <-p.ended:
			return
		}
	}
}

// sweepDelay is the wait before the next listing of a child that has run for the given time.
func sweepDelay(running time.Duration) time.Duration {
	if running < earlyWindow {
		return firstSweep
	}

	return sweepInterval
}

// psTimeout bounds one process listing, so a stuck ps can't hold up the exit sweep.
// gomutants:disable-next-line INTEGER_DECREMENT,INTEGER_INCREMENT reason="One millisecond of a timeout is moot."
const psTimeout = 1500 * time.Millisecond

// psFields is the number of fields in a listing line: the ID and the parent ID.
const psFields = 2

// snapshot is one process listing: the process IDs by parent ID.
type snapshot struct {
	children map[int][]int
}

// The listing program is a variable so that tests can point it at stand-ins that fail.
//
//nolint:gochecknoglobals // A package-level hook is the only way to fail the listing from a test.
var psPath = "/bin/ps"

// takeSnapshot lists every process with one ps run. The second result is false when ps failed, was stopped by the
// timer, or gave an empty list, because the snapshot is then missing or partial and proves nothing about a process.
func takeSnapshot() (snapshot, bool) {
	var out strings.Builder

	//nolint:noctx // A timer stops a stuck listing, which keeps the callers free of a context parameter.
	cmd := exec.Command(psPath, "-A", "-o", "pid=,ppid=")
	cmd.Stdout = &out

	// A failed start makes Wait return at once, before the timer can fire. The timer never meets a missing process.
	//nolint:errcheck // A failed listing leaves no output.
	_ = cmd.Start()

	stuck := time.AfterFunc(psTimeout, func() {
		//nolint:errcheck // The listing may have ended already.
		_ = cmd.Process.Kill()
	})

	err := cmd.Wait()

	stuck.Stop()

	snap := parseProcesses(out.String())

	return snap, err == nil && len(snap.children) > 0
}

// parseProcesses reads a listing and skips any line that doesn't parse.
func parseProcesses(listing string) snapshot {
	snap := snapshot{children: map[int][]int{}}

	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) != psFields {
			continue
		}

		child, childErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])

		if childErr != nil || parentErr != nil {
			continue
		}

		snap.children[parent] = append(snap.children[parent], child)
	}

	return snap
}

// edge is a process ID with the parent that a listing showed for it.
type edge struct {
	pid    int
	parent int
}

// below lists the processes under a process ID, found from the parent links. A process in a new session has
// left the group but keeps its parent link until the parent ends.
func (s snapshot) below(pid int) []edge {
	var found []edge

	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		for _, child := range s.children[queue[0]] {
			found = append(found, edge{pid: child, parent: queue[0]})
			queue = append(queue, child)
		}
	}

	return found
}

// record pairs each process ID with its start time. The start time and the parent come from one read of the
// process. The result leaves out an ended process, a process that can't be read now, and one whose parent differs
// from the listing, since an ID reused after the listing belongs to another process with another parent. A
// process left out here appears again in a later listing.
func record(edges []edge, read func(int) (string, int, error)) []member {
	var found []member

	for _, entry := range edges {
		started, parent, err := read(entry.pid)
		if err == nil && started != "" && parent == entry.parent {
			found = append(found, member{pid: entry.pid, started: started})
		}
	}

	return found
}

// unlessGone turns the error of a read of an ended process into nil. Any other error, such as too many open
// files, leaves the process unknown.
func unlessGone(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

// refresh lists the processes and merges them into the members. A failed listing leaves the members as recorded.
func (p *process) refresh() {
	snap, ok := takeSnapshot()
	if !ok {
		return
	}

	p.absorb(snap, stamp)
}

// absorb drops each member that has ended or lost its ID to another process, then adds the descendants of the
// leader and of every member read as unchanged. A member whose parent ended has a new parent, so its own children are
// found through the member. A listing of a reaped child is dropped, because its process ID may belong to another
// process.
func (p *process) absorb(snap snapshot, read func(int) (string, int, error)) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// The flag is read under the mutex, so a listing that ran across the reap is dropped.
	if p.reaped.Load() {
		return
	}

	tracked := map[int]bool{}
	verified := map[int]bool{}

	// The listing is older than this check, so an ID dropped here stays out until the next listing. A member that
	// can't be read now stays, because a failed read says nothing about the process.
	p.members = slices.DeleteFunc(p.members, func(entry member) bool {
		started, _, err := read(entry.pid)
		gone := err == nil && started != entry.started

		if gone {
			tracked[entry.pid] = true
		}

		// gomutants:disable-next-line EXPRESSION_REMOVE reason="A member that is gone has left the list before the roots are built."
		verified[entry.pid] = err == nil && !gone

		return gone
	})

	roots := []int{p.cmd.Process.Pid}

	for _, entry := range p.members {
		tracked[entry.pid] = true

		// A member that can't be read may be an ID that a different process now uses, and that process's children
		// would be recorded as the leader's. Only a member read as unchanged seeds discovery.
		if verified[entry.pid] {
			roots = append(roots, entry.pid)
		}
	}

	for _, root := range roots {
		for _, entry := range record(snap.below(root), read) {
			if !tracked[entry.pid] {
				tracked[entry.pid] = true
				p.members = append(p.members, entry)
			}
		}
	}
}

// A sweep reads a member that couldn't be read this many times in all, with the wait below between the reads.
const (
	sweepAttempts = 5
	// gomutants:disable-next-line INTEGER_DECREMENT,INTEGER_INCREMENT reason="One millisecond of a wait is moot."
	sweepRetry = 50 * time.Millisecond
)

// stopListed stops each listed process that still shows its recorded start time and returns the ones that couldn't
// be read. A failed read says nothing about the process, so the process is neither signaled nor taken as ended.
func stopListed(listed []member, read func(int) (string, int, error)) []member {
	var unknown []member

	for _, entry := range listed {
		started, _, err := read(entry.pid)
		if err != nil {
			unknown = append(unknown, entry)

			continue
		}

		if started == "" || started != entry.started {
			continue
		}

		//nolint:errcheck // The process may already have exited.
		_ = sendSignal(entry.pid, syscall.SIGKILL)
	}

	return unknown
}

// sweep stops every listed descendant that still shows its recorded start time. It runs without a process listing.
func (p *process) sweep() {
	p.sweepWith(stamp)
}

// cleanedResult sweeps the recorded descendants and returns the child's result. A clean result becomes
// errCleanupIncomplete when a member couldn't be read, because that descendant may still run.
func (p *process) cleanedResult(result error, read func(int) (string, int, error)) error {
	if left := p.sweepWith(read); len(left) > 0 && result == nil {
		return errCleanupIncomplete
	}

	return result
}

// sweepWith reads again each member that couldn't be read, so a short failure such as too many open files doesn't
// leave a descendant running. It returns the members that were never readable, which are left alone because their
// IDs may belong to other processes. The same members remain recorded.
func (p *process) sweepWith(read func(int) (string, int, error)) []member {
	p.mutex.Lock()
	listed := slices.Clone(p.members)
	p.mutex.Unlock()

	for attempt := range sweepAttempts {
		// gomutants:disable-next-line CONDITIONALS_BOUNDARY,INTEGER_DECREMENT reason="A wait before the first read only delays it."
		if attempt > 0 {
			time.Sleep(sweepRetry)
		}

		listed = stopListed(listed, read)
		if len(listed) == 0 {
			break
		}
	}

	p.mutex.Lock()
	p.members = listed
	p.mutex.Unlock()

	return listed
}

func (p *process) terminate() {
	if p == nil {
		return
	}

	// A leader reaped before the stop has nothing left to signal, and its process ID may be reused. The waiter
	// may still be sweeping the recorded descendants, and the stop waits for that sweep.
	if p.reaped.Load() {
		<-p.cleaned
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

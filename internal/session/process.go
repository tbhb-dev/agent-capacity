// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
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

		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.WaitDelay = terminationGrace

		if cmd.Start() != nil {
			return nil, ErrArguments
		}

		close(child.drained)
	}

	go func() { child.done <- cmd.Wait() }()

	return child, nil
}

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
	<-p.drained
}

func (p *process) terminate() {
	if p == nil {
		return
	}

	p.signalGroup(syscall.SIGTERM)

	timer := time.NewTimer(terminationGrace)
	defer timer.Stop()

	select {
	case <-p.done: //nolint:errcheck // Shutdown only needs process completion.
	case <-timer.C:
		p.signalGroup(syscall.SIGKILL)
		<-p.done //nolint:errcheck // Shutdown only needs process completion.
	}

	p.signalGroup(syscall.SIGKILL)
	p.closeTerminal()
}

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package agentd

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// processTree isolates a Provider process and its descendants from agentd's
// process group. Killing the negative root PID then reaches every descendant
// that has not deliberately escaped the inherited process group.
type processTree struct {
	command *exec.Cmd

	mu         sync.Mutex
	released   bool
	terminated bool
}

func newProcessTree(command *exec.Cmd) (*processTree, error) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	return &processTree{command: command}, nil
}

func (p *processTree) started() error { return nil }

func (p *processTree) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released || p.terminated || p.command == nil || p.command.Process == nil {
		return nil
	}
	err := syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		p.terminated = true
		return nil
	}
	rootErr := p.command.Process.Kill()
	if rootErr == nil || errors.Is(rootErr, os.ErrProcessDone) {
		return err
	}
	return errors.Join(err, rootErr)
}

func (p *processTree) release() error {
	p.mu.Lock()
	p.released = true
	p.mu.Unlock()
	return nil
}

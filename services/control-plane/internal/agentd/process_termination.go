package agentd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

const resourceSuspendContainmentCapabilityKey = "resourceSuspendContainment"

type processTreeOptions struct {
	CgroupV2Root              string
	ProtectedProviderIdentity *ProtectedCgroupIdentity
	ContainmentFence          ProtectedCgroupFence
}

type containmentError struct {
	phase string
	err   error
}

func (e *containmentError) Error() string {
	if e == nil {
		return ""
	}
	message := "process containment failed"
	if e.phase != "" {
		message += " during " + e.phase
	}
	if e.err == nil {
		return message
	}
	return fmt.Sprintf("%s: %v", message, e.err)
}

func (e *containmentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newContainmentError(phase string, err error) error {
	if err == nil {
		return nil
	}
	var containment *containmentError
	if errors.As(err, &containment) {
		return err
	}
	return &containmentError{phase: phase, err: err}
}

func isContainmentError(err error) bool {
	var containment *containmentError
	return errors.As(err, &containment)
}

// processOutputPipes uses caller-owned OS pipes so exec.Cmd.Wait can observe
// the root process independently of descendants that inherited stdout or
// stderr. Once the root exits, processTree termination closes the inherited
// writers and lets the readers drain deterministically.
type processOutputPipes struct {
	stdoutRead  *os.File
	stdoutWrite *os.File
	stderrRead  *os.File
	stderrWrite *os.File
	stderr      io.Writer

	startOnce  sync.Once
	closeOnce  sync.Once
	stderrDone chan struct{}
}

func newProcessOutputPipes(command *exec.Cmd, stderr io.Writer) (*processOutputPipes, error) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, err
	}
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	return &processOutputPipes{
		stdoutRead: stdoutRead, stdoutWrite: stdoutWrite,
		stderrRead: stderrRead, stderrWrite: stderrWrite, stderr: stderr,
	}, nil
}

func (p *processOutputPipes) started() {
	p.startOnce.Do(func() {
		_ = p.stdoutWrite.Close()
		_ = p.stderrWrite.Close()
		p.stdoutWrite = nil
		p.stderrWrite = nil
		p.stderrDone = make(chan struct{})
		go func() {
			defer close(p.stderrDone)
			_, _ = io.Copy(p.stderr, p.stderrRead)
		}()
	})
}

func (p *processOutputPipes) waitStderr() {
	if p.stderrDone != nil {
		<-p.stderrDone
	}
}

func (p *processOutputPipes) close() {
	p.closeOnce.Do(func() {
		if p.stdoutRead != nil {
			_ = p.stdoutRead.Close()
		}
		if p.stdoutWrite != nil {
			_ = p.stdoutWrite.Close()
		}
		if p.stderrRead != nil {
			_ = p.stderrRead.Close()
		}
		if p.stderrWrite != nil {
			_ = p.stderrWrite.Close()
		}
	})
}

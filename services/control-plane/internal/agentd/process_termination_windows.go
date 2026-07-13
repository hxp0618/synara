//go:build windows

package agentd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree starts each Provider suspended, assigns it to a kill-on-close
// Job Object, and only then resumes it. This closes the child-spawn race and
// keeps all descendants owned by the Job after their direct parent exits.
type processTree struct {
	command *exec.Cmd
	job     windows.Handle

	mu         sync.Mutex
	released   bool
	terminated bool
}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

func newProcessTree(command *exec.Cmd) (*processTree, error) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	if err := ntResumeProcess.Find(); err != nil {
		return nil, fmt.Errorf("resolve suspended process resume API: %w", err)
	}
	command.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process Job Object: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure process Job Object: %w", err)
	}
	return &processTree{command: command, job: job}, nil
}

func (p *processTree) started() error {
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_SUSPEND_RESUME|windows.PROCESS_TERMINATE,
		false,
		uint32(p.command.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open process for Job assignment: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(p.job, process); err != nil {
		return fmt.Errorf("assign process to Job Object: %w", err)
	}
	status, _, _ := ntResumeProcess.Call(uintptr(process))
	if status != uintptr(windows.STATUS_SUCCESS) {
		return fmt.Errorf("resume Job-owned process: %w", windows.NTStatus(status))
	}
	return nil
}

func (p *processTree) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released || p.terminated || p.command == nil || p.command.Process == nil {
		return nil
	}
	jobErr := windows.TerminateJobObject(p.job, 1)
	rootErr := p.command.Process.Kill()
	if rootErr == nil || errors.Is(rootErr, os.ErrProcessDone) {
		rootErr = nil
	}
	if jobErr == nil {
		p.terminated = true
	}
	if jobErr != nil && rootErr != nil {
		return errors.Join(jobErr, rootErr)
	}
	if jobErr != nil {
		return jobErr
	}
	return rootErr
}

func (p *processTree) release() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return nil
	}
	p.released = true
	if p.job == 0 {
		return nil
	}
	err := windows.CloseHandle(p.job)
	p.job = 0
	return err
}

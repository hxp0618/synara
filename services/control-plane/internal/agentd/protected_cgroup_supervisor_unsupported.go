//go:build !linux

package agentd

import "errors"

type ProtectedCgroupSupervisor struct{}

func NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig) (*ProtectedCgroupSupervisor, error) {
	return nil, errors.New("protected cgroup supervisor is unsupported on this operating system")
}

func (*ProtectedCgroupSupervisor) Paths() ProtectedCgroupPaths { return ProtectedCgroupPaths{} }

func (*ProtectedCgroupSupervisor) Fence() ProtectedCgroupFence { return ProtectedCgroupFence{} }

func (*ProtectedCgroupSupervisor) AttachAgentdPID(ProtectedCgroupFence, int) error {
	return errors.New("protected cgroup supervisor is unsupported on this operating system")
}

func (*ProtectedCgroupSupervisor) AttachProviderPID(ProtectedCgroupFence, int) error {
	return errors.New("protected cgroup supervisor is unsupported on this operating system")
}

func (*ProtectedCgroupSupervisor) Cleanup(ProtectedCgroupFence) error {
	return errors.New("protected cgroup supervisor is unsupported on this operating system")
}

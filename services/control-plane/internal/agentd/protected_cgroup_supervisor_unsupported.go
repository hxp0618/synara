//go:build !linux

package agentd

import (
	"errors"

	"github.com/google/uuid"
)

type ProtectedCgroupSupervisor struct{}

type ProtectedCgroupRootLease struct{}

func acquireProtectedCgroupDaemonRootLease(Config, uuid.UUID) (*ProtectedCgroupRootLease, error) {
	return nil, nil
}

func AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig) (*ProtectedCgroupRootLease, error) {
	return nil, errors.New("protected cgroup root lease is unsupported on this operating system")
}

func (*ProtectedCgroupRootLease) RecoverOrphans() error {
	return errors.New("protected cgroup root lease is unsupported on this operating system")
}

func (*ProtectedCgroupRootLease) Close() error { return nil }

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

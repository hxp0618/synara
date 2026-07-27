package agentd

import (
	"context"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/cgroupv2limits"
)

// ProtectedCgroupIdentity identifies the OS principal that must own the
// protected cgroup subtree or that is expected to run the untrusted Provider.
type ProtectedCgroupIdentity struct {
	UID uint32
	GID uint32
}

// ProtectedCgroupResourceLimits are finite hard limits applied to every
// untrusted Provider cgroup before the Provider process can start.
type ProtectedCgroupResourceLimits = cgroupv2limits.Limits

func validateProtectedCgroupResourceLimits(limits ProtectedCgroupResourceLimits) error {
	return cgroupv2limits.Validate(limits)
}

// ProtectedCgroupFence binds a protected cgroup subtree to one execution
// generation and one worker incarnation so stale supervisors cannot attach or
// clean up a replacement runtime.
type ProtectedCgroupFence struct {
	ExecutionID       uuid.UUID
	Generation        int64
	WorkerIncarnation uuid.UUID
}

// ProtectedCgroupSupervisorConfig describes the protected delegated subtree a
// privileged agentd supervisor is allowed to own.
type ProtectedCgroupSupervisorConfig struct {
	ParentPath         string
	SupervisorIdentity ProtectedCgroupIdentity
	ProviderIdentity   ProtectedCgroupIdentity
	ProviderLimits     ProtectedCgroupResourceLimits
	Fence              ProtectedCgroupFence
	SupervisorInstance uuid.UUID
	RuntimeInstance    uuid.UUID
	// RootLease is held by the daemon for its entire process lifetime. It is
	// nil for the standalone diagnostic preflight, which is deliberately
	// non-destructive and never performs startup recovery.
	RootLease  *ProtectedCgroupRootLease
	Diagnostic bool
}

// ProtectedCgroupRootLeaseConfig identifies the daemon process that may run
// startup recovery for one protected delegated cgroup parent.
type ProtectedCgroupRootLeaseConfig struct {
	ParentPath         string
	SupervisorIdentity ProtectedCgroupIdentity
	ProviderIdentity   ProtectedCgroupIdentity
	SupervisorInstance uuid.UUID
}

// ProtectedCgroupPaths exposes the fenced cgroup layout created under the
// protected parent subtree.
type ProtectedCgroupPaths struct {
	ParentPath   string
	BundlePath   string
	AgentdPath   string
	ProviderPath string
}

type protectedCgroupRootLeaseContextKey struct{}

func withProtectedCgroupRootLease(ctx context.Context, lease *ProtectedCgroupRootLease) context.Context {
	if lease == nil {
		return ctx
	}
	return context.WithValue(ctx, protectedCgroupRootLeaseContextKey{}, lease)
}

func protectedCgroupRootLeaseFromContext(ctx context.Context) *ProtectedCgroupRootLease {
	lease, _ := ctx.Value(protectedCgroupRootLeaseContextKey{}).(*ProtectedCgroupRootLease)
	return lease
}

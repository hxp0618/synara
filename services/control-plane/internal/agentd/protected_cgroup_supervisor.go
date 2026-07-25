package agentd

import "github.com/google/uuid"

// ProtectedCgroupIdentity identifies the OS principal that must own the
// protected cgroup subtree or that is expected to run the untrusted Provider.
type ProtectedCgroupIdentity struct {
	UID uint32
	GID uint32
}

// ProtectedCgroupFence binds a protected cgroup subtree to one execution
// generation and one worker incarnation so stale supervisors cannot attach or
// clean up a replacement runtime.
type ProtectedCgroupFence struct {
	Generation        int64
	WorkerIncarnation uuid.UUID
}

// ProtectedCgroupSupervisorConfig describes the protected delegated subtree a
// privileged agentd supervisor is allowed to own.
type ProtectedCgroupSupervisorConfig struct {
	ParentPath         string
	SupervisorIdentity ProtectedCgroupIdentity
	ProviderIdentity   ProtectedCgroupIdentity
	Fence              ProtectedCgroupFence
}

// ProtectedCgroupPaths exposes the fenced cgroup layout created under the
// protected parent subtree.
type ProtectedCgroupPaths struct {
	ParentPath   string
	BundlePath   string
	AgentdPath   string
	ProviderPath string
}

//go:build !linux

package agentd

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestProtectedCgroupSupervisorUnsupported(t *testing.T) {
	_, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath:         "/tmp/protected-cgroup",
		SupervisorIdentity: ProtectedCgroupIdentity{UID: 1, GID: 1},
		ProviderIdentity:   ProtectedCgroupIdentity{UID: 2, GID: 2},
		Fence: ProtectedCgroupFence{
			ExecutionID: uuid.New(), Generation: 1, WorkerIncarnation: uuid.New(),
		},
		SupervisorInstance: uuid.New(),
		RuntimeInstance:    uuid.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("NewProtectedCgroupSupervisor error = %v", err)
	}
}

//go:build linux

package agentd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func TestProtectedCgroupSupervisorCreatesFencedTreeAttachesPIDsAndCleansUp(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	fence := ProtectedCgroupFence{Generation: 7, WorkerIncarnation: uuid.New()}

	supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: fence,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := supervisor.Paths()
	if !strings.Contains(paths.BundlePath, protectedCgroupBundleName(fence)) {
		t.Fatalf("bundle path %q does not contain fence %q", paths.BundlePath, protectedCgroupBundleName(fence))
	}
	installProtectedCgroupControlFiles(t, paths.BundlePath)
	installProtectedCgroupControlFiles(t, paths.AgentdPath)
	installProtectedCgroupControlFiles(t, paths.ProviderPath)

	if err := supervisor.AttachAgentdPID(fence, 1234); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.AttachProviderPID(fence, 5678); err != nil {
		t.Fatal(err)
	}

	assertProtectedCgroupFile(t, filepath.Join(paths.AgentdPath, "cgroup.procs"), "1234")
	assertProtectedCgroupFile(t, filepath.Join(paths.ProviderPath, "cgroup.procs"), "5678")

	if err := supervisor.Cleanup(fence); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ProviderPath, paths.AgentdPath, paths.BundlePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("protected cgroup path %q still exists: %v", path, err)
		}
	}
}

func TestProtectedCgroupSupervisorRejectsFenceMismatch(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	fence := ProtectedCgroupFence{Generation: 9, WorkerIncarnation: uuid.New()}
	supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath:         root,
		SupervisorIdentity: supervisorID,
		ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
		Fence:              fence,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := supervisor.Paths()
	installProtectedCgroupControlFiles(t, paths.BundlePath)
	installProtectedCgroupControlFiles(t, paths.AgentdPath)
	installProtectedCgroupControlFiles(t, paths.ProviderPath)

	wrongFence := ProtectedCgroupFence{Generation: fence.Generation + 1, WorkerIncarnation: fence.WorkerIncarnation}
	if err := supervisor.AttachProviderPID(wrongFence, 42); err == nil || !strings.Contains(err.Error(), "generation fence mismatch") {
		t.Fatalf("wrong generation fence error = %v", err)
	}
	if err := supervisor.Cleanup(wrongFence); err == nil || !strings.Contains(err.Error(), "generation fence mismatch") {
		t.Fatalf("wrong generation cleanup error = %v", err)
	}
}

func TestProtectedCgroupSupervisorRejectsProviderWritableParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	_, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath:         root,
		SupervisorIdentity: supervisorID,
		ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
		Fence:              ProtectedCgroupFence{Generation: 1, WorkerIncarnation: uuid.New()},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be group- or other-writable") {
		t.Fatalf("provider-writable parent error = %v", err)
	}
}

func TestProtectedCgroupSupervisorRejectsProviderSharingSupervisorUID(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	_, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath:         root,
		SupervisorIdentity: supervisorID,
		ProviderIdentity: ProtectedCgroupIdentity{
			UID: supervisorID.UID,
			GID: supervisorID.GID + 1,
		},
		Fence: ProtectedCgroupFence{Generation: 2, WorkerIncarnation: uuid.New()},
	})
	if err == nil || !strings.Contains(err.Error(), "must not share a UID") {
		t.Fatalf("same-uid protected cgroup error = %v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("same-uid validation created protected cgroup entries: %#v", entries)
	}
}

func overrideProtectedCgroupLinuxTestEnvironment(t *testing.T, root string) {
	t.Helper()
	originalFstatfs := protectedCgroupFstatfs
	protectedCgroupFstatfs = func(_ int, stats *unix.Statfs_t) error {
		stats.Type = int64(linuxCgroup2SuperMagic)
		return nil
	}
	originalMountInfo := protectedCgroupMountInfo
	protectedCgroupMountInfo = func() ([]byte, error) {
		parent := filepath.Dir(root)
		return []byte("36 35 0:30 / " + parent + " rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n"), nil
	}
	originalCleanupHook := protectedCgroupCleanupTestHook
	protectedCgroupCleanupTestHook = func(_ int, directoryPath string) error {
		return removeFakeProtectedCgroupControlFiles(directoryPath)
	}
	t.Cleanup(func() {
		protectedCgroupFstatfs = originalFstatfs
		protectedCgroupMountInfo = originalMountInfo
		protectedCgroupCleanupTestHook = originalCleanupHook
	})
}

func installProtectedCgroupControlFiles(t *testing.T, directory string) {
	t.Helper()
	for _, file := range []struct {
		name    string
		content string
	}{
		{name: "cgroup.procs", content: ""},
		{name: "cgroup.kill", content: "0"},
		{name: "cgroup.events", content: "populated 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(directory, file.name), []byte(file.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func removeFakeProtectedCgroupControlFiles(directory string) error {
	var result error
	for _, name := range []string{"cgroup.procs", "cgroup.kill", "cgroup.events"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			result = errors.Join(result, fmt.Errorf("stat fake protected cgroup control file %s: %w", path, err))
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove fake protected cgroup control file %s: %w", path, err))
		}
	}
	return result
}

func assertProtectedCgroupFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

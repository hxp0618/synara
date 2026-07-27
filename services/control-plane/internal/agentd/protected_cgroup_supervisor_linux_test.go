//go:build linux

package agentd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 7, WorkerIncarnation: uuid.New()}
	supervisorInstance := uuid.New()
	runtimeInstance := uuid.New()
	rootLease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: supervisorInstance,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rootLease.Close()
	if err := rootLease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}

	supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: fence,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		SupervisorInstance: supervisorInstance, RuntimeInstance: runtimeInstance, RootLease: rootLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := supervisor.Paths()
	bundleName := protectedCgroupBundleName(fence, supervisorInstance, runtimeInstance)
	if !strings.Contains(paths.BundlePath, bundleName) {
		t.Fatalf("bundle path %q does not contain fence %q", paths.BundlePath, bundleName)
	}
	assertProtectedCgroupFile(t, filepath.Join(paths.ProviderPath, "pids.max"), "512")
	assertProtectedCgroupFile(t, filepath.Join(paths.ProviderPath, "memory.max"), "8589934592")
	assertProtectedCgroupFile(t, filepath.Join(paths.ProviderPath, "cpu.max"), "400000 100000")
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
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 9, WorkerIncarnation: uuid.New()}
	supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath:         root,
		SupervisorIdentity: supervisorID,
		ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		Fence:              fence,
		SupervisorInstance: uuid.New(),
		RuntimeInstance:    uuid.New(),
		Diagnostic:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := supervisor.Paths()
	installProtectedCgroupControlFiles(t, paths.BundlePath)
	installProtectedCgroupControlFiles(t, paths.AgentdPath)
	installProtectedCgroupControlFiles(t, paths.ProviderPath)

	wrongFence := ProtectedCgroupFence{
		ExecutionID: fence.ExecutionID, Generation: fence.Generation + 1,
		WorkerIncarnation: fence.WorkerIncarnation,
	}
	if err := supervisor.AttachProviderPID(wrongFence, 42); err == nil || !strings.Contains(err.Error(), "generation fence mismatch") {
		t.Fatalf("wrong generation fence error = %v", err)
	}
	if err := supervisor.Cleanup(wrongFence); err == nil || !strings.Contains(err.Error(), "generation fence mismatch") {
		t.Fatalf("wrong generation cleanup error = %v", err)
	}
	wrongExecutionFence := fence
	wrongExecutionFence.ExecutionID = uuid.New()
	if err := supervisor.AttachProviderPID(wrongExecutionFence, 42); err == nil ||
		!strings.Contains(err.Error(), "execution fence mismatch") {
		t.Fatalf("wrong execution fence error = %v", err)
	}
	if err := supervisor.Cleanup(fence); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedCgroupRuntimeRequiresCompletedDaemonRootLease(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	supervisorInstance := uuid.New()
	config := ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		Fence:              ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 10, WorkerIncarnation: uuid.New()},
		SupervisorInstance: supervisorInstance, RuntimeInstance: uuid.New(),
	}
	if _, err := NewProtectedCgroupSupervisor(config); err == nil || !strings.Contains(err.Error(), "requires the daemon root lease") {
		t.Fatalf("missing daemon lease error = %v", err)
	}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: supervisorInstance,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	config.RootLease = lease
	if _, err := NewProtectedCgroupSupervisor(config); err == nil || !strings.Contains(err.Error(), "has not completed startup recovery") {
		t.Fatalf("unrecovered daemon lease error = %v", err)
	}
}

func TestProtectedCgroupRootLeaseRequiresCPUAndMemoryAndPidsControllers(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID,
		ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "required pids controller") {
		t.Fatalf("missing pids controller recovery error = %v", err)
	}
}

func TestProtectedCgroupSupervisorFailsClosedBeforeProviderWhenLimitInterfaceIsMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	originalHook := protectedCgroupDirectoryCreatedTestHook
	protectedCgroupDirectoryCreatedTestHook = func(fd int, directoryPath string) error {
		if err := originalHook(fd, directoryPath); err != nil {
			return err
		}
		if filepath.Base(directoryPath) == "provider" {
			return os.Remove(filepath.Join(directoryPath, "pids.max"))
		}
		return nil
	}
	t.Cleanup(func() { protectedCgroupDirectoryCreatedTestHook = originalHook })
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	_, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID,
		ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		Fence:              ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 10, WorkerIncarnation: uuid.New()},
		SupervisorInstance: uuid.New(), RuntimeInstance: uuid.New(), Diagnostic: true,
	})
	if err == nil || !strings.Contains(err.Error(), "pids.max") {
		t.Fatalf("missing pids.max interface error = %v", err)
	}
	if entries := linuxCgroupTestChildren(t, root); len(entries) != 0 {
		t.Fatalf("failed limit setup leaked protected cgroups: %v", entries)
	}
}

func TestProtectedCgroupStandaloneConstructionDoesNotScavengeActiveRuntime(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 11, WorkerIncarnation: uuid.New()}
	supervisorInstance := uuid.New()
	rootLease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: supervisorInstance,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rootLease.Close()
	if err := rootLease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	active, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: fence,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		SupervisorInstance: supervisorInstance, RuntimeInstance: uuid.New(), RootLease: rootLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	activePaths := active.Paths()
	installProtectedCgroupControlFiles(t, activePaths.BundlePath)
	installProtectedCgroupControlFiles(t, activePaths.AgentdPath)
	installProtectedCgroupControlFiles(t, activePaths.ProviderPath)

	diagnostic, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		Fence:              ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 12, WorkerIncarnation: fence.WorkerIncarnation},
		SupervisorInstance: uuid.New(), RuntimeInstance: uuid.New(),
		Diagnostic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(activePaths.BundlePath); err != nil {
		t.Fatalf("standalone diagnostic construction scavenged active runtime: %v", err)
	}
	diagnosticFence := diagnostic.Fence()
	diagnosticPaths := diagnostic.Paths()
	if !strings.HasPrefix(filepath.Base(activePaths.BundlePath), protectedCgroupBundlePrefix) ||
		!strings.HasPrefix(filepath.Base(diagnosticPaths.BundlePath), protectedCgroupDiagnosticPrefix) {
		t.Fatalf("runtime/diagnostic namespaces = %q/%q", activePaths.BundlePath, diagnosticPaths.BundlePath)
	}
	installProtectedCgroupControlFiles(t, diagnosticPaths.BundlePath)
	installProtectedCgroupControlFiles(t, diagnosticPaths.AgentdPath)
	installProtectedCgroupControlFiles(t, diagnosticPaths.ProviderPath)
	if err := diagnostic.Cleanup(diagnosticFence); err != nil {
		t.Fatal(err)
	}
	if err := active.Cleanup(fence); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedCgroupRootLeaseRejectsRollingOverlapAndReleasesOnClose(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	first, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "already leased by another agentd") {
		t.Fatalf("rolling overlap lease error = %v", err)
	}
	if err := first.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	if err := first.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("repeated startup recovery error = %v", err)
	}
	diagnosticFence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 1, WorkerIncarnation: uuid.New()}
	standaloneDiagnostic, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: diagnosticFence,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		SupervisorInstance: uuid.New(), RuntimeInstance: uuid.New(), Diagnostic: true,
	})
	if err != nil {
		t.Fatalf("standalone diagnostic must coexist non-destructively with live daemon: %v", err)
	}
	standalonePaths := standaloneDiagnostic.Paths()
	installProtectedCgroupControlFiles(t, standalonePaths.BundlePath)
	installProtectedCgroupControlFiles(t, standalonePaths.AgentdPath)
	installProtectedCgroupControlFiles(t, standalonePaths.ProviderPath)
	if err := standaloneDiagnostic.Cleanup(diagnosticFence); err != nil {
		t.Fatal(err)
	}
	daemonDiagnostic, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: diagnosticFence,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		SupervisorInstance: uuid.New(), RuntimeInstance: uuid.New(), RootLease: first, Diagnostic: true,
	})
	if err != nil {
		t.Fatalf("daemon-owned diagnostic construction: %v", err)
	}
	diagnosticPaths := daemonDiagnostic.Paths()
	installProtectedCgroupControlFiles(t, diagnosticPaths.BundlePath)
	installProtectedCgroupControlFiles(t, diagnosticPaths.AgentdPath)
	installProtectedCgroupControlFiles(t, diagnosticPaths.ProviderPath)
	if err := daemonDiagnostic.Cleanup(diagnosticFence); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedCgroupRootLeaseReleasedAfterLeaseHolderCrash(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(
		os.Args[0], "-test.run=^TestProtectedCgroupRootLeaseCrashHelper$", "--",
		"--synara-protected-cgroup-root-lease-helper", root, ready,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()
	waitForProcessTreeReady(t, ready)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	leaseConfig := ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	}
	if lease, err := AcquireProtectedCgroupRootLease(leaseConfig); err == nil {
		_ = lease.Close()
		t.Fatal("acquired root lease while crash helper was alive")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed lease helper exited successfully")
	}
	command.Process = nil
	lease, err := AcquireProtectedCgroupRootLease(leaseConfig)
	if err != nil {
		t.Fatalf("acquire root lease after holder crash: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedCgroupRootLeaseCrashHelper(t *testing.T) {
	arguments := helperArgumentsAfter("--synara-protected-cgroup-root-lease-helper")
	if len(arguments) != 2 {
		t.Skip("protected cgroup root lease crash helper")
	}
	rootFD, err := unix.Open(arguments[0], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	if err := unix.Flock(rootFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arguments[1], []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestProtectedCgroupRootLeaseRejectsEvenEmptyLegacyV1WithoutMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 13, WorkerIncarnation: uuid.New()}
	v2Paths := createProtectedCgroupRuntimeOrphanFixture(t, root, fence)

	legacyPath := filepath.Join(root, "synara-g12-i"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	for _, path := range []string{legacyPath, filepath.Join(legacyPath, "agentd"), filepath.Join(legacyPath, "provider")} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
		installProtectedCgroupControlFiles(t, path)
	}
	diagnosticFence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 99, WorkerIncarnation: uuid.New()}
	activeDiagnostic, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: diagnosticFence,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		SupervisorInstance: uuid.New(), RuntimeInstance: uuid.New(), Diagnostic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	diagnosticPaths := activeDiagnostic.Paths()
	installProtectedCgroupControlFiles(t, diagnosticPaths.BundlePath)
	installProtectedCgroupControlFiles(t, diagnosticPaths.AgentdPath)
	installProtectedCgroupControlFiles(t, diagnosticPaths.ProviderPath)
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "manual cleanup") {
		t.Fatalf("empty legacy migration fence error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(v2Paths.BundlePath, "cgroup.kill"), "0")
	assertProtectedCgroupFile(t, filepath.Join(legacyPath, "cgroup.kill"), "0")
	if _, err := os.Stat(diagnosticPaths.BundlePath); err != nil {
		t.Fatalf("startup recovery mutated active diagnostic bundle: %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(diagnosticPaths.BundlePath, "cgroup.kill"), "0")
	if err := activeDiagnostic.Cleanup(diagnosticFence); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedCgroupRootLeaseRecoversV2OrphanWithOnlyCurrentParentPID(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	v2Orphan := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 23, WorkerIncarnation: uuid.New(),
	})
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v2Orphan.BundlePath); !os.IsNotExist(err) {
		t.Fatalf("v2 orphan survived fenced recovery: %v", err)
	}
}

func TestProtectedCgroupRootLeaseRejectsAdditionalParentProcessWithoutMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	v2Orphan := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 24, WorkerIncarnation: uuid.New(),
	})
	if err := os.WriteFile(
		filepath.Join(root, protectedCgroupSupervisorSubgroup, "cgroup.procs"),
		[]byte(fmt.Sprintf("%d\n%d\n", protectedCgroupCurrentPID(), protectedCgroupCurrentPID()+100000)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	if lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	}); err == nil {
		_ = lease.Close()
		t.Fatal("root lease accepted an additional parent process")
	} else if !strings.Contains(err.Error(), "must contain only current agentd pid") {
		t.Fatalf("additional parent process error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(v2Orphan.BundlePath, "cgroup.kill"), "0")
}

func TestProtectedCgroupRootLeaseRechecksParentProcessesAfterRecoveryBarrier(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	v2Orphan := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 25, WorkerIncarnation: uuid.New(),
	})
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	originalHook := protectedCgroupRecoveryValidatedTestHook
	protectedCgroupRecoveryValidatedTestHook = func() error {
		return os.WriteFile(
			filepath.Join(root, protectedCgroupSupervisorSubgroup, "cgroup.procs"),
			[]byte(fmt.Sprintf("%d\n%d\n", protectedCgroupCurrentPID(), protectedCgroupCurrentPID()+100001)),
			0o600,
		)
	}
	t.Cleanup(func() { protectedCgroupRecoveryValidatedTestHook = originalHook })
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "must contain only current agentd pid") {
		t.Fatalf("post-barrier parent process error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(v2Orphan.BundlePath, "cgroup.kill"), "0")
}

func TestProtectedCgroupRootLeaseRejectsLegacyEntryCreatedAfterRecoveryBarrier(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	v2Orphan := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 26, WorkerIncarnation: uuid.New(),
	})
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	legacyPath := filepath.Join(root, "synara-g27-i"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	originalHook := protectedCgroupRecoveryValidatedTestHook
	protectedCgroupRecoveryValidatedTestHook = func() error {
		for _, path := range []string{legacyPath, filepath.Join(legacyPath, "agentd"), filepath.Join(legacyPath, "provider")} {
			if err := os.Mkdir(path, 0o750); err != nil {
				return err
			}
			installProtectedCgroupControlFiles(t, path)
		}
		return nil
	}
	t.Cleanup(func() { protectedCgroupRecoveryValidatedTestHook = originalHook })
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "appeared during recovery") {
		t.Fatalf("post-barrier legacy entry error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(v2Orphan.BundlePath, "cgroup.kill"), "0")
	assertProtectedCgroupFile(t, filepath.Join(legacyPath, "cgroup.kill"), "0")
}

func TestProtectedCgroupRootLeaseFailsClosedOnPopulatedLegacyV1Bundle(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	v2Orphan := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 20, WorkerIncarnation: uuid.New(),
	})
	legacyPath := filepath.Join(root, "synara-g19-i"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	for _, path := range []string{legacyPath, filepath.Join(legacyPath, "agentd"), filepath.Join(legacyPath, "provider")} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
		installProtectedCgroupControlFiles(t, path)
	}
	if err := os.WriteFile(filepath.Join(legacyPath, "cgroup.events"), []byte("populated 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "manual cleanup") {
		t.Fatalf("populated legacy migration fence error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(legacyPath, "cgroup.kill"), "0")
	assertProtectedCgroupFile(t, filepath.Join(v2Orphan.BundlePath, "cgroup.kill"), "0")
}

func TestProtectedCgroupRootLeaseValidatesAllBundlesBeforeAnyKill(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	firstPath := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 14, WorkerIncarnation: uuid.New(),
	}).BundlePath
	secondPath := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 15, WorkerIncarnation: uuid.New(),
	}).BundlePath
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	originalHook := protectedCgroupRecoveryValidatedTestHook
	protectedCgroupRecoveryValidatedTestHook = func() error {
		return os.Mkdir(filepath.Join(secondPath, "unknown"), 0o750)
	}
	t.Cleanup(func() { protectedCgroupRecoveryValidatedTestHook = originalHook })
	err = lease.RecoverOrphans()
	if err == nil || !strings.Contains(err.Error(), "unknown child directory") {
		t.Fatalf("post-validation mutation recovery error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(firstPath, "cgroup.kill"), "0")
	assertProtectedCgroupFile(t, filepath.Join(secondPath, "cgroup.kill"), "0")
}

func TestProtectedCgroupRootLeaseAcceptsFrozenAdvertisedControllerInterfaces(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	paths := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
		ExecutionID: uuid.New(), Generation: 31, WorkerIncarnation: uuid.New(),
	})
	for name := range map[string]struct{}{"cpu.stat": {}, "memory.events": {}} {
		if err := os.WriteFile(filepath.Join(paths.BundlePath, name), []byte("kernel fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	originalCleanupHook := protectedCgroupCleanupTestHook
	protectedCgroupCleanupTestHook = func(_ int, directoryPath string) error {
		result := removeFakeProtectedCgroupControlFiles(directoryPath)
		if directoryPath == paths.BundlePath {
			result = errors.Join(result, os.Remove(filepath.Join(directoryPath, "cpu.stat")))
			result = errors.Join(result, os.Remove(filepath.Join(directoryPath, "memory.events")))
		}
		return result
	}
	t.Cleanup(func() { protectedCgroupCleanupTestHook = originalCleanupHook })
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID,
		ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.BundlePath); !os.IsNotExist(err) {
		t.Fatalf("recovered bundle survived: %v", err)
	}
}

func TestProtectedCgroupRootLeaseRejectsUnadvertisedAndNonRegularInterfacesWithoutKill(t *testing.T) {
	tests := []struct {
		name      string
		entry     string
		wantError string
		install   func(*testing.T, string) func()
	}{
		{
			name: "unknown controller prefix", entry: "evil.stat", wantError: "unknown entry",
			install: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "ordinary file", entry: "notes", wantError: "unknown entry",
			install: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "controller-like symlink", entry: "cpu.stat", wantError: "must not be a symlink",
			install: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.Symlink("cgroup.events", path); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "controller-like socket", entry: "cpu.stat", wantError: "regular cgroup interface",
			install: func(t *testing.T, path string) func() {
				t.Helper()
				originalWorkingDirectory, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chdir(filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
				listener, listenErr := net.Listen("unix", filepath.Base(path))
				if err := os.Chdir(originalWorkingDirectory); err != nil {
					t.Fatal(err)
				}
				if listenErr != nil {
					t.Fatal(listenErr)
				}
				return func() { _ = listener.Close() }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0o750); err != nil {
				t.Fatal(err)
			}
			overrideProtectedCgroupLinuxTestEnvironment(t, root)
			paths := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
				ExecutionID: uuid.New(), Generation: 32, WorkerIncarnation: uuid.New(),
			})
			cleanup := test.install(t, filepath.Join(paths.BundlePath, test.entry))
			defer cleanup()
			supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
			lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
				ParentPath: root, SupervisorIdentity: supervisorID,
				ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
				SupervisorInstance: uuid.New(),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("unsafe interface recovery error = %v", err)
			}
			assertProtectedCgroupFile(t, filepath.Join(paths.BundlePath, "cgroup.kill"), "0")
		})
	}
}

func TestProtectedCgroupRootLeaseRejectsMalformedOrDriftingControllerListWithoutKill(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*testing.T, string)
		wantError string
	}{
		{
			name: "malformed controller list",
			configure: func(t *testing.T, bundle string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bundle, "cgroup.controllers"), []byte("cpu Evil\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "invalid controller",
		},
		{
			name: "controller allowlist drift",
			configure: func(t *testing.T, bundle string) {
				t.Helper()
				originalHook := protectedCgroupRecoveryValidatedTestHook
				protectedCgroupRecoveryValidatedTestHook = func() error {
					if err := os.WriteFile(filepath.Join(bundle, "cgroup.controllers"), []byte("cpu memory evil\n"), 0o600); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(bundle, "evil.stat"), []byte("fixture\n"), 0o600)
				}
				t.Cleanup(func() { protectedCgroupRecoveryValidatedTestHook = originalHook })
			},
			wantError: "unknown entry",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0o750); err != nil {
				t.Fatal(err)
			}
			overrideProtectedCgroupLinuxTestEnvironment(t, root)
			paths := createProtectedCgroupRuntimeOrphanFixture(t, root, ProtectedCgroupFence{
				ExecutionID: uuid.New(), Generation: 33, WorkerIncarnation: uuid.New(),
			})
			test.configure(t, paths.BundlePath)
			supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
			lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
				ParentPath: root, SupervisorIdentity: supervisorID,
				ProviderIdentity:   ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1},
				SupervisorInstance: uuid.New(),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("controller list recovery error = %v", err)
			}
			assertProtectedCgroupFile(t, filepath.Join(paths.BundlePath, "cgroup.kill"), "0")
		})
	}
}

func TestProtectedCgroupSupervisorSameFenceConcurrentCreationOnlyOneSucceeds(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 16, WorkerIncarnation: uuid.New()}
	supervisorInstance := uuid.New()
	rootLease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: supervisorInstance,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rootLease.Close()
	if err := rootLease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type result struct {
		supervisor *ProtectedCgroupSupervisor
		err        error
	}
	results := make(chan result, 2)
	for index := 0; index < 2; index++ {
		go func() {
			<-start
			supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
				ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: fence,
				ProviderLimits:     testProtectedCgroupResourceLimits(),
				SupervisorInstance: supervisorInstance, RuntimeInstance: uuid.New(), RootLease: rootLease,
			})
			results <- result{supervisor: supervisor, err: err}
		}()
	}
	close(start)
	var winner *ProtectedCgroupSupervisor
	failures := 0
	for index := 0; index < 2; index++ {
		outcome := <-results
		if outcome.err == nil {
			winner = outcome.supervisor
		} else if strings.Contains(outcome.err.Error(), "already active in this agentd") {
			failures++
		} else {
			t.Fatalf("unexpected concurrent creation error = %v", outcome.err)
		}
	}
	if winner == nil || failures != 1 {
		t.Fatalf("same-fence winners/failures = %v/%d, want 1/1", winner != nil, failures)
	}
	paths := winner.Paths()
	installProtectedCgroupControlFiles(t, paths.BundlePath)
	installProtectedCgroupControlFiles(t, paths.AgentdPath)
	installProtectedCgroupControlFiles(t, paths.ProviderPath)
	if err := winner.Cleanup(fence); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedCgroupCleanupFailurePoisonsOnlyTheFailedExecutionFence(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	supervisorInstance := uuid.New()
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: supervisorInstance,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 21, WorkerIncarnation: uuid.New()}
	config := ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID, Fence: fence,
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		SupervisorInstance: supervisorInstance, RuntimeInstance: uuid.New(), RootLease: lease,
	}
	failed, err := NewProtectedCgroupSupervisor(config)
	if err != nil {
		t.Fatal(err)
	}
	failedPaths := failed.Paths()
	installProtectedCgroupControlFiles(t, failedPaths.BundlePath)
	installProtectedCgroupControlFiles(t, failedPaths.AgentdPath)
	installProtectedCgroupControlFiles(t, failedPaths.ProviderPath)
	originalHook := protectedCgroupCleanupTestHook
	t.Cleanup(func() { protectedCgroupCleanupTestHook = originalHook })
	protectedCgroupCleanupTestHook = func(_ int, path string) error {
		if path == failedPaths.ProviderPath {
			return errors.New("injected provider cgroup removal failure")
		}
		return removeFakeProtectedCgroupControlFiles(path)
	}
	if err := failed.Cleanup(fence); err == nil || !strings.Contains(err.Error(), "injected provider cgroup removal failure") {
		t.Fatalf("injected cleanup failure = %v", err)
	}
	protectedCgroupCleanupTestHook = originalHook
	if err := failed.Cleanup(fence); err == nil || !strings.Contains(err.Error(), "fence remains poisoned") {
		t.Fatalf("repeated poisoned cleanup error = %v", err)
	}
	config.RuntimeInstance = uuid.New()
	if _, err := NewProtectedCgroupSupervisor(config); err == nil || !strings.Contains(err.Error(), "already active in this agentd") {
		t.Fatalf("same-fence launch after cleanup failure = %v", err)
	}
	otherFence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 22, WorkerIncarnation: fence.WorkerIncarnation}
	config.Fence = otherFence
	config.RuntimeInstance = uuid.New()
	other, err := NewProtectedCgroupSupervisor(config)
	if err != nil {
		t.Fatalf("different fence was poisoned: %v", err)
	}
	otherPaths := other.Paths()
	installProtectedCgroupControlFiles(t, otherPaths.BundlePath)
	installProtectedCgroupControlFiles(t, otherPaths.AgentdPath)
	installProtectedCgroupControlFiles(t, otherPaths.ProviderPath)
	if err := other.Cleanup(otherFence); err != nil {
		t.Fatal(err)
	}
}

func TestParseProtectedCgroupBundleNameRequiresCanonicalLowercaseUUIDs(t *testing.T) {
	fence := ProtectedCgroupFence{
		ExecutionID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), Generation: 17,
		WorkerIncarnation: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
	}
	name := protectedCgroupBundleName(
		fence,
		uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
		uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"),
	)
	if _, recognized, err := parseProtectedCgroupBundleName(name); err != nil || !recognized {
		t.Fatalf("canonical bundle parse = recognized %v err %v", recognized, err)
	}
	uppercase := strings.ToUpper(name)
	if _, recognized, err := parseProtectedCgroupBundleName(uppercase); recognized || err != nil {
		// The uppercase prefix is not recognized at all, so it cannot be selected
		// for destructive recovery.
		t.Fatalf("uppercase bundle parse = recognized %v err %v", recognized, err)
	}
	mixedCase := name[:len(name)-1] + strings.ToUpper(name[len(name)-1:])
	if _, recognized, err := parseProtectedCgroupBundleName(mixedCase); !recognized || err == nil ||
		!strings.Contains(err.Error(), "canonical lowercase") {
		t.Fatalf("mixed-case bundle parse = recognized %v err %v", recognized, err)
	}
	malformed := protectedCgroupBundlePrefix + "not-a-valid-bundle"
	if _, recognized, err := parseProtectedCgroupBundleName(malformed); !recognized || err == nil {
		t.Fatalf("malformed bundle parse = recognized %v err %v", recognized, err)
	}
}

func TestProtectedCgroupRootLeaseRejectsSymlinkAndUnsafeModeWithoutKill(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)
	supervisorID := ProtectedCgroupIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	providerID := ProtectedCgroupIdentity{UID: supervisorID.UID + 1, GID: supervisorID.GID + 1}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 18, WorkerIncarnation: uuid.New()}
	paths := createProtectedCgroupRuntimeOrphanFixture(t, root, fence)
	if err := os.Chmod(paths.ProviderPath, 0o770); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: supervisorID, ProviderIdentity: providerID,
		SupervisorInstance: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "must not be group- or other-writable") {
		t.Fatalf("unsafe child mode recovery error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(paths.BundlePath, "cgroup.kill"), "0")

	if err := os.Chmod(paths.ProviderPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.ProviderPath); err == nil {
		t.Fatal("removed non-empty provider fixture unexpectedly")
	}
	// Replace the child only after removing fake cgroup control files. Recovery
	// must reject the symlink before writing any cgroup.kill file.
	if err := removeFakeProtectedCgroupControlFiles(paths.ProviderPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.ProviderPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), paths.ProviderPath); err != nil {
		t.Fatal(err)
	}
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink child recovery error = %v", err)
	}
	assertProtectedCgroupFile(t, filepath.Join(paths.BundlePath, "cgroup.kill"), "0")
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
		ProviderLimits:     testProtectedCgroupResourceLimits(),
		Fence: ProtectedCgroupFence{
			ExecutionID: uuid.New(), Generation: 1, WorkerIncarnation: uuid.New(),
		},
		SupervisorInstance: uuid.New(),
		RuntimeInstance:    uuid.New(),
		Diagnostic:         true,
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
		Fence: ProtectedCgroupFence{
			ExecutionID: uuid.New(), Generation: 2, WorkerIncarnation: uuid.New(),
		},
		SupervisorInstance: uuid.New(),
		RuntimeInstance:    uuid.New(),
		Diagnostic:         true,
	})
	if err == nil || !strings.Contains(err.Error(), "must not share a UID") {
		t.Fatalf("same-uid protected cgroup error = %v", err)
	}
	entries := linuxCgroupTestChildren(t, root)
	if len(entries) != 0 {
		t.Fatalf("same-uid validation created protected cgroup entries: %#v", entries)
	}
}

func overrideProtectedCgroupLinuxTestEnvironment(t *testing.T, root string) {
	t.Helper()
	originalCurrentPID := protectedCgroupCurrentPID
	protectedCgroupCurrentPID = os.Getpid
	installProtectedCgroupControlFiles(t, root)
	if err := os.WriteFile(filepath.Join(root, "cgroup.procs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "cgroup.subtree_control"),
		[]byte("cpu memory pids\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	supervisorPath := filepath.Join(root, protectedCgroupSupervisorSubgroup)
	if err := os.Mkdir(supervisorPath, 0o750); err != nil {
		t.Fatal(err)
	}
	installProtectedCgroupControlFiles(t, supervisorPath)
	if err := os.WriteFile(
		filepath.Join(supervisorPath, "cgroup.procs"),
		[]byte(strconv.Itoa(protectedCgroupCurrentPID())+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
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
	originalDirectoryCreatedHook := protectedCgroupDirectoryCreatedTestHook
	protectedCgroupDirectoryCreatedTestHook = func(_ int, directoryPath string) error {
		for _, file := range fakeProtectedCgroupControlFiles() {
			if err := os.WriteFile(filepath.Join(directoryPath, file.name), []byte(file.content), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	originalRecoveryHook := protectedCgroupRecoveryValidatedTestHook
	protectedCgroupRecoveryValidatedTestHook = func() error { return nil }
	t.Cleanup(func() {
		protectedCgroupFstatfs = originalFstatfs
		protectedCgroupMountInfo = originalMountInfo
		protectedCgroupCleanupTestHook = originalCleanupHook
		protectedCgroupDirectoryCreatedTestHook = originalDirectoryCreatedHook
		protectedCgroupRecoveryValidatedTestHook = originalRecoveryHook
		protectedCgroupCurrentPID = originalCurrentPID
	})
}

func createProtectedCgroupRuntimeOrphanFixture(
	t *testing.T,
	root string,
	fence ProtectedCgroupFence,
) ProtectedCgroupPaths {
	t.Helper()
	bundlePath := filepath.Join(root, protectedCgroupBundleName(fence, uuid.New(), uuid.New()))
	paths := ProtectedCgroupPaths{
		ParentPath: root, BundlePath: bundlePath,
		AgentdPath: filepath.Join(bundlePath, "agentd"), ProviderPath: filepath.Join(bundlePath, "provider"),
	}
	for _, path := range []string{paths.BundlePath, paths.AgentdPath, paths.ProviderPath} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
		installProtectedCgroupControlFiles(t, path)
	}
	return paths
}

func installProtectedCgroupControlFiles(t *testing.T, directory string) {
	t.Helper()
	for _, file := range fakeProtectedCgroupControlFiles() {
		if err := os.WriteFile(filepath.Join(directory, file.name), []byte(file.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func fakeProtectedCgroupControlFiles() []struct {
	name    string
	content string
} {
	return []struct {
		name    string
		content string
	}{
		{name: "cgroup.procs", content: ""},
		{name: "cgroup.kill", content: "0"},
		{name: "cgroup.events", content: "populated 0\n"},
		{name: "cgroup.controllers", content: "cpu memory pids\n"},
		{name: "cgroup.subtree_control", content: ""},
		{name: "pids.max", content: ""},
		{name: "memory.max", content: ""},
		{name: "cpu.max", content: ""},
	}
}

func removeFakeProtectedCgroupControlFiles(directory string) error {
	var result error
	for _, file := range fakeProtectedCgroupControlFiles() {
		name := file.name
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

func testProtectedCgroupResourceLimits() ProtectedCgroupResourceLimits {
	return ProtectedCgroupResourceLimits{
		PidsMax: 512, MemoryMaxBytes: 8 << 30, CPUQuotaMicros: 400_000, CPUPeriodMicros: 100_000,
	}
}

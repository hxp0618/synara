//go:build linux

package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	protectedCgroupLiveEnabledEnv    = "SYNARA_CGROUP_V2_LIVE"
	protectedCgroupLiveRootEnv       = "SYNARA_CGROUP_V2_LIVE_ROOT"
	protectedCgroupLiveBinaryEnv     = "SYNARA_CGROUP_V2_LIVE_BINARY"
	protectedCgroupLiveStateDirEnv   = "SYNARA_CGROUP_V2_LIVE_STATE_DIR"
	protectedCgroupLiveHelperUnitEnv = "SYNARA_CGROUP_V2_LIVE_HELPER_UNIT"
	protectedCgroupLiveHelperModeEnv = "SYNARA_CGROUP_V2_LIVE_HELPER_MODE"
)

type protectedCgroupLiveEvidence struct {
	SchemaVersion string                              `json:"schemaVersion"`
	CgroupRoot    string                              `json:"cgroupRoot"`
	MainPID       int                                 `json:"mainPid"`
	Systemd       protectedCgroupLiveSystemdProof     `json:"systemd"`
	Scenarios     map[string]protectedCgroupLiveProof `json:"scenarios"`
}

type protectedCgroupLiveSystemdProof struct {
	Unit               string   `json:"unit"`
	Delegate           string   `json:"delegate"`
	DelegateSubgroup   string   `json:"delegateSubgroup"`
	KillMode           string   `json:"killMode"`
	ControlGroup       string   `json:"controlGroup"`
	ActiveState        string   `json:"activeState"`
	SubState           string   `json:"subState"`
	ParentProcessCount int      `json:"parentProcessCount"`
	SupervisorPIDs     []int    `json:"supervisorPids"`
	EnabledControllers []string `json:"enabledControllers"`
}

type protectedCgroupLiveProof struct {
	Status         string                         `json:"status"`
	Mutation       string                         `json:"mutation,omitempty"`
	Bundle         string                         `json:"bundle,omitempty"`
	ProviderPID    int                            `json:"providerPid,omitempty"`
	DescendantPID  int                            `json:"descendantPid,omitempty"`
	Detail         string                         `json:"detail,omitempty"`
	ProviderLimits *ProtectedCgroupResourceLimits `json:"providerLimits,omitempty"`
}

type protectedCgroupLiveCrashRecord struct {
	BundlePath  string `json:"bundlePath"`
	SurvivorPID int    `json:"survivorPid"`
}

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && (os.Args[1] == protectedCgroupProbeHelperCommand || os.Args[1] == protectedCgroupProbeChildCommand) {
		handled, err := RunProtectedCgroupCommand(context.Background(), os.Args, os.Stdout)
		if handled {
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// TestProtectedCgroupV3LiveIntegration is intentionally absent from ordinary
// test runs. The host runner starts this exact test as the only MainPID in a
// real systemd Delegate=yes service cgroup on unified cgroup v2.
func TestProtectedCgroupV3LiveIntegration(t *testing.T) {
	if os.Getenv(protectedCgroupLiveEnabledEnv) != "1" {
		t.Skip("set SYNARA_CGROUP_V2_LIVE=1 only inside the dedicated disposable Linux host lane")
	}
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Fatal("live protected-cgroup integration requires uid=0 gid=0")
	}
	root := filepath.Clean(os.Getenv(protectedCgroupLiveRootEnv))
	if root == "." || !strings.HasPrefix(root, "/sys/fs/cgroup/system.slice/synara-cgroup-v2-live-") {
		t.Fatalf("unsafe or missing live cgroup root %q", root)
	}
	assertLiveCgroupRoot(t, root, os.Getpid())

	evidence := protectedCgroupLiveEvidence{
		SchemaVersion: "synara.protected-cgroup-v3-live-test.v1",
		CgroupRoot:    root,
		MainPID:       os.Getpid(),
		Systemd:       assertLiveSystemdService(t, root),
		Scenarios:     make(map[string]protectedCgroupLiveProof),
	}

	t.Run("runtime-diagnostic-overlap-and-fence", func(t *testing.T) {
		evidence.Scenarios["runtimeDiagnosticOverlapAndFence"] = testProtectedCgroupLiveRuntime(t, root)
	})
	evidence.Systemd = assertLiveSystemdService(t, root, true)
	assertLiveCgroupRoot(t, root, os.Getpid())
	t.Run("supervisor-subgroup-extra-pid-zero-mutation", func(t *testing.T) {
		evidence.Scenarios["supervisorSubgroupExtraPidZeroMutation"] = testProtectedCgroupLiveParentExtraPID(t, root)
	})
	assertLiveCgroupRoot(t, root, os.Getpid())
	t.Run("legacy-v1-zero-mutation", func(t *testing.T) {
		evidence.Scenarios["legacyV1ZeroMutation"] = testProtectedCgroupLiveLegacy(t, root)
	})
	assertLiveCgroupRoot(t, root, os.Getpid())
	t.Run("unknown-child-repair-recovery", func(t *testing.T) {
		evidence.Scenarios["unknownChildRepairRecovery"] = testProtectedCgroupLiveUnknownChild(t, root)
	})
	assertLiveCgroupRoot(t, root, os.Getpid())
	t.Run("sigkill-holder-orphan-recovery", func(t *testing.T) {
		evidence.Scenarios["sigkillHolderOrphanRecovery"] = testProtectedCgroupLiveCrashRecovery(t)
	})
	assertLiveCgroupRoot(t, root, os.Getpid())

	payload, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SYNARA_CGROUP_V2_LIVE_EVIDENCE=%s", payload)
}

func testProtectedCgroupLiveRuntime(t *testing.T, root string) protectedCgroupLiveProof {
	t.Helper()
	identities := liveProtectedCgroupIdentities()
	supervisorInstance := uuid.New()
	lease := acquireLiveRootLease(t, root, identities, supervisorInstance)
	defer lease.Close()
	if err := lease.RecoverOrphans(); err != nil {
		t.Fatalf("initial live recovery: %v", err)
	}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 101, WorkerIncarnation: uuid.New()}
	runtime, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: identities[0], ProviderIdentity: identities[1], Fence: fence,
		ProviderLimits:     liveProtectedCgroupResourceLimits(),
		SupervisorInstance: supervisorInstance, RuntimeInstance: uuid.New(), RootLease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := runtime.Paths()
	observedLimits := assertLiveProviderLimits(t, paths.ProviderPath, liveProtectedCgroupResourceLimits())
	provider, descendantPID := startLiveProviderTree(t, runtime, fence)
	defer reapLiveCommand(provider)
	assertPIDInCgroup(t, provider.Process.Pid, paths.ProviderPath)
	assertPIDInCgroup(t, descendantPID, paths.ProviderPath)
	assertCgroupPopulated(t, paths.BundlePath, true)

	preflight, err := buildProtectedCgroupPreflightReport(
		withProtectedCgroupRootLease(context.Background(), lease),
		Config{
			CgroupV2Root:             root,
			CgroupV2ProviderIdentity: &identities[1],
			CgroupV2ProviderLimits: func() *ProtectedCgroupResourceLimits {
				limits := liveProtectedCgroupResourceLimits()
				return &limits
			}(),
			InstanceUID:    uuid.NewString(),
			RequestTimeout: 10 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("standalone live preflight beside active runtime: %v", err)
	}
	if !preflight.Enabled || !preflight.UseCgroupFD || !preflight.SetsidDescendantKilled ||
		preflight.ProviderUID != identities[1].UID || preflight.ProviderGID != identities[1].GID ||
		!preflight.ResourceLimitsApplied || preflight.ProviderLimits == nil ||
		*preflight.ProviderLimits != liveProtectedCgroupResourceLimits() {
		t.Fatalf("standalone live preflight report = %#v", preflight)
	}
	assertPIDAlive(t, provider.Process.Pid)
	assertPIDAlive(t, descendantPID)
	assertPIDInCgroup(t, provider.Process.Pid, paths.ProviderPath)
	assertPIDInCgroup(t, descendantPID, paths.ProviderPath)

	if overlap, overlapErr := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: identities[0], ProviderIdentity: identities[1], SupervisorInstance: uuid.New(),
	}); overlapErr == nil {
		_ = overlap.Close()
		t.Fatal("second live daemon root lease unexpectedly succeeded")
	} else if !strings.Contains(overlapErr.Error(), "already leased") {
		t.Fatalf("second live daemon root lease error = %v", overlapErr)
	}
	if duplicate, duplicateErr := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: identities[0], ProviderIdentity: identities[1], Fence: fence,
		ProviderLimits:     liveProtectedCgroupResourceLimits(),
		SupervisorInstance: supervisorInstance, RuntimeInstance: uuid.New(), RootLease: lease,
	}); duplicateErr == nil {
		_ = duplicate.Cleanup(fence)
		t.Fatal("same-fence live runtime unexpectedly succeeded")
	} else if !strings.Contains(duplicateErr.Error(), "already active") {
		t.Fatalf("same-fence live runtime error = %v", duplicateErr)
	}
	wrongFence := fence
	wrongFence.Generation++
	if err := runtime.AttachProviderPID(wrongFence, os.Getpid()); err == nil || !strings.Contains(err.Error(), "fence mismatch") {
		t.Fatalf("same-runtime wrong-fence attach error = %v", err)
	}
	assertPIDInCgroup(t, os.Getpid(), filepath.Join(root, protectedCgroupSupervisorSubgroup))

	if err := runtime.Cleanup(fence); err != nil {
		t.Fatalf("real cgroup.kill cleanup: %v", err)
	}
	waitForPIDGone(t, provider.Process.Pid)
	waitForPIDGone(t, descendantPID)
	assertPathGone(t, paths.BundlePath)
	return protectedCgroupLiveProof{
		Status: "pass", Mutation: "real cgroup.kill removed the active bundle", Bundle: filepath.Base(paths.BundlePath),
		ProviderPID: provider.Process.Pid, DescendantPID: descendantPID,
		Detail:         "standalone live preflight proved UseCgroupFD, uid/gid drop, exact finite pids/memory/cpu limits, and setsid kill while preserving the runtime; overlap and same-fence activation failed closed",
		ProviderLimits: &observedLimits,
	}
}

func testProtectedCgroupLiveParentExtraPID(t *testing.T, root string) protectedCgroupLiveProof {
	t.Helper()
	fixture, process := createPopulatedLiveOrphan(t, root, 201)
	defer cleanupLiveRawBundle(fixture, process)
	extra := exec.Command("/usr/bin/sleep", "300")
	if err := extra.Start(); err != nil {
		t.Fatal(err)
	}
	defer reapLiveCommand(extra)
	assertPIDInCgroup(t, extra.Process.Pid, filepath.Join(root, protectedCgroupSupervisorSubgroup))
	identities := liveProtectedCgroupIdentities()
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: identities[0], ProviderIdentity: identities[1], SupervisorInstance: uuid.New(),
	})
	if err == nil {
		_ = lease.Close()
		t.Fatal("root lease accepted an extra supervisor-subgroup PID")
	}
	if !strings.Contains(err.Error(), "must contain only current agentd pid") {
		t.Fatalf("extra supervisor-subgroup PID error = %v", err)
	}
	assertPIDAlive(t, process.Process.Pid)
	assertCgroupPopulated(t, fixture.BundlePath, true)
	return protectedCgroupLiveProof{
		Status: "pass", Mutation: "zero", Bundle: filepath.Base(fixture.BundlePath), ProviderPID: process.Process.Pid,
		Detail: "extra supervisor-subgroup PID rejected before recovery and the populated v3 fixture remained alive",
	}
}

func testProtectedCgroupLiveLegacy(t *testing.T, root string) protectedCgroupLiveProof {
	t.Helper()
	fixture, process := createPopulatedLiveOrphan(t, root, 301)
	defer cleanupLiveRawBundle(fixture, process)
	legacyPath := filepath.Join(root, fmt.Sprintf("synara-g302-i%s", compactLiveUUID(uuid.New())))
	for _, path := range []string{legacyPath, filepath.Join(legacyPath, "agentd"), filepath.Join(legacyPath, "provider")} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	defer removeEmptyLiveBundle(legacyPath)
	identities := liveProtectedCgroupIdentities()
	lease := acquireLiveRootLease(t, root, identities, uuid.New())
	defer lease.Close()
	if err := lease.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "manual cleanup") {
		t.Fatalf("legacy v1 recovery error = %v", err)
	}
	assertPIDAlive(t, process.Process.Pid)
	assertCgroupPopulated(t, fixture.BundlePath, true)
	return protectedCgroupLiveProof{
		Status: "pass", Mutation: "zero", Bundle: filepath.Base(fixture.BundlePath), ProviderPID: process.Process.Pid,
		Detail: "legacy v1 entry rejected the complete recovery set before the live v3 fixture was killed",
	}
}

func testProtectedCgroupLiveUnknownChild(t *testing.T, root string) protectedCgroupLiveProof {
	t.Helper()
	fixture, process := createPopulatedLiveOrphan(t, root, 401)
	defer reapLiveCommand(process)
	unknown := filepath.Join(fixture.BundlePath, "unknown")
	if err := os.Mkdir(unknown, 0o750); err != nil {
		t.Fatal(err)
	}
	identities := liveProtectedCgroupIdentities()
	first := acquireLiveRootLease(t, root, identities, uuid.New())
	if err := first.RecoverOrphans(); err == nil || !strings.Contains(err.Error(), "unknown child directory") {
		_ = first.Close()
		cleanupLiveRawBundle(fixture, process)
		t.Fatalf("unknown-child recovery error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	assertPIDAlive(t, process.Process.Pid)
	assertCgroupPopulated(t, fixture.BundlePath, true)
	if err := os.Remove(unknown); err != nil {
		t.Fatalf("repair exact unknown fixture: %v", err)
	}
	second := acquireLiveRootLease(t, root, identities, uuid.New())
	defer second.Close()
	if err := second.RecoverOrphans(); err != nil {
		t.Fatalf("fresh daemon recovery after exact fixture repair: %v", err)
	}
	waitForPIDGone(t, process.Process.Pid)
	assertPathGone(t, fixture.BundlePath)
	return protectedCgroupLiveProof{
		Status: "pass", Mutation: "zero before repair; real cgroup.kill after repair", Bundle: filepath.Base(fixture.BundlePath),
		ProviderPID: process.Process.Pid,
		Detail:      "unknown child failed closed; removing only that empty fixture let a fresh lease recover the orphan",
	}
}

func testProtectedCgroupLiveCrashRecovery(t *testing.T) protectedCgroupLiveProof {
	t.Helper()
	binary := filepath.Clean(os.Getenv(protectedCgroupLiveBinaryEnv))
	stateDir := filepath.Clean(os.Getenv(protectedCgroupLiveStateDirEnv))
	unit := os.Getenv(protectedCgroupLiveHelperUnitEnv)
	if !strings.HasPrefix(binary, "/opt/synara-cgroup-v2-live/") ||
		!strings.HasPrefix(stateDir, "/run/synara-cgroup-v2-live-") ||
		!validLiveUnitName(unit) {
		t.Fatalf("unsafe crash-recovery inputs binary=%q stateDir=%q unit=%q", binary, stateDir, unit)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join("/run/systemd/system", unit)
	root := filepath.Join("/sys/fs/cgroup/system.slice", unit)
	unitPayload := fmt.Sprintf(`[Unit]
Description=Synara protected cgroup v2 live crash fixture

[Service]
Type=exec
ExecStart=%s -test.run=^TestProtectedCgroupV3LiveHelper$ -test.v -test.timeout=90s
Environment=%s=1
Environment=%s=crash-holder
Environment=%s=%s
Environment=%s=%s
Delegate=yes
DelegateSubgroup=%s
KillMode=process
Restart=no
TimeoutStopSec=2s
	`, binary, protectedCgroupLiveEnabledEnv, protectedCgroupLiveHelperModeEnv,
		protectedCgroupLiveRootEnv, root, protectedCgroupLiveStateDirEnv, stateDir,
		protectedCgroupSupervisorSubgroup)
	if err := os.WriteFile(unitPath, []byte(unitPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	defer cleanupLiveSystemdUnit(unit, unitPath)
	runSystemctl(t, 10*time.Second, "daemon-reload")
	runSystemctl(t, 10*time.Second, "start", unit)
	recordPath := filepath.Join(stateDir, "crash-created.json")
	waitForFile(t, recordPath)
	record := readLiveCrashRecord(t, recordPath)
	assertPIDAlive(t, record.SurvivorPID)
	assertPIDInCgroup(t, record.SurvivorPID, filepath.Join(record.BundlePath, "provider"))
	mainPID := readSystemdMainPID(t, unit)
	if err := unix.Kill(mainPID, unix.SIGKILL); err != nil {
		t.Fatalf("SIGKILL lease holder: %v", err)
	}
	waitForSystemdState(t, unit, "failed")
	assertPIDAlive(t, record.SurvivorPID)
	assertCgroupPopulated(t, record.BundlePath, true)
	runSystemctl(t, 10*time.Second, "reset-failed", unit)
	runSystemctl(t, 10*time.Second, "start", unit)
	recoveredPath := filepath.Join(stateDir, "crash-recovered.json")
	waitForFile(t, recoveredPath)
	waitForSystemdState(t, unit, "inactive")
	waitForPIDGone(t, record.SurvivorPID)
	assertPathGone(t, record.BundlePath)
	return protectedCgroupLiveProof{
		Status: "pass", Mutation: "fresh holder used real cgroup.kill/events", Bundle: filepath.Base(record.BundlePath),
		ProviderPID: record.SurvivorPID,
		Detail:      "SIGKILL released flock; KillMode=process preserved a non-Pdeathsig descendant until same-service recovery",
	}
}

// TestProtectedCgroupV3LiveHelper is selected only by the live lane's exact
// helper command. It supports a Provider process tree and a restartable
// systemd crash holder without adding production-only hooks.
func TestProtectedCgroupV3LiveHelper(t *testing.T) {
	if os.Getenv(protectedCgroupLiveEnabledEnv) != "1" {
		t.Skip("live helper")
	}
	switch os.Getenv(protectedCgroupLiveHelperModeEnv) {
	case "provider":
		gate := os.NewFile(3, "live-provider-gate")
		if gate == nil {
			t.Fatal("provider gate fd 3 unavailable")
		}
		var signal [1]byte
		if _, err := gate.Read(signal[:]); err != nil {
			t.Fatal(err)
		}
		_ = gate.Close()
		descendant := exec.Command("/usr/bin/sleep", "300")
		descendant.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: 0}
		if err := descendant.Start(); err != nil {
			t.Fatal(err)
		}
		stateDir := os.Getenv(protectedCgroupLiveStateDirEnv)
		if err := os.WriteFile(filepath.Join(stateDir, "provider-descendant.pid"), []byte(strconv.Itoa(descendant.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "provider-ready"), []byte("ready\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = descendant.Wait()
	case "crash-holder":
		runProtectedCgroupLiveCrashHolder(t)
	default:
		t.Fatalf("unknown live helper mode %q", os.Getenv(protectedCgroupLiveHelperModeEnv))
	}
}

func runProtectedCgroupLiveCrashHolder(t *testing.T) {
	t.Helper()
	root := filepath.Clean(os.Getenv(protectedCgroupLiveRootEnv))
	stateDir := filepath.Clean(os.Getenv(protectedCgroupLiveStateDirEnv))
	assertLiveCgroupRoot(t, root, os.Getpid())
	identities := liveProtectedCgroupIdentities()
	lease := acquireLiveRootLease(t, root, identities, uuid.New())
	defer lease.Close()
	recordPath := filepath.Join(stateDir, "crash-created.json")
	if _, err := os.Stat(recordPath); err == nil {
		record := readLiveCrashRecord(t, recordPath)
		if err := lease.RecoverOrphans(); err != nil {
			t.Fatalf("recover crash orphan: %v", err)
		}
		waitForPIDGone(t, record.SurvivorPID)
		assertPathGone(t, record.BundlePath)
		if err := os.WriteFile(filepath.Join(stateDir, "crash-recovered.json"), []byte("{\"status\":\"pass\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := lease.RecoverOrphans(); err != nil {
		t.Fatal(err)
	}
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: 501, WorkerIncarnation: uuid.New()}
	supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath: root, SupervisorIdentity: identities[0], ProviderIdentity: identities[1], Fence: fence,
		ProviderLimits:     liveProtectedCgroupResourceLimits(),
		SupervisorInstance: lease.supervisorInstance, RuntimeInstance: uuid.New(), RootLease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	survivor := exec.Command("/usr/bin/sleep", "300")
	survivor.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: 0}
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.AttachProviderPID(fence, survivor.Process.Pid); err != nil {
		_ = survivor.Process.Kill()
		t.Fatal(err)
	}
	record := protectedCgroupLiveCrashRecord{BundlePath: supervisor.Paths().BundlePath, SurvivorPID: survivor.Process.Pid}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func liveProtectedCgroupIdentities() [2]ProtectedCgroupIdentity {
	return [2]ProtectedCgroupIdentity{{UID: 0, GID: 0}, {UID: 65534, GID: 65534}}
}

func liveProtectedCgroupResourceLimits() ProtectedCgroupResourceLimits {
	return ProtectedCgroupResourceLimits{
		PidsMax: 128, MemoryMaxBytes: 512 << 20, CPUQuotaMicros: 200_000, CPUPeriodMicros: 100_000,
	}
}

func assertLiveProviderLimits(
	t *testing.T,
	providerPath string,
	limits ProtectedCgroupResourceLimits,
) ProtectedCgroupResourceLimits {
	t.Helper()
	want := map[string]string{
		"pids.max":   strconv.FormatUint(limits.PidsMax, 10),
		"memory.max": strconv.FormatUint(limits.MemoryMaxBytes, 10),
		"cpu.max":    fmt.Sprintf("%d %d", limits.CPUQuotaMicros, limits.CPUPeriodMicros),
	}
	for _, name := range []string{"pids.max", "memory.max", "cpu.max"} {
		data, err := os.ReadFile(filepath.Join(providerPath, name))
		if err != nil {
			t.Fatal(err)
		}
		if actual := strings.Join(strings.Fields(string(data)), " "); actual != want[name] {
			t.Fatalf("live Provider %s = %q, want %q", name, actual, want[name])
		}
	}
	return limits
}

func acquireLiveRootLease(t *testing.T, root string, identities [2]ProtectedCgroupIdentity, instance uuid.UUID) *ProtectedCgroupRootLease {
	t.Helper()
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath: root, SupervisorIdentity: identities[0], ProviderIdentity: identities[1], SupervisorInstance: instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func startLiveProviderTree(t *testing.T, supervisor *ProtectedCgroupSupervisor, fence ProtectedCgroupFence) (*exec.Cmd, int) {
	t.Helper()
	stateDir, err := os.MkdirTemp("/run", "synara-cgroup-v2-live-provider-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	readGate, writeGate, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProtectedCgroupV3LiveHelper$", "-test.v", "-test.timeout=60s")
	command.Env = append(os.Environ(),
		protectedCgroupLiveEnabledEnv+"=1",
		protectedCgroupLiveHelperModeEnv+"=provider",
		protectedCgroupLiveStateDirEnv+"="+stateDir,
	)
	command.ExtraFiles = []*os.File{readGate}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readGate.Close()
	if err := supervisor.AttachProviderPID(fence, command.Process.Pid); err != nil {
		_ = command.Process.Kill()
		_ = writeGate.Close()
		t.Fatal(err)
	}
	if _, err := writeGate.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = writeGate.Close()
	waitForFile(t, filepath.Join(stateDir, "provider-ready"))
	descendantPID := readPIDFile(t, filepath.Join(stateDir, "provider-descendant.pid"))
	return command, descendantPID
}

func createPopulatedLiveOrphan(t *testing.T, root string, generation int64) (ProtectedCgroupPaths, *exec.Cmd) {
	t.Helper()
	fence := ProtectedCgroupFence{ExecutionID: uuid.New(), Generation: generation, WorkerIncarnation: uuid.New()}
	bundle := filepath.Join(root, protectedCgroupBundleName(fence, uuid.New(), uuid.New()))
	paths := ProtectedCgroupPaths{ParentPath: root, BundlePath: bundle, AgentdPath: filepath.Join(bundle, "agentd"), ProviderPath: filepath.Join(bundle, "provider")}
	for _, path := range []string{paths.BundlePath, paths.AgentdPath, paths.ProviderPath} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	process := exec.Command("/usr/bin/sleep", "300")
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: 0}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ProviderPath, "cgroup.procs"), []byte(strconv.Itoa(process.Process.Pid)), 0o600); err != nil {
		_ = process.Process.Kill()
		t.Fatal(err)
	}
	assertPIDInCgroup(t, process.Process.Pid, paths.ProviderPath)
	assertCgroupPopulated(t, paths.BundlePath, true)
	return paths, process
}

func cleanupLiveRawBundle(paths ProtectedCgroupPaths, process *exec.Cmd) {
	if process != nil && process.Process != nil {
		_ = os.WriteFile(filepath.Join(paths.BundlePath, "cgroup.kill"), []byte("1"), 0o600)
		_, _ = process.Process.Wait()
	}
	for _, path := range []string{paths.ProviderPath, paths.AgentdPath, paths.BundlePath} {
		_ = os.Remove(path)
	}
}

func removeEmptyLiveBundle(bundle string) {
	_ = os.Remove(filepath.Join(bundle, "provider"))
	_ = os.Remove(filepath.Join(bundle, "agentd"))
	_ = os.Remove(bundle)
}

func assertLiveCgroupRoot(t *testing.T, root string, pid int) {
	t.Helper()
	var stats unix.Statfs_t
	if err := unix.Statfs(root, &stats); err != nil {
		t.Fatal(err)
	}
	if uint64(stats.Type) != linuxCgroup2SuperMagic {
		t.Fatalf("%s is not cgroup2", root)
	}
	supervisorPath := filepath.Join(root, protectedCgroupSupervisorSubgroup)
	assertPIDInCgroup(t, pid, supervisorPath)
	data, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(data)); len(fields) != 0 {
		t.Fatalf("live root cgroup.procs = %q, want process-free delegated root", fields)
	}
	data, err = os.ReadFile(filepath.Join(supervisorPath, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 1 || fields[0] != strconv.Itoa(pid) {
		t.Fatalf("live supervisor subgroup cgroup.procs = %q, want only %d", fields, pid)
	}
}

func assertLiveSystemdService(t *testing.T, root string, requireControllers ...bool) protectedCgroupLiveSystemdProof {
	t.Helper()
	unit := filepath.Base(root)
	command := exec.Command(
		"systemctl", "show", unit,
		"--property=Delegate", "--property=DelegateSubgroup", "--property=KillMode", "--property=ControlGroup",
		"--property=MainPID", "--property=ActiveState", "--property=SubState",
	)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("inspect live systemd service: %v", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}
	wantControlGroup := "/system.slice/" + unit
	if values["Delegate"] != "yes" || values["DelegateSubgroup"] != protectedCgroupSupervisorSubgroup ||
		values["KillMode"] != "process" ||
		values["ControlGroup"] != wantControlGroup || values["MainPID"] != strconv.Itoa(os.Getpid()) ||
		values["ActiveState"] != "active" || values["SubState"] != "running" {
		t.Fatalf("live systemd service properties = %#v", values)
	}
	parentPIDs := readLiveCgroupPIDs(t, root)
	if len(parentPIDs) != 0 {
		t.Fatalf("live delegated root contains processes: %v", parentPIDs)
	}
	supervisorPIDs := readLiveCgroupPIDs(t, filepath.Join(root, protectedCgroupSupervisorSubgroup))
	if len(supervisorPIDs) != 1 || supervisorPIDs[0] != os.Getpid() {
		t.Fatalf("live supervisor subgroup processes = %v, want only %d", supervisorPIDs, os.Getpid())
	}
	controllerData, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		t.Fatal(err)
	}
	controllers := strings.Fields(string(controllerData))
	slices.Sort(controllers)
	controllerSet := make(map[string]struct{}, len(controllers))
	for _, controller := range controllers {
		controllerSet[controller] = struct{}{}
	}
	if len(requireControllers) > 0 && requireControllers[0] {
		for _, required := range protectedCgroupRequiredControllers {
			if _, found := controllerSet[required]; !found {
				t.Fatalf("live delegated root enabled controllers = %v, missing %s", controllers, required)
			}
		}
	}
	return protectedCgroupLiveSystemdProof{
		Unit: unit, Delegate: values["Delegate"], DelegateSubgroup: values["DelegateSubgroup"], KillMode: values["KillMode"],
		ControlGroup: values["ControlGroup"], ActiveState: values["ActiveState"], SubState: values["SubState"],
		ParentProcessCount: len(parentPIDs), SupervisorPIDs: supervisorPIDs, EnabledControllers: controllers,
	}
}

func readLiveCgroupPIDs(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	result := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil || pid <= 0 {
			t.Fatalf("invalid PID %q in %s/cgroup.procs", field, path)
		}
		result = append(result, pid)
	}
	return result
}

func assertPIDInCgroup(t *testing.T, pid int, expected string) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	_, relative, found := strings.Cut(line, "0::")
	if !found {
		t.Fatalf("pid %d has non-unified cgroup %q", pid, line)
	}
	actual := filepath.Join("/sys/fs/cgroup", relative)
	if filepath.Clean(actual) != filepath.Clean(expected) {
		t.Fatalf("pid %d cgroup = %q, want %q", pid, actual, expected)
	}
}

func assertCgroupPopulated(t *testing.T, path string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err == nil {
			populated := strings.Contains(string(data), "populated 1")
			if populated == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cgroup %s populated did not become %v", path, want)
}

func assertPIDAlive(t *testing.T, pid int) {
	t.Helper()
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("pid %d is not alive: %v", pid, err)
	}
}

func waitForPIDGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 2 && fields[2] == "Z" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d survived cgroup cleanup", pid)
}

func assertPathGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s survived: %v", path, err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid pid file %s: %q", path, data)
	}
	return pid
}

func readLiveCrashRecord(t *testing.T, path string) protectedCgroupLiveCrashRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record protectedCgroupLiveCrashRecord
	if err := json.Unmarshal(data, &record); err != nil || record.SurvivorPID <= 0 || record.BundlePath == "" {
		t.Fatalf("invalid crash record %s: %v", path, err)
	}
	return record
}

func readSystemdMainPID(t *testing.T, unit string) int {
	t.Helper()
	command := exec.Command("systemctl", "show", unit, "--property=MainPID", "--value")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid MainPID for %s: %q", unit, output)
	}
	return pid
}

func waitForSystemdState(t *testing.T, unit, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		command := exec.Command("systemctl", "show", unit, "--property=ActiveState", "--value")
		if output, err := command.Output(); err == nil && strings.TrimSpace(string(output)) == want {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("unit %s did not reach %s", unit, want)
}

func runSystemctl(t *testing.T, timeout time.Duration, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "systemctl", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("systemctl %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func cleanupLiveSystemdUnit(unit, unitPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", "kill", "--kill-who=all", "--signal=KILL", unit).Run()
	_ = exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", unit).Run()
	_ = os.Remove(unitPath)
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
}

func reapLiveCommand(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Kill()
	_, _ = command.Process.Wait()
}

func compactLiveUUID(value uuid.UUID) string {
	return strings.ReplaceAll(value.String(), "-", "")
}

func validLiveUnitName(value string) bool {
	return strings.HasPrefix(value, "synara-cgroup-v2-live-") && strings.HasSuffix(value, ".service") &&
		!strings.ContainsAny(value, "/\\\r\n\t ")
}

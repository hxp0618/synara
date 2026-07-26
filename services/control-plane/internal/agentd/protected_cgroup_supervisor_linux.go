//go:build linux

package agentd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	protectedCgroupBundlePrefix       = "synara-e"
	protectedCgroupDiagnosticPrefix   = "synara-d"
	protectedCgroupLegacyBundlePrefix = "synara-g"
)

var (
	protectedCgroupFstatfs   = unix.Fstatfs
	protectedCgroupMountInfo = func() ([]byte, error) {
		return os.ReadFile("/proc/self/mountinfo")
	}
	protectedCgroupCleanupTestHook           = func(_ int, _ string) error { return nil }
	protectedCgroupRecoveryValidatedTestHook = func() error { return nil }
	protectedCgroupCurrentPID                = os.Getpid
)

var protectedCgroupCreationReservations = struct {
	sync.Mutex
	active map[string]struct{}
}{active: make(map[string]struct{})}

// ProtectedCgroupRootLease is an open-file-description flock on the delegated
// parent directory. The kernel releases it on daemon exit, including crashes.
// Keeping the directory fd open also pins the exact validated parent inode.
type ProtectedCgroupRootLease struct {
	mu sync.Mutex

	parentFD           int
	parentPath         string
	parentDevice       uint64
	parentInode        uint64
	supervisorIdentity ProtectedCgroupIdentity
	providerIdentity   ProtectedCgroupIdentity
	supervisorInstance uuid.UUID
	closed             bool
	recoveryComplete   bool
	recoveryMutating   bool
	runtimeAuthorized  bool
}

// ProtectedCgroupSupervisor owns a fenced cgroup-v2 subtree for a privileged
// agentd supervisor and one untrusted Provider process tree.
type ProtectedCgroupSupervisor struct {
	parentFD   int
	bundleFD   int
	agentdFD   int
	providerFD int

	paths               ProtectedCgroupPaths
	fence               ProtectedCgroupFence
	creationReservation string
	cleanupPoisoned     bool
}

func acquireProtectedCgroupDaemonRootLease(
	cfg Config,
	supervisorInstance uuid.UUID,
) (*ProtectedCgroupRootLease, error) {
	if strings.TrimSpace(cfg.CgroupV2Root) == "" || cfg.CgroupV2ProviderIdentity == nil {
		return nil, nil
	}
	lease, err := AcquireProtectedCgroupRootLease(ProtectedCgroupRootLeaseConfig{
		ParentPath:         cfg.CgroupV2Root,
		SupervisorIdentity: currentProtectedCgroupSupervisorIdentity(),
		ProviderIdentity:   *cfg.CgroupV2ProviderIdentity,
		SupervisorInstance: supervisorInstance,
	})
	if err != nil {
		return nil, err
	}
	if err := lease.RecoverOrphans(); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}

func AcquireProtectedCgroupRootLease(config ProtectedCgroupRootLeaseConfig) (*ProtectedCgroupRootLease, error) {
	if strings.TrimSpace(config.ParentPath) == "" {
		return nil, errors.New("protected cgroup root lease parent path is required")
	}
	if config.SupervisorInstance == uuid.Nil {
		return nil, errors.New("protected cgroup root lease supervisor instance is required")
	}
	if err := validateProtectedCgroupIdentityBoundary(config.SupervisorIdentity, config.ProviderIdentity); err != nil {
		return nil, err
	}
	parentFD, normalizedParent, err := openDirectoryNoSymlinks(config.ParentPath)
	if err != nil {
		return nil, fmt.Errorf("open protected cgroup parent for root lease: %w", err)
	}
	closeParent := true
	defer func() {
		if closeParent {
			_ = unix.Close(parentFD)
		}
	}()
	if err := validateProtectedCgroupParent(parentFD, normalizedParent, ProtectedCgroupSupervisorConfig{
		SupervisorIdentity: config.SupervisorIdentity,
	}); err != nil {
		return nil, err
	}
	if err := unix.Flock(parentFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("protected cgroup parent %s is already leased by another agentd", normalizedParent)
		}
		return nil, fmt.Errorf("lock protected cgroup parent %s: %w", normalizedParent, err)
	}
	var stats unix.Stat_t
	if err := unix.Fstat(parentFD, &stats); err != nil {
		_ = unix.Flock(parentFD, unix.LOCK_UN)
		return nil, fmt.Errorf("stat protected cgroup root lease %s: %w", normalizedParent, err)
	}
	if err := validateProtectedCgroupParentProcesses(parentFD, normalizedParent); err != nil {
		_ = unix.Flock(parentFD, unix.LOCK_UN)
		return nil, err
	}
	closeParent = false
	return &ProtectedCgroupRootLease{
		parentFD: parentFD, parentPath: normalizedParent,
		parentDevice: uint64(stats.Dev), parentInode: stats.Ino,
		supervisorIdentity: config.SupervisorIdentity,
		providerIdentity:   config.ProviderIdentity,
		supervisorInstance: config.SupervisorInstance,
	}, nil
}

func (l *ProtectedCgroupRootLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := errors.Join(unix.Flock(l.parentFD, unix.LOCK_UN), closeIfOpen(l.parentFD))
	l.parentFD = -1
	return err
}

func NewProtectedCgroupSupervisor(config ProtectedCgroupSupervisorConfig) (*ProtectedCgroupSupervisor, error) {
	if err := validateProtectedCgroupSupervisorConfig(config); err != nil {
		return nil, err
	}

	parentFD, normalizedParent, err := openDirectoryNoSymlinks(config.ParentPath)
	if err != nil {
		return nil, fmt.Errorf("open protected cgroup parent: %w", err)
	}
	cleanupParent := true
	defer func() {
		if cleanupParent {
			_ = unix.Close(parentFD)
		}
	}()
	if err := validateProtectedCgroupParent(parentFD, normalizedParent, config); err != nil {
		return nil, err
	}
	if config.RootLease != nil {
		if err := config.RootLease.authorizeConstruction(parentFD, normalizedParent, config); err != nil {
			return nil, err
		}
	}

	creationReservation := protectedCgroupCreationReservationKey(normalizedParent, config)
	if err := reserveProtectedCgroupCreation(creationReservation); err != nil {
		return nil, err
	}
	releaseReservation := true
	defer func() {
		if releaseReservation {
			releaseProtectedCgroupCreation(creationReservation)
		}
	}()

	bundleName := protectedCgroupBundleNameForConfig(config)
	bundleFD, bundlePath, err := createProtectedCgroupDirectory(
		parentFD,
		normalizedParent,
		bundleName,
		config.SupervisorIdentity,
	)
	if err != nil {
		return nil, err
	}
	cleanupBundle := true
	defer func() {
		if cleanupBundle {
			_ = cleanupProtectedCgroupDirectory(bundleFD, parentFD, normalizedParent, bundleName)
		}
	}()

	agentdFD, agentdPath, err := createProtectedCgroupDirectory(
		bundleFD,
		bundlePath,
		"agentd",
		config.SupervisorIdentity,
	)
	if err != nil {
		return nil, err
	}
	cleanupAgentd := true
	defer func() {
		if cleanupAgentd {
			_ = cleanupProtectedCgroupDirectory(agentdFD, bundleFD, bundlePath, "agentd")
		}
	}()

	providerFD, providerPath, err := createProtectedCgroupDirectory(
		bundleFD,
		bundlePath,
		"provider",
		config.SupervisorIdentity,
	)
	if err != nil {
		return nil, err
	}
	cleanupProvider := true
	defer func() {
		if cleanupProvider {
			_ = cleanupProtectedCgroupDirectory(providerFD, bundleFD, bundlePath, "provider")
		}
	}()

	cleanupParent = false
	cleanupBundle = false
	cleanupAgentd = false
	cleanupProvider = false
	releaseReservation = false
	return &ProtectedCgroupSupervisor{
		parentFD: parentFD, bundleFD: bundleFD, agentdFD: agentdFD, providerFD: providerFD,
		paths: ProtectedCgroupPaths{
			ParentPath: normalizedParent, BundlePath: bundlePath, AgentdPath: agentdPath, ProviderPath: providerPath,
		},
		fence:               config.Fence,
		creationReservation: creationReservation,
	}, nil
}

func (s *ProtectedCgroupSupervisor) Paths() ProtectedCgroupPaths {
	if s == nil {
		return ProtectedCgroupPaths{}
	}
	return s.paths
}

func (s *ProtectedCgroupSupervisor) Fence() ProtectedCgroupFence {
	if s == nil {
		return ProtectedCgroupFence{}
	}
	return s.fence
}

func (s *ProtectedCgroupSupervisor) AttachAgentdPID(expected ProtectedCgroupFence, pid int) error {
	return s.attachPID("agentd", s.agentdFD, s.paths.AgentdPath, expected, pid)
}

func (s *ProtectedCgroupSupervisor) AttachProviderPID(expected ProtectedCgroupFence, pid int) error {
	return s.attachPID("provider", s.providerFD, s.paths.ProviderPath, expected, pid)
}

func (s *ProtectedCgroupSupervisor) Cleanup(expected ProtectedCgroupFence) error {
	if s == nil {
		return errors.New("protected cgroup supervisor is nil")
	}
	if err := s.assertFence(expected); err != nil {
		return err
	}
	if s.cleanupPoisoned {
		return newContainmentError("cleanup", errors.New("protected cgroup cleanup previously failed; execution fence remains poisoned"))
	}
	var result error
	if s.providerFD >= 0 {
		result = errors.Join(
			result,
			cleanupProtectedCgroupDirectory(s.providerFD, s.bundleFD, s.paths.BundlePath, "provider"),
		)
		s.providerFD = -1
	}
	if s.agentdFD >= 0 {
		result = errors.Join(
			result,
			cleanupProtectedCgroupDirectory(s.agentdFD, s.bundleFD, s.paths.BundlePath, "agentd"),
		)
		s.agentdFD = -1
	}
	if s.bundleFD >= 0 {
		result = errors.Join(
			result,
			cleanupProtectedCgroupDirectory(
				s.bundleFD,
				s.parentFD,
				s.paths.ParentPath,
				filepath.Base(s.paths.BundlePath),
			),
		)
		s.bundleFD = -1
	}
	if s.parentFD >= 0 {
		result = errors.Join(result, closeIfOpen(s.parentFD))
		s.parentFD = -1
	}
	if result == nil && s.creationReservation != "" {
		releaseProtectedCgroupCreation(s.creationReservation)
		s.creationReservation = ""
	}
	if result != nil {
		s.cleanupPoisoned = true
	}
	return newContainmentError("cleanup", result)
}

func (s *ProtectedCgroupSupervisor) attachPID(role string, directoryFD int, directoryPath string, expected ProtectedCgroupFence, pid int) error {
	if s == nil {
		return errors.New("protected cgroup supervisor is nil")
	}
	if err := s.assertFence(expected); err != nil {
		return err
	}
	if directoryFD < 0 {
		return fmt.Errorf("protected %s cgroup is already closed", role)
	}
	if pid <= 0 {
		return fmt.Errorf("protected %s PID must be positive", role)
	}
	fd, err := unix.Openat(directoryFD, "cgroup.procs", unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %s/cgroup.procs: %w", directoryPath, err)
	}
	defer unix.Close(fd)
	payload := []byte(fmt.Sprintf("%d", pid))
	for len(payload) > 0 {
		written, writeErr := unix.Write(fd, payload)
		if writeErr != nil {
			return fmt.Errorf("write %s/cgroup.procs: %w", directoryPath, writeErr)
		}
		payload = payload[written:]
	}
	return nil
}

func (s *ProtectedCgroupSupervisor) assertFence(expected ProtectedCgroupFence) error {
	if expected.ExecutionID != s.fence.ExecutionID {
		return fmt.Errorf(
			"protected cgroup execution fence mismatch: have %s want %s",
			s.fence.ExecutionID,
			expected.ExecutionID,
		)
	}
	if expected.Generation != s.fence.Generation {
		return fmt.Errorf("protected cgroup generation fence mismatch: have %d want %d", s.fence.Generation, expected.Generation)
	}
	if expected.WorkerIncarnation != s.fence.WorkerIncarnation {
		return fmt.Errorf(
			"protected cgroup incarnation fence mismatch: have %s want %s",
			s.fence.WorkerIncarnation,
			expected.WorkerIncarnation,
		)
	}
	return nil
}

func validateProtectedCgroupSupervisorConfig(config ProtectedCgroupSupervisorConfig) error {
	if strings.TrimSpace(config.ParentPath) == "" {
		return errors.New("protected cgroup parent path is required")
	}
	if config.Fence.ExecutionID == uuid.Nil {
		return errors.New("protected cgroup execution fence is required")
	}
	if config.Fence.Generation <= 0 {
		return errors.New("protected cgroup generation fence must be positive")
	}
	if config.Fence.WorkerIncarnation == uuid.Nil {
		return errors.New("protected cgroup worker incarnation fence is required")
	}
	if config.SupervisorInstance == uuid.Nil {
		return errors.New("protected cgroup supervisor instance is required")
	}
	if config.RuntimeInstance == uuid.Nil {
		return errors.New("protected cgroup runtime instance is required")
	}
	if !config.Diagnostic && config.RootLease == nil {
		return errors.New("protected cgroup runtime requires the daemon root lease")
	}
	if err := validateProtectedCgroupIdentityBoundary(config.SupervisorIdentity, config.ProviderIdentity); err != nil {
		return err
	}
	return nil
}

func validateProtectedCgroupIdentityBoundary(supervisor, provider ProtectedCgroupIdentity) error {
	if supervisor.UID == provider.UID {
		return errors.New("protected cgroup supervisor and provider must not share a UID")
	}
	return nil
}

func validateProtectedCgroupParent(parentFD int, parentPath string, config ProtectedCgroupSupervisorConfig) error {
	if err := validateProtectedCgroupFilesystem(parentFD, parentPath); err != nil {
		return err
	}
	if err := validateProtectedCgroupDirectoryOwnership(parentFD, parentPath, config.SupervisorIdentity); err != nil {
		return err
	}
	return nil
}

type parsedProtectedCgroupBundle struct {
	fence              ProtectedCgroupFence
	supervisorInstance uuid.UUID
	runtimeInstance    uuid.UUID
	legacy             bool
}

type protectedCgroupRecoveryBundle struct {
	name        string
	bundleFD    int
	childFDs    map[string]int
	controllers map[string]struct{}
}

func (l *ProtectedCgroupRootLease) authorizeConstruction(
	parentFD int,
	parentPath string,
	config ProtectedCgroupSupervisorConfig,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.parentFD < 0 {
		return errors.New("protected cgroup root lease is closed")
	}
	if parentPath != l.parentPath || config.SupervisorIdentity != l.supervisorIdentity ||
		config.ProviderIdentity != l.providerIdentity ||
		(!config.Diagnostic && config.SupervisorInstance != l.supervisorInstance) {
		return errors.New("protected cgroup runtime does not match the daemon root lease authority")
	}
	var stats unix.Stat_t
	if err := unix.Fstat(parentFD, &stats); err != nil {
		return fmt.Errorf("stat protected cgroup runtime parent %s: %w", parentPath, err)
	}
	if uint64(stats.Dev) != l.parentDevice || stats.Ino != l.parentInode {
		return errors.New("protected cgroup runtime parent inode does not match the daemon root lease")
	}
	if !l.recoveryComplete {
		return errors.New("protected cgroup daemon root lease has not completed startup recovery")
	}
	if !config.Diagnostic {
		l.runtimeAuthorized = true
	}
	return nil
}

// RecoverOrphans is the sole destructive startup-recovery entrypoint. The
// daemon must call it after acquiring the exclusive parent lease and before it
// runs preflight or registers the Worker.
func (l *ProtectedCgroupRootLease) RecoverOrphans() error {
	if l == nil {
		return errors.New("protected cgroup root lease is nil")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.parentFD < 0 {
		return errors.New("protected cgroup root lease is closed")
	}
	if l.recoveryComplete {
		return errors.New("protected cgroup startup recovery has already completed")
	}
	if l.recoveryMutating {
		return errors.New("protected cgroup startup recovery previously entered mutation and cannot be retried")
	}
	if l.runtimeAuthorized {
		return errors.New("protected cgroup startup recovery cannot run after runtime authorization")
	}
	if err := validateProtectedCgroupParentProcesses(l.parentFD, l.parentPath); err != nil {
		return err
	}
	plans, err := inspectProtectedCgroupRecoveryBundles(
		l.parentFD,
		l.parentPath,
		l.supervisorIdentity,
	)
	if err != nil {
		return err
	}
	defer closeProtectedCgroupRecoveryBundles(plans)
	if err := protectedCgroupRecoveryValidatedTestHook(); err != nil {
		return err
	}
	// Revalidate every held fd and directory entry after the test/race barrier
	// and before the first cgroup.kill. The exclusive lease prevents another
	// conforming daemon from mutating this parent during the operation.
	for _, plan := range plans {
		if err := revalidateProtectedCgroupRecoveryBundle(
			l.parentFD, l.parentPath, plan, l.supervisorIdentity,
		); err != nil {
			return err
		}
	}
	if err := revalidateProtectedCgroupRecoverySet(l.parentFD, l.parentPath, plans); err != nil {
		return err
	}
	if err := validateProtectedCgroupParentProcesses(l.parentFD, l.parentPath); err != nil {
		return err
	}
	l.recoveryMutating = true
	for _, plan := range plans {
		if err := cleanupProtectedCgroupRecoveryBundle(l.parentFD, l.parentPath, plan); err != nil {
			return fmt.Errorf("recover orphaned protected cgroup %s/%s: %w", l.parentPath, plan.name, err)
		}
	}
	l.recoveryMutating = false
	l.recoveryComplete = true
	return nil
}

func inspectProtectedCgroupRecoveryBundles(
	parentFD int,
	parentPath string,
	owner ProtectedCgroupIdentity,
) ([]*protectedCgroupRecoveryBundle, error) {
	parentControllers, err := readProtectedCgroupControllers(parentFD, parentPath)
	if err != nil {
		return nil, err
	}
	names, err := readProtectedCgroupDirectoryNames(parentFD, parentPath)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	plans := make([]*protectedCgroupRecoveryBundle, 0)
	for _, name := range names {
		parsed, recognized, parseErr := parseProtectedCgroupBundleName(name)
		if parseErr != nil {
			closeProtectedCgroupRecoveryBundles(plans)
			return nil, fmt.Errorf("parse protected cgroup bundle %s/%s: %w", parentPath, name, parseErr)
		}
		if !recognized {
			continue
		}
		if parsed.legacy {
			closeProtectedCgroupRecoveryBundles(plans)
			return nil, fmt.Errorf(
				"legacy protected cgroup %s/%s requires an explicit stopped-v1 migration and manual cleanup",
				parentPath,
				name,
			)
		}
		plan, inspectErr := inspectProtectedCgroupRecoveryBundle(parentFD, parentPath, name, owner, parentControllers)
		if inspectErr != nil {
			closeProtectedCgroupRecoveryBundles(plans)
			return nil, inspectErr
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func inspectProtectedCgroupRecoveryBundle(
	parentFD int,
	parentPath string,
	name string,
	owner ProtectedCgroupIdentity,
	parentControllers map[string]struct{},
) (*protectedCgroupRecoveryBundle, error) {
	bundleFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open protected cgroup bundle %s/%s: %w", parentPath, name, err)
	}
	plan := &protectedCgroupRecoveryBundle{name: name, bundleFD: bundleFD, childFDs: make(map[string]int)}
	bundlePath := filepath.Join(parentPath, name)
	if err := validateProtectedCgroupDirectoryOwnership(bundleFD, bundlePath, owner); err != nil {
		closeProtectedCgroupRecoveryBundles([]*protectedCgroupRecoveryBundle{plan})
		return nil, err
	}
	if err := validateProtectedCgroupNameStillReferencesFD(parentFD, name, bundleFD, bundlePath); err != nil {
		closeProtectedCgroupRecoveryBundles([]*protectedCgroupRecoveryBundle{plan})
		return nil, err
	}
	bundleControllers, err := readProtectedCgroupControllers(bundleFD, bundlePath)
	if err != nil {
		closeProtectedCgroupRecoveryBundles([]*protectedCgroupRecoveryBundle{plan})
		return nil, err
	}
	plan.controllers = mergeProtectedCgroupControllers(parentControllers, bundleControllers)
	if err := validateProtectedCgroupRecoveryBundle(parentFD, parentPath, plan, owner, true); err != nil {
		closeProtectedCgroupRecoveryBundles([]*protectedCgroupRecoveryBundle{plan})
		return nil, err
	}
	return plan, nil
}

func revalidateProtectedCgroupRecoverySet(
	parentFD int,
	parentPath string,
	plans []*protectedCgroupRecoveryBundle,
) error {
	expected := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		expected[plan.name] = struct{}{}
	}
	names, err := readProtectedCgroupDirectoryNames(parentFD, parentPath)
	if err != nil {
		return err
	}
	for _, name := range names {
		parsed, recognized, parseErr := parseProtectedCgroupBundleName(name)
		if parseErr != nil {
			return fmt.Errorf("revalidate protected cgroup bundle %s/%s: %w", parentPath, name, parseErr)
		}
		if !recognized {
			continue
		}
		if parsed.legacy {
			return fmt.Errorf(
				"legacy protected cgroup %s/%s appeared during recovery; stop v1 and clean it manually",
				parentPath,
				name,
			)
		}
		if _, found := expected[name]; !found {
			return fmt.Errorf("protected cgroup runtime %s/%s appeared during recovery", parentPath, name)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		return errors.New("protected cgroup recovery set changed before mutation")
	}
	return nil
}

func validateProtectedCgroupParentProcesses(parentFD int, parentPath string) error {
	fd, err := unix.Openat(parentFD, "cgroup.procs", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open protected cgroup parent %s/cgroup.procs: %w", parentPath, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parentPath, "cgroup.procs"))
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open protected cgroup parent process file %s/cgroup.procs", parentPath)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 1<<20))
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read protected cgroup parent %s/cgroup.procs: %w", parentPath, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close protected cgroup parent %s/cgroup.procs: %w", parentPath, closeErr)
	}
	fields := strings.Fields(string(data))
	currentPID := protectedCgroupCurrentPID()
	if len(fields) != 1 || fields[0] != strconv.Itoa(currentPID) {
		return fmt.Errorf(
			"protected cgroup parent %s must contain only current agentd pid %d; found %q",
			parentPath,
			currentPID,
			fields,
		)
	}
	return nil
}

func revalidateProtectedCgroupRecoveryBundle(
	parentFD int,
	parentPath string,
	plan *protectedCgroupRecoveryBundle,
	owner ProtectedCgroupIdentity,
) error {
	return validateProtectedCgroupRecoveryBundle(parentFD, parentPath, plan, owner, false)
}

func validateProtectedCgroupRecoveryBundle(
	parentFD int,
	parentPath string,
	plan *protectedCgroupRecoveryBundle,
	owner ProtectedCgroupIdentity,
	openChildren bool,
) error {
	bundlePath := filepath.Join(parentPath, plan.name)
	if err := validateProtectedCgroupDirectoryOwnership(plan.bundleFD, bundlePath, owner); err != nil {
		return err
	}
	if err := validateProtectedCgroupNameStillReferencesFD(parentFD, plan.name, plan.bundleFD, bundlePath); err != nil {
		return err
	}
	entries, err := readProtectedCgroupDirectoryEntries(plan.bundleFD, bundlePath)
	if err != nil {
		return err
	}
	seenChildren := make(map[string]struct{})
	for _, entry := range entries {
		var stats unix.Stat_t
		if err := unix.Fstatat(plan.bundleFD, entry, &stats, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("stat protected cgroup entry %s/%s: %w", bundlePath, entry, err)
		}
		modeType := stats.Mode & unix.S_IFMT
		if modeType == unix.S_IFLNK {
			return fmt.Errorf("protected cgroup entry %s/%s must not be a symlink", bundlePath, entry)
		}
		if modeType != unix.S_IFDIR {
			if modeType != unix.S_IFREG {
				return fmt.Errorf("protected cgroup entry %s/%s must be a regular cgroup interface", bundlePath, entry)
			}
			if !strings.HasPrefix(entry, "cgroup.") && !isProtectedCgroupControllerEntry(entry, plan.controllers) {
				return fmt.Errorf("protected cgroup %s contains unknown entry %q", bundlePath, entry)
			}
			continue
		}
		if entry != "agentd" && entry != "provider" {
			return fmt.Errorf("protected cgroup %s contains unknown child directory %q", bundlePath, entry)
		}
		seenChildren[entry] = struct{}{}
		childPath := filepath.Join(bundlePath, entry)
		if openChildren {
			childFD, openErr := unix.Openat(plan.bundleFD, entry, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if openErr != nil {
				return fmt.Errorf("open protected cgroup %s: %w", childPath, openErr)
			}
			plan.childFDs[entry] = childFD
		}
		childFD, found := plan.childFDs[entry]
		if !found {
			return fmt.Errorf("protected cgroup child %s was replaced during recovery", childPath)
		}
		if err := validateProtectedCgroupDirectoryOwnership(childFD, childPath, owner); err != nil {
			return err
		}
		if err := validateProtectedCgroupNameStillReferencesFD(plan.bundleFD, entry, childFD, childPath); err != nil {
			return err
		}
	}
	for childName := range plan.childFDs {
		if _, found := seenChildren[childName]; !found {
			return fmt.Errorf("protected cgroup child %s/%s disappeared during recovery", bundlePath, childName)
		}
	}
	return nil
}

func readProtectedCgroupControllers(directoryFD int, directoryPath string) (map[string]struct{}, error) {
	fd, err := unix.Openat(directoryFD, "cgroup.controllers", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open protected cgroup controllers %s/cgroup.controllers: %w", directoryPath, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directoryPath, "cgroup.controllers"))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open protected cgroup controllers file %s/cgroup.controllers", directoryPath)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read protected cgroup controllers %s/cgroup.controllers: %w", directoryPath, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close protected cgroup controllers %s/cgroup.controllers: %w", directoryPath, closeErr)
	}
	if len(data) > 4096 {
		return nil, fmt.Errorf("protected cgroup controllers %s/cgroup.controllers exceeds 4096 bytes", directoryPath)
	}
	controllers := make(map[string]struct{})
	for _, controller := range strings.Fields(string(data)) {
		if !validProtectedCgroupControllerName(controller) {
			return nil, fmt.Errorf("protected cgroup controllers %s/cgroup.controllers contains invalid controller %q", directoryPath, controller)
		}
		if _, duplicate := controllers[controller]; duplicate {
			return nil, fmt.Errorf("protected cgroup controllers %s/cgroup.controllers contains duplicate controller %q", directoryPath, controller)
		}
		controllers[controller] = struct{}{}
	}
	return controllers, nil
}

func validProtectedCgroupControllerName(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func isProtectedCgroupControllerEntry(entry string, controllers map[string]struct{}) bool {
	controller, suffix, found := strings.Cut(entry, ".")
	if !found || suffix == "" {
		return false
	}
	_, allowed := controllers[controller]
	return allowed
}

func mergeProtectedCgroupControllers(controllerSets ...map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for _, controllers := range controllerSets {
		for controller := range controllers {
			result[controller] = struct{}{}
		}
	}
	return result
}

func validateProtectedCgroupNameStillReferencesFD(parentFD int, name string, openedFD int, path string) error {
	var named unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat protected cgroup name %s: %w", path, err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(openedFD, &opened); err != nil {
		return fmt.Errorf("stat protected cgroup fd %s: %w", path, err)
	}
	if named.Mode&unix.S_IFMT != unix.S_IFDIR || named.Dev != opened.Dev || named.Ino != opened.Ino {
		return fmt.Errorf("protected cgroup %s was replaced during recovery", path)
	}
	return nil
}

func readProtectedCgroupDirectoryEntries(directoryFD int, directoryPath string) ([]string, error) {
	duplicateFD, err := unix.Openat(directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open protected cgroup directory fd for %s: %w", directoryPath, err)
	}
	directory := os.NewFile(uintptr(duplicateFD), directoryPath)
	if directory == nil {
		_ = unix.Close(duplicateFD)
		return nil, fmt.Errorf("open protected cgroup directory file for %s", directoryPath)
	}
	defer directory.Close()
	entries, err := directory.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("read protected cgroup directory %s: %w", directoryPath, err)
	}
	sort.Strings(entries)
	return entries, nil
}

func cleanupProtectedCgroupRecoveryBundle(parentFD int, parentPath string, plan *protectedCgroupRecoveryBundle) error {
	bundlePath := filepath.Join(parentPath, plan.name)
	var result error
	for _, childName := range []string{"provider", "agentd"} {
		childFD, found := plan.childFDs[childName]
		if !found || childFD < 0 {
			continue
		}
		result = errors.Join(result, cleanupProtectedCgroupDirectory(childFD, plan.bundleFD, bundlePath, childName))
		plan.childFDs[childName] = -1
	}
	if result != nil {
		return result
	}
	result = cleanupProtectedCgroupDirectory(plan.bundleFD, parentFD, parentPath, plan.name)
	plan.bundleFD = -1
	return result
}

func closeProtectedCgroupRecoveryBundles(plans []*protectedCgroupRecoveryBundle) {
	for _, plan := range plans {
		for name, fd := range plan.childFDs {
			_ = closeIfOpen(fd)
			plan.childFDs[name] = -1
		}
		_ = closeIfOpen(plan.bundleFD)
		plan.bundleFD = -1
	}
}

func readProtectedCgroupDirectoryNames(parentFD int, parentPath string) ([]string, error) {
	return readProtectedCgroupDirectoryEntries(parentFD, parentPath)
}

func parseProtectedCgroupBundleName(name string) (parsedProtectedCgroupBundle, bool, error) {
	if strings.HasPrefix(name, protectedCgroupBundlePrefix) {
		parts := strings.Split(name, "-")
		if len(parts) != 6 || parts[0] != "synara" ||
			!strings.HasPrefix(parts[1], "e") ||
			!strings.HasPrefix(parts[2], "g") ||
			!strings.HasPrefix(parts[3], "w") ||
			!strings.HasPrefix(parts[4], "s") ||
			!strings.HasPrefix(parts[5], "r") {
			return parsedProtectedCgroupBundle{}, true, errors.New("invalid protected cgroup bundle shape")
		}
		executionID, err := parseProtectedCgroupCompactUUID(strings.TrimPrefix(parts[1], "e"))
		if err != nil {
			return parsedProtectedCgroupBundle{}, true, fmt.Errorf("execution id: %w", err)
		}
		generation, err := strconv.ParseInt(strings.TrimPrefix(parts[2], "g"), 10, 64)
		if err != nil || generation <= 0 {
			return parsedProtectedCgroupBundle{}, true, errors.New("invalid execution generation")
		}
		workerInstance, err := parseProtectedCgroupCompactUUID(strings.TrimPrefix(parts[3], "w"))
		if err != nil {
			return parsedProtectedCgroupBundle{}, true, fmt.Errorf("worker instance: %w", err)
		}
		supervisorInstance, err := parseProtectedCgroupCompactUUID(strings.TrimPrefix(parts[4], "s"))
		if err != nil {
			return parsedProtectedCgroupBundle{}, true, fmt.Errorf("supervisor instance: %w", err)
		}
		runtimeInstance, err := parseProtectedCgroupCompactUUID(strings.TrimPrefix(parts[5], "r"))
		if err != nil {
			return parsedProtectedCgroupBundle{}, true, fmt.Errorf("runtime instance: %w", err)
		}
		return parsedProtectedCgroupBundle{
			fence: ProtectedCgroupFence{
				ExecutionID: executionID, Generation: generation, WorkerIncarnation: workerInstance,
			},
			supervisorInstance: supervisorInstance,
			runtimeInstance:    runtimeInstance,
		}, true, nil
	}
	if strings.HasPrefix(name, protectedCgroupLegacyBundlePrefix) {
		parts := strings.Split(name, "-")
		if len(parts) != 3 || parts[0] != "synara" ||
			!strings.HasPrefix(parts[1], "g") || !strings.HasPrefix(parts[2], "i") {
			return parsedProtectedCgroupBundle{}, true, errors.New("invalid legacy protected cgroup bundle shape")
		}
		generation, err := strconv.ParseInt(strings.TrimPrefix(parts[1], "g"), 10, 64)
		if err != nil || generation <= 0 {
			return parsedProtectedCgroupBundle{}, true, errors.New("invalid legacy execution generation")
		}
		workerInstance, err := parseProtectedCgroupCompactUUID(strings.TrimPrefix(parts[2], "i"))
		if err != nil {
			return parsedProtectedCgroupBundle{}, true, fmt.Errorf("legacy worker instance: %w", err)
		}
		return parsedProtectedCgroupBundle{
			fence:  ProtectedCgroupFence{Generation: generation, WorkerIncarnation: workerInstance},
			legacy: true,
		}, true, nil
	}
	return parsedProtectedCgroupBundle{}, false, nil
}

func parseProtectedCgroupCompactUUID(value string) (uuid.UUID, error) {
	if len(value) != 32 {
		return uuid.Nil, errors.New("compact UUID must contain 32 hexadecimal characters")
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return uuid.Nil, errors.New("compact UUID must use canonical lowercase hexadecimal characters")
		}
	}
	parsed, err := uuid.Parse(
		value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:],
	)
	if err != nil {
		return uuid.Nil, err
	}
	if parsed == uuid.Nil {
		return uuid.Nil, errors.New("compact UUID must not be nil")
	}
	return parsed, nil
}

func validateProtectedCgroupFilesystem(parentFD int, parentPath string) error {
	var stats unix.Statfs_t
	if err := protectedCgroupFstatfs(parentFD, &stats); err != nil {
		return fmt.Errorf("stat protected cgroup parent %s: %w", parentPath, err)
	}
	if uint64(stats.Type) != linuxCgroup2SuperMagic {
		return fmt.Errorf("%s is not on a cgroup2 filesystem", parentPath)
	}
	mountPoint, err := findProtectedCgroupV2MountPoint(parentPath)
	if err != nil {
		return err
	}
	if filepath.Clean(mountPoint) == filepath.Clean(parentPath) {
		return fmt.Errorf("%s must be a delegated cgroup subtree, not the cgroup2 mount root", parentPath)
	}
	return nil
}

func findProtectedCgroupV2MountPoint(path string) (string, error) {
	data, err := protectedCgroupMountInfo()
	if err != nil {
		return "", fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	best := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		left, right, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		rightFields := strings.Fields(right)
		if len(rightFields) == 0 || rightFields[0] != "cgroup2" {
			continue
		}
		leftFields := strings.Fields(left)
		if len(leftFields) < 5 {
			continue
		}
		mountPoint := decodeMountInfoPath(leftFields[4])
		if !pathWithinMount(path, mountPoint) {
			continue
		}
		if len(mountPoint) > len(best) {
			best = mountPoint
		}
	}
	if best == "" {
		return "", fmt.Errorf("cgroup2 mount for %s was not found", path)
	}
	return filepath.Clean(best), nil
}

func validateProtectedCgroupDirectoryOwnership(directoryFD int, directoryPath string, owner ProtectedCgroupIdentity) error {
	var stats unix.Stat_t
	if err := unix.Fstat(directoryFD, &stats); err != nil {
		return fmt.Errorf("stat protected cgroup %s: %w", directoryPath, err)
	}
	if stats.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%s is not a directory", directoryPath)
	}
	if uint32(stats.Uid) != owner.UID || uint32(stats.Gid) != owner.GID {
		return fmt.Errorf(
			"%s is owned by uid=%d gid=%d, want uid=%d gid=%d",
			directoryPath,
			stats.Uid,
			stats.Gid,
			owner.UID,
			owner.GID,
		)
	}
	mode := os.FileMode(stats.Mode) & os.ModePerm
	if mode&0o022 != 0 {
		return fmt.Errorf("%s must not be group- or other-writable", directoryPath)
	}
	return nil
}

func createProtectedCgroupDirectory(parentFD int, parentPath, name string, owner ProtectedCgroupIdentity) (int, string, error) {
	if err := unix.Mkdirat(parentFD, name, 0o750); err != nil {
		return -1, "", fmt.Errorf("create protected cgroup %s/%s: %w", parentPath, name, err)
	}
	directoryFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		removeErr := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
		return -1, "", errors.Join(
			fmt.Errorf("open protected cgroup %s/%s: %w", parentPath, name, err),
			removeErr,
		)
	}
	directoryPath := filepath.Join(parentPath, name)
	if err := validateProtectedCgroupDirectoryOwnership(directoryFD, directoryPath, owner); err != nil {
		removeErr := cleanupProtectedCgroupDirectory(directoryFD, parentFD, parentPath, name)
		return -1, "", errors.Join(err, removeErr)
	}
	return directoryFD, directoryPath, nil
}

func cleanupProtectedCgroupDirectory(directoryFD, parentFD int, parentPath, name string) error {
	if directoryFD < 0 {
		return nil
	}
	var result error
	directoryPath := filepath.Join(parentPath, name)
	result = errors.Join(result, writeCgroupKill(directoryFD, parentPath, name))
	result = errors.Join(result, waitForCgroupEmpty(directoryFD, parentPath, name))
	result = errors.Join(result, protectedCgroupCleanupTestHook(directoryFD, directoryPath))
	result = errors.Join(result, closeIfOpen(directoryFD))
	if parentFD >= 0 {
		result = errors.Join(
			result,
			func() error {
				if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
					return fmt.Errorf("remove protected cgroup %s/%s: %w", parentPath, name, err)
				}
				return nil
			}(),
		)
	}
	return result
}

func protectedCgroupBundleName(
	fence ProtectedCgroupFence,
	supervisorInstance uuid.UUID,
	runtimeInstance uuid.UUID,
) string {
	return fmt.Sprintf(
		"%s%s-g%d-w%s-s%s-r%s",
		protectedCgroupBundlePrefix,
		strings.ReplaceAll(fence.ExecutionID.String(), "-", ""),
		fence.Generation,
		strings.ReplaceAll(fence.WorkerIncarnation.String(), "-", ""),
		strings.ReplaceAll(supervisorInstance.String(), "-", ""),
		strings.ReplaceAll(runtimeInstance.String(), "-", ""),
	)
}

func protectedCgroupBundleNameForConfig(config ProtectedCgroupSupervisorConfig) string {
	name := protectedCgroupBundleName(config.Fence, config.SupervisorInstance, config.RuntimeInstance)
	if config.Diagnostic {
		return protectedCgroupDiagnosticPrefix + strings.TrimPrefix(name, protectedCgroupBundlePrefix)
	}
	return name
}

func protectedCgroupCreationReservationKey(parentPath string, config ProtectedCgroupSupervisorConfig) string {
	return fmt.Sprintf(
		"%s\x00%s\x00%d\x00%s\x00%s",
		parentPath,
		config.Fence.ExecutionID,
		config.Fence.Generation,
		config.Fence.WorkerIncarnation,
		config.SupervisorInstance,
	)
}

func reserveProtectedCgroupCreation(key string) error {
	protectedCgroupCreationReservations.Lock()
	defer protectedCgroupCreationReservations.Unlock()
	if _, exists := protectedCgroupCreationReservations.active[key]; exists {
		return errors.New("protected cgroup execution fence is already active in this agentd")
	}
	protectedCgroupCreationReservations.active[key] = struct{}{}
	return nil
}

func releaseProtectedCgroupCreation(key string) {
	protectedCgroupCreationReservations.Lock()
	delete(protectedCgroupCreationReservations.active, key)
	protectedCgroupCreationReservations.Unlock()
}

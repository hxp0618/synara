//go:build linux

package agentd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const protectedCgroupBundlePrefix = "synara-g"

var (
	protectedCgroupFstatfs   = unix.Fstatfs
	protectedCgroupMountInfo = func() ([]byte, error) {
		return os.ReadFile("/proc/self/mountinfo")
	}
	protectedCgroupCleanupTestHook = func(_ int, _ string) error { return nil }
)

// ProtectedCgroupSupervisor owns a fenced cgroup-v2 subtree for a privileged
// agentd supervisor and one untrusted Provider process tree.
type ProtectedCgroupSupervisor struct {
	parentFD   int
	bundleFD   int
	agentdFD   int
	providerFD int

	paths ProtectedCgroupPaths
	fence ProtectedCgroupFence
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

	bundleName := protectedCgroupBundleName(config.Fence)
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
	return &ProtectedCgroupSupervisor{
		parentFD: parentFD, bundleFD: bundleFD, agentdFD: agentdFD, providerFD: providerFD,
		paths: ProtectedCgroupPaths{
			ParentPath: normalizedParent, BundlePath: bundlePath, AgentdPath: agentdPath, ProviderPath: providerPath,
		},
		fence: config.Fence,
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
	if config.Fence.Generation <= 0 {
		return errors.New("protected cgroup generation fence must be positive")
	}
	if config.Fence.WorkerIncarnation == uuid.Nil {
		return errors.New("protected cgroup worker incarnation fence is required")
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

func protectedCgroupBundleName(fence ProtectedCgroupFence) string {
	return fmt.Sprintf(
		"%s%d-i%s",
		protectedCgroupBundlePrefix,
		fence.Generation,
		strings.ReplaceAll(fence.WorkerIncarnation.String(), "-", ""),
	)
}

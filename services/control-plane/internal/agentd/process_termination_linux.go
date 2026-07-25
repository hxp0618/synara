//go:build linux

package agentd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	linuxCgroupCleanupTimeout = 5 * time.Second
	linuxCgroupPollInterval   = 25 * time.Millisecond
	linuxCgroup2SuperMagic    = 0x63677270
)

// processTree either falls back to Unix process-group termination or, when
// explicitly configured with a delegated cgroup-v2 subtree, binds the child to
// a per-process cgroup before its first instruction executes.
type processTree struct {
	command     *exec.Cmd
	containment linuxProcessContainment

	mu         sync.Mutex
	released   bool
	terminated bool

	cleanupOnce sync.Once
	cleanupErr  error
}

type linuxProcessContainment interface {
	started() error
	cleanup() error
}

type linuxCgroup struct {
	rootPath  string
	rootFD    int
	childName string
	childFD   int
}

type protectedLinuxCgroup struct {
	supervisor *ProtectedCgroupSupervisor
	fence      ProtectedCgroupFence
	attachFD   int
}

func newProcessTree(command *exec.Cmd, options ...processTreeOptions) (*processTree, error) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	if command.SysProcAttr.Pdeathsig == 0 {
		command.SysProcAttr.Pdeathsig = syscall.SIGKILL
	}
	var opts processTreeOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if strings.TrimSpace(opts.CgroupV2Root) == "" {
		command.SysProcAttr.Setpgid = true
		return &processTree{command: command}, nil
	}
	if opts.ProtectedProviderIdentity != nil {
		protectedCgroup, err := newProtectedLinuxCgroup(
			opts.CgroupV2Root,
			*opts.ProtectedProviderIdentity,
			opts.ContainmentFence,
		)
		if err != nil {
			return nil, err
		}
		command.SysProcAttr.UseCgroupFD = true
		command.SysProcAttr.CgroupFD = protectedCgroup.attachFD
		command.SysProcAttr.Credential = &syscall.Credential{
			Uid:         opts.ProtectedProviderIdentity.UID,
			Gid:         opts.ProtectedProviderIdentity.GID,
			NoSetGroups: true,
		}
		return &processTree{command: command, containment: protectedCgroup}, nil
	}
	cgroup, err := newLinuxCgroup(opts.CgroupV2Root)
	if err != nil {
		return nil, err
	}
	command.SysProcAttr.UseCgroupFD = true
	command.SysProcAttr.CgroupFD = cgroup.childFD
	return &processTree{command: command, containment: cgroup}, nil
}

func (p *processTree) started() error {
	if p == nil || p.containment == nil {
		return nil
	}
	return p.containment.started()
}

func (p *processTree) terminate() error {
	return p.cleanup("termination", true)
}

func (p *processTree) release() error {
	// Release is a final ownership handoff, not permission for descendants to
	// outlive the Provider root. Kill the boundary even on the normal-exit path.
	return p.cleanup("cleanup", true)
}

func (p *processTree) cleanup(phase string, force bool) error {
	p.cleanupOnce.Do(func() {
		if p.containment != nil {
			p.cleanupErr = newContainmentError(phase, p.containment.cleanup())
			return
		}
		if !force || p.command == nil || p.command.Process == nil {
			return
		}
		err := syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return
		}
		rootErr := p.command.Process.Kill()
		if rootErr == nil || errors.Is(rootErr, os.ErrProcessDone) {
			rootErr = nil
		}
		if rootErr != nil {
			err = errors.Join(err, rootErr)
		}
		p.cleanupErr = newContainmentError(phase, err)
	})
	p.mu.Lock()
	if p.cleanupErr == nil {
		p.terminated = p.containment != nil || (p.command != nil && p.command.Process != nil)
	}
	if phase == "cleanup" {
		p.released = true
	}
	p.mu.Unlock()
	return p.cleanupErr
}

func newProtectedLinuxCgroup(
	rootPath string,
	providerIdentity ProtectedCgroupIdentity,
	fence ProtectedCgroupFence,
) (*protectedLinuxCgroup, error) {
	supervisorIdentity := currentProtectedCgroupSupervisorIdentity()
	if err := validateProtectedCgroupIdentityBoundary(supervisorIdentity, providerIdentity); err != nil {
		return nil, err
	}
	supervisor, err := NewProtectedCgroupSupervisor(ProtectedCgroupSupervisorConfig{
		ParentPath:         rootPath,
		SupervisorIdentity: supervisorIdentity,
		ProviderIdentity:   providerIdentity,
		Fence:              fence,
	})
	if err != nil {
		return nil, err
	}
	attachFD, _, err := openDirectoryNoSymlinks(supervisor.Paths().ProviderPath)
	if err != nil {
		cleanupErr := supervisor.Cleanup(fence)
		return nil, errors.Join(
			fmt.Errorf("open protected provider cgroup attach fd: %w", err),
			cleanupErr,
		)
	}
	return &protectedLinuxCgroup{supervisor: supervisor, fence: fence, attachFD: attachFD}, nil
}

func newLinuxCgroup(rootPath string) (*linuxCgroup, error) {
	rootFD, normalizedPath, err := openDirectoryNoSymlinks(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open delegated cgroup root: %w", err)
	}
	cleanupRoot := true
	defer func() {
		if cleanupRoot {
			_ = unix.Close(rootFD)
		}
	}()
	if err := validateDelegatedCgroupRoot(rootFD, normalizedPath); err != nil {
		return nil, err
	}
	childName := "synara-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := unix.Mkdirat(rootFD, childName, 0o755); err != nil {
		return nil, fmt.Errorf("create delegated child cgroup under %s: %w", normalizedPath, err)
	}
	childFD, err := unix.Openat(rootFD, childName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		removeErr := unix.Unlinkat(rootFD, childName, unix.AT_REMOVEDIR)
		return nil, errors.Join(
			fmt.Errorf("open delegated child cgroup %s/%s: %w", normalizedPath, childName, err),
			closeIfOpen(rootFD),
			removeErr,
		)
	}
	cleanupRoot = false
	return &linuxCgroup{
		rootPath: normalizedPath, rootFD: rootFD, childName: childName, childFD: childFD,
	}, nil
}

func validateDelegatedCgroupRoot(rootFD int, rootPath string) error {
	var stats unix.Statfs_t
	if err := unix.Fstatfs(rootFD, &stats); err != nil {
		return fmt.Errorf("stat delegated cgroup root %s: %w", rootPath, err)
	}
	if uint64(stats.Type) != linuxCgroup2SuperMagic {
		return fmt.Errorf("%s is not on a cgroup2 filesystem", rootPath)
	}
	mountPoint, err := findCgroupV2MountPoint(rootPath)
	if err != nil {
		return err
	}
	if filepath.Clean(mountPoint) == filepath.Clean(rootPath) {
		return fmt.Errorf("%s must be a delegated cgroup subtree, not the cgroup2 mount root", rootPath)
	}
	return nil
}

func (c *linuxCgroup) cleanup() error {
	var result error
	if err := writeCgroupKill(c.childFD, c.rootPath, c.childName); err != nil {
		result = errors.Join(result, err)
	}
	if err := waitForCgroupEmpty(c.childFD, c.rootPath, c.childName); err != nil {
		result = errors.Join(result, err)
	}
	result = errors.Join(result, closeIfOpen(c.childFD))
	c.childFD = -1
	if err := unix.Unlinkat(c.rootFD, c.childName, unix.AT_REMOVEDIR); err != nil {
		result = errors.Join(
			result,
			fmt.Errorf("remove delegated child cgroup %s/%s: %w", c.rootPath, c.childName, err),
		)
	}
	result = errors.Join(result, closeIfOpen(c.rootFD))
	c.rootFD = -1
	return result
}

func (c *linuxCgroup) started() error { return nil }

func (c *protectedLinuxCgroup) started() error {
	if c == nil {
		return nil
	}
	err := closeIfOpen(c.attachFD)
	c.attachFD = -1
	return err
}

func (c *protectedLinuxCgroup) cleanup() error {
	if c == nil {
		return nil
	}
	var result error
	result = errors.Join(result, closeIfOpen(c.attachFD))
	c.attachFD = -1
	if c.supervisor != nil {
		result = errors.Join(result, c.supervisor.Cleanup(c.fence))
		c.supervisor = nil
	}
	return result
}

func writeCgroupKill(childFD int, rootPath, childName string) error {
	fd, err := unix.Openat(childFD, "cgroup.kill", unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %s/%s/cgroup.kill: %w", rootPath, childName, err)
	}
	defer unix.Close(fd)
	payload := []byte("1")
	for len(payload) > 0 {
		written, writeErr := unix.Write(fd, payload)
		if writeErr != nil {
			return fmt.Errorf("write %s/%s/cgroup.kill: %w", rootPath, childName, writeErr)
		}
		payload = payload[written:]
	}
	return nil
}

func waitForCgroupEmpty(childFD int, rootPath, childName string) error {
	deadline := time.Now().Add(linuxCgroupCleanupTimeout)
	for {
		populated, err := cgroupPopulated(childFD, rootPath, childName)
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"wait for %s/%s to empty exceeded %s",
				rootPath,
				childName,
				linuxCgroupCleanupTimeout,
			)
		}
		time.Sleep(linuxCgroupPollInterval)
	}
}

func cgroupPopulated(childFD int, rootPath, childName string) (bool, error) {
	fd, err := unix.Openat(childFD, "cgroup.events", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("open %s/%s/cgroup.events: %w", rootPath, childName, err)
	}
	defer unix.Close(fd)
	buffer := make([]byte, 4096)
	count, readErr := unix.Read(fd, buffer)
	if readErr != nil {
		return false, fmt.Errorf("read %s/%s/cgroup.events: %w", rootPath, childName, readErr)
	}
	for _, line := range strings.Split(string(buffer[:count]), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == "populated" {
			switch fields[1] {
			case "0":
				return false, nil
			case "1":
				return true, nil
			default:
				return false, fmt.Errorf(
					"%s/%s/cgroup.events reported unexpected populated value %q",
					rootPath,
					childName,
					fields[1],
				)
			}
		}
	}
	return false, fmt.Errorf("%s/%s/cgroup.events omitted populated state", rootPath, childName)
}

func openDirectoryNoSymlinks(path string) (int, string, error) {
	if !filepath.IsAbs(path) {
		return -1, "", errors.New("delegated cgroup root must be an absolute path")
	}
	normalized := filepath.Clean(path)
	if normalized == string(filepath.Separator) {
		return -1, "", errors.New("delegated cgroup root cannot be /")
	}
	currentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	for _, component := range strings.Split(strings.TrimPrefix(normalized, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(currentFD)
			return -1, "", errors.New("delegated cgroup root contains an invalid path segment")
		}
		nextFD, openErr := unix.Openat(
			currentFD,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		_ = unix.Close(currentFD)
		if openErr != nil {
			return -1, "", fmt.Errorf("open delegated cgroup root segment %q: %w", component, openErr)
		}
		currentFD = nextFD
	}
	return currentFD, normalized, nil
}

func findCgroupV2MountPoint(path string) (string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
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

func decodeMountInfoPath(value string) string {
	var builder strings.Builder
	for index := 0; index < len(value); {
		if value[index] == '\\' && index+3 < len(value) {
			if decoded, err := strconv.ParseInt(value[index+1:index+4], 8, 32); err == nil {
				builder.WriteByte(byte(decoded))
				index += 4
				continue
			}
		}
		builder.WriteByte(value[index])
		index++
	}
	return builder.String()
}

func pathWithinMount(path, mountPoint string) bool {
	path = filepath.Clean(path)
	mountPoint = filepath.Clean(mountPoint)
	if mountPoint == string(filepath.Separator) {
		return true
	}
	if path == mountPoint {
		return true
	}
	return strings.HasPrefix(path, mountPoint+string(filepath.Separator))
}

func closeIfOpen(fd int) error {
	if fd < 0 {
		return nil
	}
	return unix.Close(fd)
}

func currentProtectedCgroupSupervisorIdentity() ProtectedCgroupIdentity {
	return ProtectedCgroupIdentity{UID: uint32(unix.Geteuid()), GID: uint32(unix.Getegid())}
}

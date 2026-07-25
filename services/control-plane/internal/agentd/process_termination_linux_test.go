//go:build linux

package agentd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const linuxCgroupTestRootEnvironment = "SYNARA_TEST_CGROUP_V2_ROOT"

func TestLinuxCgroupV2ContainmentKillsSetsidDescendantAndRemovesChild(t *testing.T) {
	root := strings.TrimSpace(os.Getenv(linuxCgroupTestRootEnvironment))
	if root == "" {
		t.Skip(linuxCgroupTestRootEnvironment + " is not configured")
	}
	before := linuxCgroupTestChildren(t, root)
	ready, sentinel := processTreeTestPaths(t)
	command := exec.Command(
		os.Args[0], "-test.run=^TestLinuxCgroupEscapeHelper$", "--",
		"--synara-linux-cgroup-helper", "root", ready, sentinel,
	)
	command.Env = os.Environ()
	tree, err := newProcessTree(command, processTreeOptions{CgroupV2Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		_ = tree.release()
		t.Fatal(err)
	}
	if err := tree.started(); err != nil {
		_ = tree.terminate()
		_ = command.Wait()
		t.Fatal(err)
	}
	waitForProcessTreeReady(t, ready)
	if err := tree.terminate(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if !tree.terminated {
		t.Fatal("cgroup process tree did not record confirmed termination")
	}
	assertProcessTreeSentinelAbsent(t, sentinel)
	after := linuxCgroupTestChildren(t, root)
	if !sameStringSet(before, after) {
		t.Fatalf("cgroup cleanup leaked a child: before=%v after=%v", before, after)
	}
}

func TestLinuxCgroupV2DoesNotAdvertiseStrictContainmentWithoutProtectedSupervisor(t *testing.T) {
	root := strings.TrimSpace(os.Getenv(linuxCgroupTestRootEnvironment))
	if root == "" {
		t.Skip(linuxCgroupTestRootEnvironment + " is not configured")
	}
	runner := providerHostV2TestRunner()
	runner.cgroupV2Root = root
	if _, err := runner.CapabilitySummary(t.Context()); err != nil {
		t.Fatal(err)
	}
	capabilities := withProviderHostCapabilities(nil, map[string]any{"legacy": false}, Config{
		Version: "agentd-test", RunnerProtocol: RunnerProtocolV2,
	})
	if _, found := capabilities[resourceSuspendContainmentCapabilityKey]; found {
		t.Fatalf("same-identity cgroup containment was advertised without an escape-resistant supervisor: %#v", capabilities)
	}
}

func TestProtectedLinuxCgroupPreparesProviderCredentialAndAttachFD(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorIdentity := currentProtectedCgroupSupervisorIdentity()
	providerIdentity := ProtectedCgroupIdentity{
		UID: supervisorIdentity.UID + 1,
		GID: supervisorIdentity.GID + 1,
	}
	command := exec.Command("true")
	tree, err := newProcessTree(command, processTreeOptions{
		CgroupV2Root:              root,
		ProtectedProviderIdentity: &providerIdentity,
		ContainmentFence: ProtectedCgroupFence{
			Generation:        3,
			WorkerIncarnation: uuid.New(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if command.SysProcAttr == nil || command.SysProcAttr.Credential == nil {
		t.Fatalf("protected process tree omitted Linux credentials: %#v", command.SysProcAttr)
	}
	if command.SysProcAttr.Credential.Uid != providerIdentity.UID ||
		command.SysProcAttr.Credential.Gid != providerIdentity.GID ||
		!command.SysProcAttr.Credential.NoSetGroups {
		t.Fatalf("unexpected protected credentials: %#v", command.SysProcAttr.Credential)
	}
	if command.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("parent-death signal = %v, want SIGKILL", command.SysProcAttr.Pdeathsig)
	}
	if !command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD <= 0 {
		t.Fatalf("protected process tree omitted cgroup attach fd: %#v", command.SysProcAttr)
	}
	entriesBefore, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesBefore) != 1 || !entriesBefore[0].IsDir() {
		t.Fatalf("protected cgroup tree was not created under %s: %#v", root, entriesBefore)
	}
	bundlePath := filepath.Join(root, entriesBefore[0].Name())
	installProtectedCgroupControlFiles(t, bundlePath)
	installProtectedCgroupControlFiles(t, filepath.Join(bundlePath, "agentd"))
	installProtectedCgroupControlFiles(t, filepath.Join(bundlePath, "provider"))
	if err := tree.started(); err != nil {
		t.Fatal(err)
	}
	if err := tree.release(); err != nil {
		t.Fatal(err)
	}
	entriesAfter, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != 0 {
		t.Fatalf("protected cgroup tree leaked entries under %s: %#v", root, entriesAfter)
	}
}

func TestProtectedLinuxCgroupRejectsProviderSharingSupervisorUID(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	overrideProtectedCgroupLinuxTestEnvironment(t, root)

	supervisorIdentity := currentProtectedCgroupSupervisorIdentity()
	_, err := newProcessTree(exec.Command("true"), processTreeOptions{
		CgroupV2Root: root,
		ProtectedProviderIdentity: &ProtectedCgroupIdentity{
			UID: supervisorIdentity.UID,
			GID: supervisorIdentity.GID + 1,
		},
		ContainmentFence: ProtectedCgroupFence{
			Generation:        1,
			WorkerIncarnation: uuid.New(),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "must not share a UID") {
		t.Fatalf("same-uid protected identity error = %v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("same-uid validation created protected cgroup entries: %#v", entries)
	}
}

func TestLinuxCgroupEscapeHelper(t *testing.T) {
	arguments := helperArgumentsAfter("--synara-linux-cgroup-helper")
	if len(arguments) != 3 {
		t.Skip("Linux cgroup helper")
	}
	mode, ready, sentinel := arguments[0], arguments[1], arguments[2]
	switch mode {
	case "root":
		child := exec.Command(
			os.Args[0], "-test.run=^TestLinuxCgroupEscapeHelper$", "--",
			"--synara-linux-cgroup-helper", "child", ready, sentinel,
		)
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			_ = os.WriteFile(ready, []byte("spawn escaped child: "+err.Error()), 0o600)
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "child":
		if _, err := unix.Setsid(); err != nil {
			_ = os.WriteFile(ready, []byte("setsid: "+err.Error()), 0o600)
			os.Exit(3)
		}
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(processTreeGrandchildDelay)
		if err := os.WriteFile(sentinel, []byte("setsid descendant survived"), 0o600); err != nil {
			os.Exit(5)
		}
		os.Exit(0)
	default:
		os.Exit(6)
	}
}

func helperArgumentsAfter(marker string) []string {
	for index, argument := range os.Args {
		if argument == marker && index+1 < len(os.Args) {
			return os.Args[index+1:]
		}
	}
	return nil
}

func linuxCgroupTestChildren(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	children := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "synara-") {
			children = append(children, filepath.Clean(entry.Name()))
		}
	}
	return children
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

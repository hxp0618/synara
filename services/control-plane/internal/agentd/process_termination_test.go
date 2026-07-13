//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows

package agentd

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	processTreeTestModeEnv     = "SYNARA_PROCESS_TREE_TEST_MODE"
	processTreeTestReadyEnv    = "SYNARA_PROCESS_TREE_TEST_READY"
	processTreeTestSentinelEnv = "SYNARA_PROCESS_TREE_TEST_SENTINEL"
	processTreeTestObservedEnv = "SYNARA_PROCESS_TREE_TEST_OBSERVED"
	processTreeGrandchildDelay = 750 * time.Millisecond
)

func TestLegacyRunnerCancellationTerminatesDescendants(t *testing.T) {
	ready, sentinel := processTreeTestPaths(t)
	t.Setenv(processTreeTestModeEnv, "root")
	t.Setenv(processTreeTestReadyEnv, ready)
	t.Setenv(processTreeTestSentinelEnv, sentinel)
	runner := &Runner{
		command:         processTreeRunnerTestCommand(),
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workspace := t.TempDir()
	result := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, RunnerInput{WorkspaceDirectory: workspace}, nil, func(context.Context, RunnerMessage) error {
			return nil
		})
		result <- err
	}()

	waitForProcessTreeReady(t, ready)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("legacy Runner returned %v after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legacy Runner did not terminate after cancellation")
	}
	assertProcessTreeSentinelAbsent(t, sentinel)
}

func TestProviderHostAbortTerminatesDescendants(t *testing.T) {
	ready, sentinel := processTreeTestPaths(t)
	t.Setenv(processTreeTestModeEnv, "root")
	t.Setenv(processTreeTestReadyEnv, ready)
	t.Setenv(processTreeTestSentinelEnv, sentinel)
	runner := &Runner{
		command:         processTreeRunnerTestCommand(),
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV2,
	}
	process, err := runner.startProviderHostV2(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer process.abort()
	waitForProcessTreeReady(t, ready)
	process.abort()
	assertProcessTreeSentinelAbsent(t, sentinel)
}

func TestProviderHostBlockedStdinHonorsContext(t *testing.T) {
	ready, sentinel := processTreeTestPaths(t)
	t.Setenv(processTreeTestModeEnv, "root")
	t.Setenv(processTreeTestReadyEnv, ready)
	t.Setenv(processTreeTestSentinelEnv, sentinel)
	runner := &Runner{
		command:         processTreeRunnerTestCommand(),
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV2,
	}
	process, err := runner.startProviderHostV2(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer process.abort()
	waitForProcessTreeReady(t, ready)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	command := newProviderHostCommand(
		"blocked-write-execution", 1, "SendTurn", "blocked-write-command",
		map[string]any{"padding": strings.Repeat("x", 1<<20)},
	)
	started := time.Now()
	if _, err := process.startCommand(ctx, command, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked Provider Host stdin returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("blocked Provider Host stdin ignored cancellation for %s", elapsed)
	}
	assertProcessTreeSentinelAbsent(t, sentinel)
}

func TestProviderHostControlTerminalWaitHonorsContext(t *testing.T) {
	ready, _ := processTreeTestPaths(t)
	observed := ready + ".observed"
	t.Setenv(processTreeTestModeEnv, "reader")
	t.Setenv(processTreeTestReadyEnv, ready)
	t.Setenv(processTreeTestObservedEnv, observed)
	runner := &Runner{
		command:         processTreeRunnerTestCommand(),
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV2,
	}
	process, err := runner.startProviderHostV2(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer process.abort()
	waitForProcessTreeReady(t, ready)
	input := providerHostV2TestInput(t)
	delivered := false
	acknowledged := false
	writtenBeforeDelivered := false
	control := RunnerControl{
		Command: RunnerControlCommand{
			Provider: input.Workload.Provider, CommandType: "SteerTurn", CommandID: "blocked-control-terminal",
			Payload: map[string]any{"inputText": "continue"},
		},
		MarkDelivered: func(context.Context) error {
			if _, err := os.Lstat(observed); err == nil {
				writtenBeforeDelivered = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			delivered = true
			return nil
		},
		Acknowledge: func(context.Context, map[string]any) error {
			acknowledged = true
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := process.executeControl(ctx, input, "active-send", control); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Provider Host control terminal wait returned %v", err)
	}
	if !delivered || acknowledged {
		t.Fatalf("unexpected durable control callbacks: delivered=%t acknowledged=%t", delivered, acknowledged)
	}
	if writtenBeforeDelivered {
		t.Fatal("durable Control Command reached Provider Host before delivered persistence")
	}
	waitForProcessTreeReady(t, observed)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Provider Host control terminal wait ignored cancellation for %s", elapsed)
	}
}

func TestProviderHostDoesNotWriteBeforeDeliveredPersistence(t *testing.T) {
	ready, _ := processTreeTestPaths(t)
	observed := ready + ".observed"
	t.Setenv(processTreeTestModeEnv, "reader")
	t.Setenv(processTreeTestReadyEnv, ready)
	t.Setenv(processTreeTestObservedEnv, observed)
	runner := &Runner{
		command:         processTreeRunnerTestCommand(),
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV2,
	}
	process, err := runner.startProviderHostV2(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer process.abort()
	waitForProcessTreeReady(t, ready)
	input := providerHostV2TestInput(t)
	persistenceErr := errors.New("injected delivered persistence failure")
	control := RunnerControl{
		Command: RunnerControlCommand{
			Provider: input.Workload.Provider, CommandType: "InterruptTurn", CommandID: "undelivered-control",
			Payload: map[string]any{},
		},
		MarkDelivered: func(context.Context) error { return persistenceErr },
		Acknowledge:   func(context.Context, map[string]any) error { return nil },
	}
	if _, err := process.executeControl(context.Background(), input, "active-send", control); !errors.Is(err, persistenceErr) {
		t.Fatalf("delivered persistence failure returned %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Lstat(observed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Provider Host received a command whose delivered state was not persisted: %v", err)
	}
}

func TestProviderHostCrashDuringDeliveredPersistenceDoesNotDoubleResolve(t *testing.T) {
	ready, _ := processTreeTestPaths(t)
	trigger := ready + ".exit"
	t.Setenv(processTreeTestModeEnv, "exit-trigger")
	t.Setenv(processTreeTestReadyEnv, ready)
	t.Setenv(processTreeTestObservedEnv, trigger)
	runner := &Runner{
		command:         processTreeRunnerTestCommand(),
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV2,
	}
	process, err := runner.startProviderHostV2(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer process.abort()
	waitForProcessTreeReady(t, ready)
	input := providerHostV2TestInput(t)
	persistenceErr := errors.New("injected delivered persistence failure after Host crash")
	control := RunnerControl{
		Command: RunnerControlCommand{
			Provider: input.Workload.Provider, CommandType: "SteerTurn", CommandID: "crashed-before-delivery",
			Payload: map[string]any{"inputText": "continue"},
		},
		MarkDelivered: func(context.Context) error {
			if err := os.WriteFile(trigger, []byte("exit"), 0o600); err != nil {
				return err
			}
			<-process.readerDone
			return persistenceErr
		},
		Acknowledge: func(context.Context, map[string]any) error { return nil },
	}
	if _, err := process.executeControl(context.Background(), input, "active-send", control); !errors.Is(err, persistenceErr) {
		t.Fatalf("Host crash during delivered persistence returned %v", err)
	}
}

func TestProcessTreeTerminateIsSingleShotAfterSuccessfulCleanup(t *testing.T) {
	t.Setenv(processTreeTestModeEnv, "sleep")
	commandLine := processTreeRunnerTestCommand()
	command := exec.Command(commandLine[0], commandLine[1:]...)
	command.Env = os.Environ()
	tree, err := newProcessTree(command)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.release()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := tree.started(); err != nil {
		t.Fatal(err)
	}
	if err := tree.terminate(); err != nil {
		t.Fatal(err)
	}
	if !tree.terminated {
		t.Fatal("process tree did not record successful termination")
	}
	if err := tree.terminate(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
}

func TestProcessTreeHelperProcess(t *testing.T) {
	if !containsString(os.Args, "--synara-process-tree-test-helper") {
		t.Skip("process-tree helper")
	}
	mode := processTreeTestArgument("--synara-process-tree-test-mode")
	ready := processTreeTestArgument("--synara-process-tree-test-ready")
	sentinel := processTreeTestArgument("--synara-process-tree-test-sentinel")
	observed := processTreeTestArgument("--synara-process-tree-test-observed")
	switch mode {
	case "root":
		commandLine := processTreeTestCommand("grandchild", ready, sentinel, observed)
		command := exec.Command(commandLine[0], commandLine[1:]...)
		command.Env = runnerEnvironment(os.Environ())
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			_ = os.WriteFile(ready, []byte("spawn grandchild: "+err.Error()), 0o600)
			os.Exit(2)
		}
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "grandchild":
		time.Sleep(processTreeGrandchildDelay)
		if err := os.WriteFile(sentinel, []byte("descendant survived"), 0o600); err != nil {
			os.Exit(4)
		}
		os.Exit(0)
	case "reader":
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(6)
		}
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if observed != "" {
				_ = os.WriteFile(observed, []byte("ready"), 0o600)
			}
		}
		os.Exit(0)
	case "exit-trigger":
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(7)
		}
		for {
			if _, err := os.Lstat(observed); err == nil {
				os.Exit(8)
			} else if !errors.Is(err, os.ErrNotExist) {
				os.Exit(9)
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "sleep":
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(5)
	}
}

func processTreeTestPaths(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "ready"), filepath.Join(root, "sentinel")
}

func waitForProcessTreeReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil {
			if string(content) != "ready" {
				t.Fatalf("process-tree helper failed before readiness: %s", content)
			}
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process-tree helper did not become ready")
}

func assertProcessTreeSentinelAbsent(t *testing.T, path string) {
	t.Helper()
	time.Sleep(processTreeGrandchildDelay + 300*time.Millisecond)
	content, err := os.ReadFile(path)
	if err == nil {
		t.Fatalf("Provider descendant outlived its owner and wrote %q", content)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func processTreeRunnerTestCommand() []string {
	return processTreeTestCommand(
		os.Getenv(processTreeTestModeEnv), os.Getenv(processTreeTestReadyEnv),
		os.Getenv(processTreeTestSentinelEnv), os.Getenv(processTreeTestObservedEnv),
	)
}

func processTreeTestCommand(mode, ready, sentinel, observed string) []string {
	command := []string{
		os.Args[0], "-test.run=^TestProcessTreeHelperProcess$", "--", "--synara-process-tree-test-helper",
		"--synara-process-tree-test-mode", mode,
	}
	for _, item := range []struct{ name, value string }{
		{name: "--synara-process-tree-test-ready", value: ready},
		{name: "--synara-process-tree-test-sentinel", value: sentinel},
		{name: "--synara-process-tree-test-observed", value: observed},
	} {
		if item.value != "" {
			command = append(command, item.name, item.value)
		}
	}
	return command
}

func processTreeTestArgument(name string) string {
	for index := 0; index+1 < len(os.Args); index++ {
		if os.Args[index] == name {
			return os.Args[index+1]
		}
	}
	return ""
}

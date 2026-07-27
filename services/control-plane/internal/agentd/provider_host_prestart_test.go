package agentd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestProviderHostPrestartRequiresWarmPoolWithoutProtectedCgroup(t *testing.T) {
	warm := Config{WorkerMode: executions.WorkerModeWarmPool, RunnerProtocol: RunnerProtocolV2}
	if !providerHostPrestartEnabled(warm) {
		t.Fatal("warm-pool Provider Host prestart was disabled without a protected cgroup root")
	}

	general := warm
	general.WorkerMode = executions.WorkerModeGeneralPool
	if providerHostPrestartEnabled(general) {
		t.Fatal("general-pool Worker enabled Provider Host prestart")
	}

	assignedExecutionID := uuid.New()
	pinned := warm
	pinned.WorkerMode = executions.WorkerModeExecutionPinned
	pinned.AssignedExecutionID = &assignedExecutionID
	if providerHostPrestartEnabled(pinned) {
		t.Fatal("execution-pinned Worker enabled Provider Host prestart")
	}

	protected := warm
	protected.CgroupV2Root = "/sys/fs/cgroup/synara"
	if providerHostPrestartEnabled(protected) {
		t.Fatal("protected cgroup-v2 Worker enabled Provider Host prestart")
	}

	legacy := warm
	legacy.RunnerProtocol = RunnerProtocolV1
	if providerHostPrestartEnabled(legacy) {
		t.Fatal("legacy Runner enabled Provider Host v2 prestart")
	}
}

func TestProviderHostPrestartAdoptsLiveProcessAndDeliversCredential(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")
	processLog := filepath.Join(t.TempDir(), "processes.log")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	credentialLog := filepath.Join(t.TempDir(), "credentials.log")
	t.Setenv("PROVIDER_HOST_TEST_PROCESS_LOG", processLog)
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)
	t.Setenv("PROVIDER_HOST_TEST_CREDENTIAL_LOG", credentialLog)

	runner := providerHostV2TestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.startProviderHostV2Prestart(ctx, 2*time.Second)
	defer runner.stopProviderHostV2Prestart()

	result, err := runner.Run(
		context.Background(),
		providerHostV2TestInput(t),
		&RunnerCredential{Payload: map[string]any{"apiKey": "provider-secret"}},
		func(context.Context, RunnerMessage) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["text"] != "done" {
		t.Fatalf("unexpected adopted Provider Host result: %#v", result)
	}
	if lines := readProviderHostTestLog(t, processLog); len(lines) != 1 {
		t.Fatalf("Provider Host process starts = %d, want 1: %#v", len(lines), lines)
	}
	commands := readProviderHostTestLog(t, commandLog)
	if countProviderHostTestCommand(commands, "Describe") != len(providerHostProviders) {
		t.Fatalf("adopted Host issued a second Describe: %#v", commands)
	}
	if len(commands) != len(providerHostProviders)+2 ||
		commands[len(commands)-2] != "StartSession" || commands[len(commands)-1] != "SendTurn" {
		t.Fatalf("unexpected adopted Provider Host commands: %#v", commands)
	}
	if credentials := readProviderHostTestLog(t, credentialLog); len(credentials) != 1 || credentials[0] != "provider-secret" {
		t.Fatalf("credential delivery after prestart = %#v", credentials)
	}
}

func TestProviderHostPrestartMismatchFallsBackToNormalSpawn(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "prestart-mismatch")
	processLog := filepath.Join(t.TempDir(), "processes.log")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	credentialLog := filepath.Join(t.TempDir(), "credentials.log")
	t.Setenv("PROVIDER_HOST_TEST_PROCESS_LOG", processLog)
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)
	t.Setenv("PROVIDER_HOST_TEST_CREDENTIAL_LOG", credentialLog)

	runner := providerHostV2TestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.startProviderHostV2Prestart(ctx, 2*time.Second)

	result, err := runner.Run(
		context.Background(),
		providerHostV2TestInput(t),
		&RunnerCredential{Payload: map[string]any{"apiKey": "provider-secret"}},
		func(context.Context, RunnerMessage) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["text"] != "done" {
		t.Fatalf("unexpected fallback Provider Host result: %#v", result)
	}
	if lines := readProviderHostTestLog(t, processLog); len(lines) != 2 {
		t.Fatalf("Provider Host process starts = %d, want prestart plus fallback: %#v", len(lines), lines)
	}
	commands := readProviderHostTestLog(t, commandLog)
	if countProviderHostTestCommand(commands, "Describe") != len(providerHostProviders)+1 {
		t.Fatalf("fallback did not use the normal Describe path: %#v", commands)
	}
	if credentials := readProviderHostTestLog(t, credentialLog); len(credentials) != 1 {
		t.Fatalf("credential was written to the rejected prestart: %#v", credentials)
	}
}

func TestProviderHostPrestartCancellationTearsDownWithoutCredentialWrite(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")
	processLog := filepath.Join(t.TempDir(), "processes.log")
	credentialLog := filepath.Join(t.TempDir(), "credentials.log")
	t.Setenv("PROVIDER_HOST_TEST_PROCESS_LOG", processLog)
	t.Setenv("PROVIDER_HOST_TEST_CREDENTIAL_LOG", credentialLog)

	runner := providerHostV2TestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	runner.startProviderHostV2Prestart(ctx, 2*time.Second)
	runner.prestartMu.Lock()
	manager := runner.providerHostPrestart
	manager.mu.Lock()
	process := manager.host.process
	manager.mu.Unlock()
	runner.prestartMu.Unlock()

	cancel()
	runner.stopProviderHostV2Prestart()
	if process.command.ProcessState == nil {
		t.Fatal("prestarted Provider Host process survived cancellation")
	}
	if lines := readProviderHostTestLog(t, processLog); len(lines) != 1 {
		t.Fatalf("Provider Host process starts = %d, want 1: %#v", len(lines), lines)
	}
	if _, err := os.Stat(credentialLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prestart teardown wrote credential bytes: %v", err)
	}
}

func TestProviderHostPrestartCrashUsesBoundedRespawn(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "prestart-crash")
	processLog := filepath.Join(t.TempDir(), "processes.log")
	t.Setenv("PROVIDER_HOST_TEST_PROCESS_LOG", processLog)

	runner := providerHostV2TestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.startProviderHostV2Prestart(ctx, 2*time.Second)
	runner.prestartMu.Lock()
	manager := runner.providerHostPrestart
	runner.prestartMu.Unlock()
	select {
	case <-manager.done:
	case <-time.After(5 * time.Second):
		t.Fatal("prestarted Provider Host did not stop after bounded crash retries")
	}
	runner.stopProviderHostV2Prestart()

	if lines := readProviderHostTestLog(t, processLog); len(lines) != providerHostPrestartMaximumAttempts {
		t.Fatalf(
			"Provider Host crash starts = %d, want bounded %d: %#v",
			len(lines), providerHostPrestartMaximumAttempts, lines,
		)
	}
}

func readProviderHostTestLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func countProviderHostTestCommand(commands []string, command string) int {
	count := 0
	for _, candidate := range commands {
		if candidate == command {
			count++
		}
	}
	return count
}

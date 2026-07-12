package agentd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestRunnerEmitsEventsArtifactsAndResult(t *testing.T) {
	workspace := t.TempDir()
	runner := &Runner{
		command: []string{"sh", "-c", `
cat >/dev/null
printf 'artifact payload' > result.txt
printf '%s\n' '{"type":"event","eventType":"runtime.output.delta","payload":{"text":"hello"}}'
printf '%s\n' '{"type":"artifact","artifact":{"path":"result.txt","kind":"generated_file","contentType":"text/plain"}}'
printf '%s\n' '{"type":"result","output":{"summary":"done"},"providerResumeCursor":"resume-1"}'
`},
		maxMessageBytes: 1 << 20,
	}
	messages := make([]RunnerMessage, 0, 2)
	result, err := runner.Run(context.Background(), RunnerInput{
		Execution: executions.Execution{ID: uuid.New()},
		Workload:  executions.Workload{InputText: "run"}, WorkspaceDirectory: workspace,
	}, nil, func(_ context.Context, message RunnerMessage) error {
		messages = append(messages, message)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Type != "event" || messages[1].Artifact == nil {
		t.Fatalf("unexpected runner messages: %#v", messages)
	}
	if result.Output["summary"] != "done" || result.ProviderResumeCursor == nil || *result.ProviderResumeCursor != "resume-1" {
		t.Fatalf("unexpected runner result: %#v", result)
	}
	if payload, err := os.ReadFile(filepath.Join(workspace, "result.txt")); err != nil || string(payload) != "artifact payload" {
		t.Fatalf("runner artifact missing: %q, %v", payload, err)
	}
}

func TestRunnerDeliversCredentialOnlyThroughPipeAndStripsWorkerEnvironment(t *testing.T) {
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "registration-secret")
	t.Setenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", uuid.NewString())
	workspace := t.TempDir()
	runner := &Runner{
		command: []string{"sh", "-c", `
cat <&3 > credential.json
test -z "${SYNARA_WORKER_REGISTRATION_TOKEN:-}"
test -z "${SYNARA_AGENTD_ASSIGNED_EXECUTION_ID:-}"
grep -q 'provider-secret' credential.json
rm credential.json
printf '%s\n' '{"type":"result","output":{"summary":"credential received"}}'
`},
		maxMessageBytes: 1 << 20,
	}
	result, err := runner.Run(context.Background(), RunnerInput{
		Execution:          executions.Execution{ID: uuid.New()},
		Workload:           executions.Workload{Provider: "codex", InputText: "run"},
		WorkspaceDirectory: workspace,
	}, &RunnerCredential{Payload: map[string]any{"apiKey": "provider-secret"}}, func(context.Context, RunnerMessage) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["summary"] != "credential received" {
		t.Fatalf("unexpected runner result: %#v", result)
	}
}

func TestResolveWorkspaceArtifactRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorkspaceArtifact(workspace, "escape.txt"); err == nil {
		t.Fatal("agentd accepted an artifact symlink outside the execution workspace")
	}
}

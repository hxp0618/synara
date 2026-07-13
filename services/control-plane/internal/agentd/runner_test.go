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

func TestLegacyRunnerCannotAdvertiseRuntimeEventV2(t *testing.T) {
	runner := &Runner{
		command: []string{"sh", "-c", `
cat >/dev/null
printf '%s\n' '{"type":"event","eventVersion":2,"eventType":"content.delta","payload":{"streamKind":"assistant_text","delta":"hello"}}'
printf '%s\n' '{"type":"result","output":{}}'
`},
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV1,
	}
	_, err := runner.Run(context.Background(), RunnerInput{
		Execution: executions.Execution{ID: uuid.New()}, WorkspaceDirectory: t.TempDir(),
	}, nil, func(context.Context, RunnerMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("legacy runner advertised Runtime Event v2: %v", err)
	}
}

func TestRunnerEnvironmentUsesExplicitRuntimeAllowlist(t *testing.T) {
	source := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/home/runner",
		"TMPDIR=/tmp/synara",
		"LANG=en_US.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"SSL_CERT_FILE=/etc/ssl/cert.pem",
		"SECRET=ordinary-secret",
		"SYNARA_HOST_SECRET=synara-secret",
		"SYNARA_AUTH_TOKEN=auth-secret",
		"SYNARA_WORKER_REGISTRATION_TOKEN=worker-secret",
		"SYNARA_LEASE_TOKEN=lease-secret",
		"SYNARA_CONTROL_PLANE_URL=https://control.example.test",
		"OPENAI_API_KEY=openai-secret",
		"ANTHROPIC_API_KEY=anthropic-secret",
		"AWS_ACCESS_KEY_ID=aws-key",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"GITHUB_TOKEN=github-secret",
		"GH_TOKEN=gh-secret",
		"DATABASE_URL=postgres://user:secret@db/synara",
		"PGPASSWORD=postgres-secret",
		"S3_ACCESS_KEY_ID=s3-key",
		"MINIO_ROOT_PASSWORD=minio-secret",
		"GOOGLE_APPLICATION_CREDENTIALS=/host/gcp-credential.json",
		"AZURE_CLIENT_SECRET=azure-secret",
		"HTTP_PROXY=http://user:secret@proxy.example.test",
		"SSH_AUTH_SOCK=/host/agent.sock",
		"NODE_OPTIONS=--require=/host/inject-secrets.js",
	}

	actual := make(map[string]string)
	for _, entry := range runnerEnvironment(source) {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("invalid child environment entry %q", entry)
		}
		actual[name] = value
	}
	want := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/home/runner", "TMPDIR": "/tmp/synara",
		"LANG": "en_US.UTF-8", "LC_ALL": "C.UTF-8", "TERM": "xterm-256color",
		"COLORTERM": "truecolor", "SSL_CERT_FILE": "/etc/ssl/cert.pem",
	}
	if len(actual) != len(want) {
		t.Fatalf("Runner child environment contains unexpected entries: %#v", actual)
	}
	for name, value := range want {
		if actual[name] != value {
			t.Fatalf("Runner child environment %s = %q, want %q", name, actual[name], value)
		}
	}
}

func TestRunnerDeliversCredentialOnlyThroughPipeAndStripsWorkerEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("SECRET", "ordinary-secret")
	t.Setenv("SYNARA_HOST_SECRET", "synara-secret")
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "registration-secret")
	t.Setenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", uuid.NewString())
	t.Setenv("SYNARA_LEASE_TOKEN", "lease-secret")
	t.Setenv("SYNARA_CONTROL_PLANE_URL", "https://control.example.test")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("DATABASE_URL", "postgres://user:secret@db/synara")
	t.Setenv("PGPASSWORD", "postgres-secret")
	t.Setenv("MINIO_ROOT_PASSWORD", "minio-secret")
	workspace := t.TempDir()
	runner := &Runner{
		command: []string{"sh", "-c", `
cat <&3 > credential.json
test -n "${PATH:-}"
test -n "${HOME:-}"
test -n "${TMPDIR:-}"
test "${LANG:-}" = "C.UTF-8"
test "${TERM:-}" = "xterm-256color"
test -z "${SECRET:-}"
test -z "${SYNARA_HOST_SECRET:-}"
test -z "${SYNARA_WORKER_REGISTRATION_TOKEN:-}"
test -z "${SYNARA_AGENTD_ASSIGNED_EXECUTION_ID:-}"
test -z "${SYNARA_LEASE_TOKEN:-}"
test -z "${SYNARA_CONTROL_PLANE_URL:-}"
test -z "${AWS_ACCESS_KEY_ID:-}"
test -z "${AWS_SECRET_ACCESS_KEY:-}"
test -z "${GITHUB_TOKEN:-}"
test -z "${DATABASE_URL:-}"
test -z "${PGPASSWORD:-}"
test -z "${MINIO_ROOT_PASSWORD:-}"
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

func TestRetryInteractionDeliveryUpdateRecoversTransientResponseLoss(t *testing.T) {
	daemon := &Daemon{config: Config{RequestTimeout: time.Second}}
	attempts := 0
	err := daemon.retryInteractionDeliveryUpdate(context.Background(), func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary response loss")
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("delivery update retry = %d attempts, %v", attempts, err)
	}
}

func TestBuildGitSHAValidationMatchesPersistedManifestConstraint(t *testing.T) {
	for value, want := range map[string]bool{
		"abcdef0": true, strings.Repeat("a", 64): true,
		"ABCDEF0": false, "not-a-sha": false, "abcdef": false,
	} {
		if actual := validBuildGitSHA(value); actual != want {
			t.Fatalf("validBuildGitSHA(%q) = %t, want %t", value, actual, want)
		}
	}
}

func TestWithProviderHostCapabilitiesIncludesWorkerRuntimeManifest(t *testing.T) {
	providerHost := map[string]any{"protocolVersion": map[string]any{"major": 2, "minor": 0}}
	result := withProviderHostCapabilities(map[string]any{"gpu": false}, providerHost, Config{
		Version: "agentd-test", BuildGitSHA: "abcdef0", ImageDigest: "sha256:test", RunnerProtocol: RunnerProtocolV2,
	})
	runtimeManifest, ok := result["workerRuntime"].(map[string]any)
	if !ok || runtimeManifest["workerBuildVersion"] != "agentd-test" ||
		runtimeManifest["workerBuildGitSha"] != "abcdef0" || runtimeManifest["imageDigest"] != "sha256:test" ||
		runtimeManifest["workerProtocolMinimum"] != 2 || runtimeManifest["workerProtocolMaximum"] != 2 ||
		runtimeManifest["runtimeEventMinimum"] != executions.RuntimeEventVersionV2 ||
		runtimeManifest["runtimeEventMaximum"] != executions.RuntimeEventVersionV2 {
		t.Fatalf("worker runtime manifest was not advertised: %#v", result)
	}
	if result["providerHost"] == nil || result["gpu"] != false {
		t.Fatalf("worker capabilities were not merged: %#v", result)
	}
}

func TestWithProviderHostCapabilitiesKeepsExplicitLegacyRuntimeEventV1(t *testing.T) {
	result := withProviderHostCapabilities(nil, map[string]any{"legacy": true}, Config{
		Version: "agentd-legacy", RunnerProtocol: RunnerProtocolV1,
	})
	runtimeManifest := result["workerRuntime"].(map[string]any)
	if runtimeManifest["runtimeEventMinimum"] != executions.RuntimeEventVersionV1 ||
		runtimeManifest["runtimeEventMaximum"] != executions.RuntimeEventVersionV1 {
		t.Fatalf("legacy runner advertised a non-v1 Runtime Event range: %#v", runtimeManifest)
	}
}

func TestCanonicalInteractionRuntimeEventUsesNegotiatedV2(t *testing.T) {
	approval, err := canonicalInteractionRuntimeEvent(RunnerMessage{
		Type: "interaction", EventVersion: executions.RuntimeEventVersionV2,
		Payload: map[string]any{
			"interactionType": "approval", "requestId": "approval-1", "requestKind": "file-read", "summary": "Read file",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if approval.EventType != "request.opened" || approval.Payload["requestType"] != "file_read_approval" ||
		approval.Payload["detail"] != "Read file" {
		t.Fatalf("unexpected canonical approval event: %#v", approval)
	}

	userInput, err := canonicalInteractionRuntimeEvent(RunnerMessage{
		Type: "interaction", EventVersion: executions.RuntimeEventVersionV2,
		Payload: map[string]any{
			"interactionType": "user-input", "requestId": "input-1",
			"questions": []any{map[string]any{
				"id": "q1", "header": "Choice", "question": "Pick one",
				"options": []any{map[string]any{"label": "A", "description": ""}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	questions, ok := userInput.Payload["questions"].([]any)
	if userInput.EventType != "user-input.requested" || !ok || len(questions) != 1 {
		t.Fatalf("unexpected canonical user-input event: %#v", userInput)
	}
	question := questions[0].(map[string]any)
	options, ok := question["options"].([]any)
	if !ok || len(options) != 1 || options[0].(map[string]any)["description"] != "A" {
		t.Fatalf("empty option description was not normalized to a canonical fallback: %#v", question)
	}

	if _, err := canonicalInteractionRuntimeEvent(RunnerMessage{
		Type: "interaction", EventVersion: executions.RuntimeEventVersionV1,
		Payload: map[string]any{"interactionType": "approval", "requestId": "legacy"},
	}); runnerFailureCode(err) != "protocol_violation" {
		t.Fatalf("v1 interaction bypassed v2 negotiation: %v", err)
	}
}

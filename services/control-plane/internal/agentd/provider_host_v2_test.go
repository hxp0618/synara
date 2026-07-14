package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestRunnerProviderHostV2NegotiatesAndRunsResumeTurn(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("SECRET", "ordinary-secret")
	t.Setenv("HOST_SECRET", "host-secret")
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "worker-secret")
	t.Setenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", uuid.NewString())
	t.Setenv("SYNARA_LEASE_TOKEN", "lease-secret")
	t.Setenv("SYNARA_CONTROL_PLANE_URL", "https://control.example.test")
	t.Setenv("OPENAI_API_KEY", "ambient-openai-secret")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-anthropic-secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("DATABASE_URL", "postgres://user:secret@db/synara")
	t.Setenv("PGPASSWORD", "postgres-secret")
	t.Setenv("MINIO_ROOT_PASSWORD", "minio-secret")
	t.Setenv("HTTP_PROXY", "http://ambient-user:ambient-secret@proxy.example.test")
	t.Setenv("SYNARA_PROVIDER_HTTP_PROXY", "http://provider-user:provider-secret@proxy.example.test")
	t.Setenv("SYNARA_PROVIDER_NO_PROXY", "127.0.0.1,localhost")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	runner := providerHostV2TestRunner()
	input := providerHostV2TestInput(t)
	input.ProviderResumeCursor = stringPointer("native-cursor")
	input.Workload.ConversationHistory = []executions.ConversationMessage{{Role: "user", Text: "earlier"}}
	credential := &RunnerCredential{Payload: map[string]any{"apiKey": "provider-secret"}}
	var messages []RunnerMessage
	result, err := runner.Run(context.Background(), input, credential, func(_ context.Context, message RunnerMessage) error {
		messages = append(messages, message)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["text"] != "done" || result.ProviderResumeCursor == nil || *result.ProviderResumeCursor != "cursor-next" {
		t.Fatalf("unexpected Provider Host result: %#v", result)
	}
	if len(messages) != 1 || messages[0].Type != "event" ||
		messages[0].EventVersion != executions.RuntimeEventVersionV2 || messages[0].EventType != "content.delta" ||
		messages[0].Payload["streamKind"] != "assistant_text" || messages[0].Payload["delta"] != "hello" {
		t.Fatalf("unexpected Provider Host messages: %#v", messages)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nResumeSession\nSendTurn\n" {
		t.Fatalf("unexpected Provider Host command sequence %q", commands)
	}
}

func TestRunnerMessageFromProviderHostDerivesStableProviderResumeFallbackEventIDForReplay(t *testing.T) {
	payload := map[string]any{
		"message": "Native Provider resume failed before turn activity; authoritative-history fallback selected.",
		"detail": map[string]any{
			"kind":                         "session_resume",
			"attemptedStrategy":            "native-cursor",
			"selectedStrategy":             "authoritative-history",
			"outcome":                      "fallback_selected",
			"reasonCode":                   "session_resume_invalid",
			"fallbackSafety":               "before_turn_activity",
			"authoritativeHistorySequence": float64(31),
			"provider":                     "codex",
		},
	}
	message := providerHostMessage{
		RequestID: "request-1", ProtocolVersion: providerHostProtocolVersion{Major: 2, Minor: 1},
		ExecutionID: uuid.NewString(), Generation: 3, CommandID: "send:stable-command",
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), MessageType: "Event",
		Payload: map[string]any{
			"eventVersion": float64(executions.RuntimeEventVersionV2),
			"eventType":    "runtime.warning",
			"payload":      payload,
		},
	}
	first, err := runnerMessageFromProviderHost(message, executions.RuntimeEventVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	message.RequestID = "request-2"
	message.OccurredAt = time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	replayed, err := runnerMessageFromProviderHost(message, executions.RuntimeEventVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventID == nil || replayed.EventID == nil || *first.EventID != *replayed.EventID {
		t.Fatalf("fallback replay Event IDs differ: first=%v replayed=%v", first.EventID, replayed.EventID)
	}

	changedReason := message
	changedPayload := map[string]any{}
	for key, value := range payload {
		changedPayload[key] = value
	}
	changedDetail := map[string]any{}
	for key, value := range payload["detail"].(map[string]any) {
		changedDetail[key] = value
	}
	changedDetail["reasonCode"] = "session_resume_expired"
	changedPayload["detail"] = changedDetail
	changedReason.Payload = map[string]any{
		"eventVersion": float64(executions.RuntimeEventVersionV2),
		"eventType":    "runtime.warning",
		"payload":      changedPayload,
	}
	changedReasonMessage, err := runnerMessageFromProviderHost(changedReason, executions.RuntimeEventVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	if changedReasonMessage.EventID == nil || *changedReasonMessage.EventID != *first.EventID {
		t.Fatalf("same Send fallback semantic slot changed Event ID: first=%v changed=%v", first.EventID, changedReasonMessage.EventID)
	}

	changedGeneration := message
	changedGeneration.Generation++
	nextGeneration, err := runnerMessageFromProviderHost(changedGeneration, executions.RuntimeEventVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	changedCommand := message
	changedCommand.CommandID = "send:next-command"
	nextCommand, err := runnerMessageFromProviderHost(changedCommand, executions.RuntimeEventVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	if nextGeneration.EventID == nil || nextCommand.EventID == nil ||
		*nextGeneration.EventID == *first.EventID || *nextCommand.EventID == *first.EventID {
		t.Fatalf("fallback Event ID was not scoped by generation and Send command: first=%v generation=%v command=%v",
			first.EventID, nextGeneration.EventID, nextCommand.EventID)
	}
}

func TestRunnerMessageFromProviderHostLeavesOtherRuntimeEventsWithoutDerivedEventID(t *testing.T) {
	message := providerHostMessage{
		RequestID: "request-1", ProtocolVersion: providerHostProtocolVersion{Major: 2, Minor: 1},
		ExecutionID: uuid.NewString(), Generation: 1, CommandID: "send:ordinary-warning",
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), MessageType: "Event",
		Payload: map[string]any{
			"eventVersion": float64(executions.RuntimeEventVersionV2),
			"eventType":    "runtime.warning",
			"payload": map[string]any{
				"message": "Provider emitted an unknown native event",
				"detail":  map[string]any{"provider": "codex"},
			},
		},
	}
	runnerMessage, err := runnerMessageFromProviderHost(message, executions.RuntimeEventVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	if runnerMessage.EventID != nil {
		t.Fatalf("ordinary Runtime Event received a derived Event ID: %v", runnerMessage.EventID)
	}
}

func TestRunnerProviderHostV2DeliversInteractionResolutionDuringSend(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "interaction")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	input := providerHostV2TestInput(t)
	controls := make(chan RunnerControl, 1)
	done := make(chan error, 1)
	markedDelivered := false
	acknowledged := false
	result, err := providerHostV2TestRunner().RunControlled(
		context.Background(), input, nil, controls,
		func(_ context.Context, message RunnerMessage) error {
			if message.Type != "interaction" || message.EventVersion != executions.RuntimeEventVersionV2 {
				return fmt.Errorf("unexpected Runner message %#v", message)
			}
			controls <- RunnerControl{
				Command: RunnerControlCommand{
					Provider: "codex", CommandType: "ResolveApproval", CommandID: "approval-1:resolution",
					Payload: map[string]any{
						"interactionId": uuid.NewString(), "requestId": "approval-1",
						"resolutionKind": "approved", "resolution": map[string]any{"decision": "accept"},
					},
				},
				MarkDelivered: func(context.Context) error {
					markedDelivered = true
					return nil
				},
				Acknowledge: func(context.Context, map[string]any) error {
					acknowledged = true
					return nil
				},
				Done: done,
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["text"] != "approved" || !markedDelivered || !acknowledged {
		t.Fatalf("interaction resolution did not complete: result=%#v delivered=%t acknowledged=%t", result, markedDelivered, acknowledged)
	}
	select {
	case controlErr := <-done:
		if controlErr != nil {
			t.Fatalf("interaction control failed: %v", controlErr)
		}
	default:
		t.Fatal("interaction control completion was not reported")
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\nResolveApproval\n" {
		t.Fatalf("unexpected concurrent Provider Host command sequence %q", commands)
	}
}

func TestRunnerProviderHostV2DeliversDurableInterruptDuringSend(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "interrupt")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	input := providerHostV2TestInput(t)
	controls := make(chan RunnerControl, 1)
	done := make(chan error, 1)
	markedDelivered := false
	acknowledgedCursor := ""
	result, err := providerHostV2TestRunner().RunControlled(
		context.Background(), input, nil, controls,
		func(_ context.Context, message RunnerMessage) error {
			if message.Type == "progress" {
				controls <- RunnerControl{
					Command: RunnerControlCommand{
						Provider: "codex", CommandType: "InterruptTurn", CommandID: "interrupt:durable",
						Payload: map[string]any{"turnId": input.Workload.TurnID.String()},
					},
					MarkDelivered: func(context.Context) error {
						markedDelivered = true
						return nil
					},
					Acknowledge: func(_ context.Context, payload map[string]any) error {
						acknowledgedCursor, _ = payload["providerResumeCursor"].(string)
						return nil
					},
					Done: done,
				}
			}
			return nil
		},
	)
	if result.Output != nil || runnerFailureCode(err) != "interrupted" || !runnerFailurePersisted(err) {
		t.Fatalf("unexpected durable interrupt result=%#v err=%v", result, err)
	}
	if !markedDelivered || acknowledgedCursor != "cursor-interrupted" {
		t.Fatalf("durable interrupt delivery was incomplete: delivered=%t cursor=%q", markedDelivered, acknowledgedCursor)
	}
	select {
	case controlErr := <-done:
		if controlErr != nil {
			t.Fatalf("interrupt control failed: %v", controlErr)
		}
	default:
		t.Fatal("interrupt control completion was not reported")
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\nInterruptTurn\n" {
		t.Fatalf("unexpected interrupt command sequence %q", commands)
	}
}

func TestRunnerProviderHostV2DeliversDurableSteerDuringSend(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "steer")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	input := providerHostV2TestInput(t)
	controls := make(chan RunnerControl, 1)
	done := make(chan error, 1)
	markedDelivered := false
	acknowledged := false
	result, err := providerHostV2TestRunner().RunControlled(
		context.Background(), input, nil, controls,
		func(_ context.Context, message RunnerMessage) error {
			if message.Type == "progress" {
				controls <- RunnerControl{
					Command: RunnerControlCommand{
						Provider: "codex", CommandType: "SteerTurn", CommandID: "steer:durable",
						Payload: map[string]any{
							"turnId": input.Workload.TurnID.String(), "inputText": "focus on tests",
						},
					},
					MarkDelivered: func(context.Context) error {
						markedDelivered = true
						return nil
					},
					Acknowledge: func(context.Context, map[string]any) error {
						acknowledged = true
						return nil
					},
					Done: done,
				}
			}
			return nil
		},
	)
	if err != nil || result.Output["text"] != "steered" || !markedDelivered || !acknowledged {
		t.Fatalf("durable Steer failed: result=%#v err=%v delivered=%t acknowledged=%t",
			result, err, markedDelivered, acknowledged)
	}
	select {
	case controlErr := <-done:
		if controlErr != nil {
			t.Fatalf("Steer control failed: %v", controlErr)
		}
	default:
		t.Fatal("Steer control completion was not reported")
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\nSteerTurn\n" {
		t.Fatalf("unexpected Steer command sequence %q", commands)
	}
}

func TestRunnerProviderHostV2InterruptsProviderBeforeHostShutdown(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "interrupt")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)
	ctx, cancel := context.WithCancel(context.Background())

	_, err := providerHostV2TestRunner().Run(
		ctx, providerHostV2TestInput(t), nil,
		func(_ context.Context, message RunnerMessage) error {
			if message.Type == "progress" {
				cancel()
			}
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled Runner after graceful interrupt, got %v", err)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\nInterruptTurn\n" {
		t.Fatalf("Provider was not interrupted before Host shutdown: %q", commands)
	}
}

func TestRunnerProviderHostV2RejectsMissingCapabilityBeforeSessionCommand(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "missing-capability")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "capability_unsupported" {
		t.Fatalf("expected capability_unsupported, got %v", err)
	}
	commands, readErr := os.ReadFile(commandLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(commands) != "Describe\n" {
		t.Fatalf("Provider command ran before capability rejection: %q", commands)
	}
}

func TestRunnerProviderHostV2RejectsIncompatibleMajor(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "incompatible-major")

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "provider_version_incompatible" {
		t.Fatalf("expected provider_version_incompatible, got %v", err)
	}
}

func TestRunnerProviderHostV2RejectsIncompatibleRuntimeEventRange(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "runtime-event-v1-only")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "provider_version_incompatible" {
		t.Fatalf("expected provider_version_incompatible, got %v", err)
	}
	commands, readErr := os.ReadFile(commandLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(commands) != "Describe\n" {
		t.Fatalf("Provider command ran before Runtime Event negotiation rejection: %q", commands)
	}
}

func TestRunnerProviderHostV2RejectsInvalidCanonicalEventFrames(t *testing.T) {
	for _, mode := range []string{
		"event-version-missing", "event-version-v1", "event-type-unknown",
		"event-payload-not-object", "event-payload-invalid-shape", "event-payload-too-large",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
			t.Setenv("PROVIDER_HOST_TEST_MODE", mode)
			_, err := providerHostV2TestRunner().Run(
				context.Background(), providerHostV2TestInput(t), nil,
				func(context.Context, RunnerMessage) error { return nil },
			)
			if runnerFailureCode(err) != "protocol_violation" {
				t.Fatalf("expected protocol_violation, got %v", err)
			}
		})
	}
}

func TestRunnerProviderHostV2PreservesStableHostError(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "provider-error")

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	var failure *runnerFailure
	if !errors.As(err, &failure) || failure.code != "provider_rate_limited" || !failure.retryable ||
		!failure.requiresNewExecution || !failure.canMoveWorker {
		t.Fatalf("unexpected stable Provider Host error: %#v, %v", failure, err)
	}
}

func TestRunnerProviderHostV2ClassifiesMalformedJSONL(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "malformed")

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "protocol_violation" {
		t.Fatalf("expected protocol_violation, got %v", err)
	}
}

func TestRunnerProviderHostV2ClassifiesHostCrash(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "crash")

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "provider_unavailable" || !strings.Contains(err.Error(), "simulated Host crash") {
		t.Fatalf("expected provider_unavailable Host crash, got %v", err)
	}
}

func TestRunnerProviderHostV2ClassifiesOversizedJSONL(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "oversized")
	runner := providerHostV2TestRunner()
	runner.maxMessageBytes = 1 << 10

	_, err := runner.Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "protocol_violation" {
		t.Fatalf("expected oversized protocol_violation, got %v", err)
	}
}

func TestRunnerProviderHostV2RejectsOutputAfterTerminal(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "output-after-terminal")

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "protocol_violation" {
		t.Fatalf("expected trailing output protocol_violation, got %v", err)
	}
}

func TestRunnerCapabilitySummaryUsesProviderHostDescribe(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")

	summary, err := providerHostV2TestRunner().CapabilitySummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	protocol, ok := summary["protocolVersion"].(map[string]any)
	if !ok || protocol["major"] != providerHostProtocolMajor || summary["legacy"] != false {
		t.Fatalf("unexpected Provider Host summary: %#v", summary)
	}
	providers, ok := summary["providers"].(map[string]any)
	if !ok || providers["codex"] == nil || providers["claudeAgent"] == nil {
		t.Fatalf("Provider Host summary omitted remote Providers: %#v", summary)
	}
	codex, ok := providers["codex"].(providerHostDescriptor)
	if !ok || codex.RuntimeEventVersions.Minimum != providerHostRuntimeEventVersion ||
		codex.RuntimeEventVersions.Maximum != providerHostRuntimeEventVersion || codex.ProtocolVersion.Minor < 1 ||
		codex.CapabilityDescriptor.Runtime == nil || codex.CapabilityDescriptor.ReleasePolicy == nil ||
		!codex.CapabilityDescriptor.ReleasePolicy.Enabled {
		t.Fatalf("Provider Host summary advertised an unexpected Runtime Event range: %#v", providers["codex"])
	}
}

func TestRunnerProviderHostV2ExperimentalProviderDefaultsDisabled(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	_, err := providerHostV2TestRunnerWithExperimental().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "capability_unsupported" {
		t.Fatalf("disabled experimental Provider returned %v", err)
	}
	commands, readErr := os.ReadFile(commandLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(commands) != "Describe\n" {
		t.Fatalf("disabled experimental Provider ran before rejection: %q", commands)
	}
}

func TestRunnerProviderHostV2ExplicitEnablementOverridesInheritedEnvironment(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")
	t.Setenv(providerHostExperimentalEnv, "pi")

	result, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if err != nil || result.Output["text"] != "done" {
		t.Fatalf("explicit experimental Provider policy was not injected: result=%#v err=%v", result, err)
	}
}

func TestProviderHostEnvironmentUsesExplicitRuntimeAndPolicyAllowlist(t *testing.T) {
	source := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/home/worker",
		"TMPDIR=/tmp/synara",
		"LANG=en_US.UTF-8",
		"TERM=xterm-256color",
		"SYNARA_PROVIDER_HOST_BUILD_VERSION=ambient-build-must-not-win",
		providerHostExperimentalEnv + "=ambient-provider-must-not-win",
		"HTTP_PROXY=http://ambient-user:ambient-secret@proxy.example.test",
		"HTTPS_PROXY=https://ambient-user:ambient-secret@proxy.example.test",
		"ALL_PROXY=socks5://ambient-user:ambient-secret@proxy.example.test",
		"NO_PROXY=ambient.internal",
		"SYNARA_PROVIDER_HTTP_PROXY=http://provider-user:provider-secret@proxy.example.test",
		"SYNARA_PROVIDER_HTTPS_PROXY=https://provider-user:provider-secret@proxy.example.test",
		"SYNARA_PROVIDER_ALL_PROXY=socks5://provider-user:provider-secret@proxy.example.test",
		"SYNARA_PROVIDER_NO_PROXY=127.0.0.1,localhost",
		"SECRET=ordinary-secret",
		"HOST_SECRET=host-secret",
		"SYNARA_AUTH_TOKEN=auth-secret",
		"SYNARA_WORKER_TOKEN=worker-secret",
		"SYNARA_LEASE_TOKEN=lease-secret",
		"SYNARA_CONTROL_PLANE_URL=https://control.example.test",
		"OPENAI_API_KEY=openai-secret",
		"ANTHROPIC_API_KEY=anthropic-secret",
		"AWS_ACCESS_KEY_ID=aws-key",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"GITHUB_TOKEN=github-secret",
		"DATABASE_URL=postgres://user:secret@db/synara",
		"PGPASSWORD=postgres-secret",
		"MINIO_ROOT_PASSWORD=minio-secret",
	}

	actual := make(map[string]string)
	for _, entry := range providerHostEnvironment(source, []string{"claudeAgent", "codex"}) {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("invalid Provider Host environment entry %q", entry)
		}
		actual[name] = value
	}
	want := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/home/worker", "TMPDIR": "/tmp/synara",
		"LANG": "en_US.UTF-8", "TERM": "xterm-256color",
		providerHostExperimentalEnv:   "claudeAgent,codex",
		"SYNARA_PROVIDER_HTTP_PROXY":  "http://provider-user:provider-secret@proxy.example.test",
		"SYNARA_PROVIDER_HTTPS_PROXY": "https://provider-user:provider-secret@proxy.example.test",
		"SYNARA_PROVIDER_ALL_PROXY":   "socks5://provider-user:provider-secret@proxy.example.test",
		"SYNARA_PROVIDER_NO_PROXY":    "127.0.0.1,localhost",
	}
	if len(actual) != len(want) {
		t.Fatalf("Provider Host environment contains unexpected entries: %#v", actual)
	}
	for name, value := range want {
		if actual[name] != value {
			t.Fatalf("Provider Host environment %s = %q, want %q", name, actual[name], value)
		}
	}
}

func TestRunnerCapabilitySummaryRejectsProviderPolicyMismatch(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "policy-mismatch")

	_, err := providerHostV2TestRunner().CapabilitySummary(context.Background())
	if runnerFailureCode(err) != "provider_version_incompatible" {
		t.Fatalf("Provider policy mismatch returned %v", err)
	}
}

func TestRunnerProviderHostV2RejectsInvalidRuntimeState(t *testing.T) {
	for _, test := range []struct {
		mode string
		code string
	}{
		{mode: "runtime-unavailable", code: "provider_not_installed"},
		{mode: "runtime-incompatible", code: "provider_version_incompatible"},
		{mode: "runtime-version-unreported", code: "provider_version_incompatible"},
		{mode: "runtime-missing-version", code: "protocol_violation"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
			t.Setenv("PROVIDER_HOST_TEST_MODE", test.mode)
			_, err := providerHostV2TestRunner().Run(
				context.Background(), providerHostV2TestInput(t), nil,
				func(context.Context, RunnerMessage) error { return nil },
			)
			if runnerFailureCode(err) != test.code {
				t.Fatalf("runtime mode %s returned %v", test.mode, err)
			}
		})
	}
}

func TestRunnerCapabilitySummaryPreservesClaudeSDKRuntime(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "success")

	summary, err := providerHostV2TestRunner().CapabilitySummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	providers := summary["providers"].(map[string]any)
	claude := providers["claudeAgent"].(providerHostDescriptor)
	runtime := claude.CapabilityDescriptor.Runtime
	if runtime == nil || runtime.Kind != "sdk" ||
		runtime.Name != "@anthropic-ai/claude-agent-sdk" || runtime.VersionSource != "package" ||
		runtime.Version == nil || *runtime.Version != "0.3.207" || !runtime.Compatible {
		t.Fatalf("Claude SDK runtime metadata was not preserved: %#v", runtime)
	}
}

func TestRunnerExperimentalProviderEnablementNormalizesProviderID(t *testing.T) {
	runner := providerHostV2TestRunnerWithExperimental("claudeAgent")
	if !runner.experimentalProviderEnabled("claudeagent") || runner.experimentalProviderEnabled("codex") {
		t.Fatalf("experimental Provider enablement did not normalize Provider IDs: %#v", runner.experimentalProviders)
	}
}

func TestRunnerProviderHostV2RunsClaudeSDKWithoutLegacyProviderCLIVersion(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "missing-provider-cli-version")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	input := providerHostV2TestInput(t)
	input.Workload.Provider = "claudeAgent"
	result, err := providerHostV2TestRunner().Run(
		context.Background(), input, nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["text"] != "done" {
		t.Fatalf("unexpected Claude SDK result: %#v", result)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "Describe\nStartSession\nSendTurn\n" {
		t.Fatalf("unexpected Claude SDK command sequence %q", commands)
	}
}

func TestRunnerProviderHostV2RejectsCodexCLIWithoutProviderCLIVersion(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "missing-provider-cli-version")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("PROVIDER_HOST_TEST_COMMAND_LOG", commandLog)

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "provider_not_installed" {
		t.Fatalf("Codex CLI without providerCliVersion returned %v", err)
	}
	commands, readErr := os.ReadFile(commandLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(commands) != "Describe\n" {
		t.Fatalf("Codex CLI command ran before providerCliVersion rejection: %q", commands)
	}
}

func TestRunnerCapabilitySummaryRequiresNestedRuntimePolicy(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "root-runtime-policy")

	_, err := providerHostV2TestRunner().CapabilitySummary(context.Background())
	if runnerFailureCode(err) != "protocol_violation" {
		t.Fatalf("root-level runtime policy returned %v", err)
	}
}

func TestRunnerCapabilitySummaryRequiresNonExperimentalProvidersEnabled(t *testing.T) {
	for _, test := range []struct {
		mode    string
		wantErr bool
	}{
		{mode: "local-only-policy-enabled"},
		{mode: "local-only-policy-disabled", wantErr: true},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
			t.Setenv("PROVIDER_HOST_TEST_MODE", test.mode)

			_, err := providerHostV2TestRunner().CapabilitySummary(context.Background())
			if test.wantErr {
				if runnerFailureCode(err) != "provider_version_incompatible" {
					t.Fatalf("disabled non-experimental Provider returned %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("enabled non-experimental Provider returned %v", err)
			}
		})
	}
}

func TestRunnerProviderHostV2ExperimentalProtocol20FailsClosed(t *testing.T) {
	t.Setenv("GO_WANT_PROVIDER_HOST_HELPER", "1")
	t.Setenv("PROVIDER_HOST_TEST_MODE", "protocol-2.0")

	_, err := providerHostV2TestRunner().Run(
		context.Background(), providerHostV2TestInput(t), nil,
		func(context.Context, RunnerMessage) error { return nil },
	)
	if runnerFailureCode(err) != "provider_version_incompatible" {
		t.Fatalf("experimental Provider Host 2.0 did not fail closed: %v", err)
	}
}

func TestRunnerProviderHostV2BuiltHostIntegration(t *testing.T) {
	rawCommand := strings.TrimSpace(os.Getenv("SYNARA_TEST_PROVIDER_HOST_COMMAND_JSON"))
	if rawCommand == "" {
		t.Skip("set SYNARA_TEST_PROVIDER_HOST_COMMAND_JSON to a built Provider Host command")
	}
	var command []string
	if err := json.Unmarshal([]byte(rawCommand), &command); err != nil || len(command) == 0 {
		t.Fatalf("invalid SYNARA_TEST_PROVIDER_HOST_COMMAND_JSON: %v", err)
	}
	runner := &Runner{command: command, maxMessageBytes: 1 << 20, protocol: RunnerProtocolV2}
	summary, err := runner.CapabilitySummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	providers, ok := summary["providers"].(map[string]any)
	if !ok {
		t.Fatalf("built Provider Host omitted Provider descriptors: %#v", summary)
	}
	for _, provider := range providerHostProviders {
		descriptor, ok := providers[provider].(providerHostDescriptor)
		if !ok || descriptor.ProtocolVersion.Major != providerHostProtocolMajor ||
			descriptor.ProtocolVersion.Minor < providerHostProtocolMinor ||
			descriptor.RuntimeEventVersions.Minimum > providerHostRuntimeEventVersion ||
			descriptor.RuntimeEventVersions.Maximum < providerHostRuntimeEventVersion ||
			descriptor.CapabilityDescriptor.Runtime == nil ||
			descriptor.CapabilityDescriptor.ReleasePolicy == nil {
			t.Fatalf("built Provider Host descriptor for %s is incompatible: %#v", provider, providers[provider])
		}
		if provider == "codex" || provider == "claudeAgent" {
			if descriptor.CapabilityDescriptor.Capabilities["send-turn"] != "native" {
				t.Fatalf("remote Provider %s omitted send-turn: %#v", provider, descriptor)
			}
			if !descriptor.CapabilityDescriptor.ReleasePolicy.RequiresExplicitEnablement ||
				descriptor.CapabilityDescriptor.ReleasePolicy.Enabled {
				t.Fatalf("experimental Provider %s advertised an invalid release policy: %#v", provider, descriptor)
			}
		} else if descriptor.CapabilityDescriptor.SupportTier != "local-only" {
			t.Fatalf("Provider %s was not kept Local-only: %#v", provider, descriptor)
		} else if descriptor.CapabilityDescriptor.ReleasePolicy.RequiresExplicitEnablement ||
			!descriptor.CapabilityDescriptor.ReleasePolicy.Enabled {
			t.Fatalf("Local-only Provider %s advertised an invalid release policy: %#v", provider, descriptor)
		}
		if provider == "claudeAgent" {
			runtime := descriptor.CapabilityDescriptor.Runtime
			if runtime.Kind != "sdk" || runtime.Name != "@anthropic-ai/claude-agent-sdk" ||
				runtime.VersionSource != "package" {
				t.Fatalf("Claude Provider did not advertise its SDK runtime: %#v", runtime)
			}
		}
	}
}

func TestParseRunnerProtocolRequiresExplicitLegacyMode(t *testing.T) {
	for _, test := range []struct {
		value string
		want  RunnerProtocol
	}{
		{value: "", want: RunnerProtocolV2},
		{value: "v2", want: RunnerProtocolV2},
		{value: "v1", want: RunnerProtocolV1},
	} {
		actual, err := parseRunnerProtocol(test.value)
		if err != nil || actual != test.want {
			t.Fatalf("parseRunnerProtocol(%q) = %q, %v", test.value, actual, err)
		}
	}
	if _, err := parseRunnerProtocol("auto"); err == nil {
		t.Fatal("automatic v2 to v1 fallback must not be accepted")
	}
}

func providerHostV2TestRunner() *Runner {
	return providerHostV2TestRunnerWithExperimental("codex", "claudeAgent")
}

func providerHostV2TestRunnerWithExperimental(providers ...string) *Runner {
	experimentalProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		experimentalProviders[provider] = struct{}{}
	}
	return &Runner{
		command:               providerHostV2TestCommand(),
		maxMessageBytes:       1 << 20,
		protocol:              RunnerProtocolV2,
		experimentalProviders: experimentalProviders,
	}
}

const (
	providerHostTestHelperArgument     = "--synara-provider-host-test-helper"
	providerHostTestModeArgument       = "--synara-provider-host-test-mode"
	providerHostTestCommandLogArgument = "--synara-provider-host-test-command-log"
	providerHostTestExpectedEnvArg     = "--synara-provider-host-test-expected-env"
)

func providerHostV2TestCommand() []string {
	command := []string{
		os.Args[0], "-test.run=^TestProviderHostV2HelperProcess$", "--", providerHostTestHelperArgument,
	}
	if mode := os.Getenv("PROVIDER_HOST_TEST_MODE"); mode != "" {
		command = append(command, providerHostTestModeArgument, mode)
	}
	if commandLog := os.Getenv("PROVIDER_HOST_TEST_COMMAND_LOG"); commandLog != "" {
		command = append(command, providerHostTestCommandLogArgument, commandLog)
	}
	for _, name := range append(
		[]string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TERM"},
		providerHostProxyEnvironmentAllowlist...,
	) {
		if value, found := os.LookupEnv(name); found {
			command = append(command, providerHostTestExpectedEnvArg, name+"="+value)
		}
	}
	return command
}

func providerHostTestArgument(name string) string {
	for index := 0; index+1 < len(os.Args); index++ {
		if os.Args[index] == name {
			return os.Args[index+1]
		}
	}
	return ""
}

func providerHostTestExpectedEnvironment() []string {
	var result []string
	for index := 0; index+1 < len(os.Args); index++ {
		if os.Args[index] == providerHostTestExpectedEnvArg {
			result = append(result, os.Args[index+1])
		}
	}
	return result
}

func providerHostV2TestInput(t *testing.T) RunnerInput {
	t.Helper()
	executionID := uuid.New()
	turnID := uuid.New()
	return RunnerInput{
		Execution: executions.Execution{ID: executionID, TurnID: turnID, Generation: 3},
		Workload: executions.Workload{
			TenantID: uuid.New(), OrganizationID: uuid.New(), ProjectID: uuid.New(),
			SessionID: uuid.New(), TurnID: turnID, Provider: "codex", InputText: "run",
		},
		WorkspaceDirectory: t.TempDir(),
	}
}

func stringPointer(value string) *string { return &value }

func providerHostTestRuntimeEventVersions(mode string) map[string]any {
	if mode == "runtime-event-v1-only" {
		return map[string]any{"minimum": 1, "maximum": 1}
	}
	return map[string]any{"minimum": 2, "maximum": 2}
}

func providerHostTestRuntime(provider, mode string) map[string]any {
	runtime := map[string]any{
		"kind": "cli", "name": provider, "version": "1.2.3", "available": true,
		"versionSource": "probe",
		"compatibleRange": map[string]any{
			"minimumInclusive": "1.0.0", "maximumExclusive": "2.0.0",
		},
		"compatible": true,
	}
	if provider == "claudeAgent" {
		runtime["kind"] = "sdk"
		runtime["name"] = "@anthropic-ai/claude-agent-sdk"
		runtime["version"] = "0.3.207"
		runtime["versionSource"] = "package"
		runtime["compatibleRange"] = map[string]any{
			"minimumInclusive": "0.3.0", "maximumExclusive": "0.4.0",
		}
	}
	switch mode {
	case "runtime-unavailable":
		runtime["available"] = false
		runtime["compatible"] = false
		delete(runtime, "version")
	case "runtime-incompatible":
		runtime["compatible"] = false
	case "runtime-version-unreported":
		runtime["compatible"] = false
		delete(runtime, "version")
	case "runtime-missing-version":
		delete(runtime, "version")
	}
	return runtime
}

func providerHostTestExperimentalProviderEnabled(provider string) bool {
	for _, enabled := range strings.Split(os.Getenv(providerHostExperimentalEnv), ",") {
		if strings.TrimSpace(enabled) == provider {
			return true
		}
	}
	return false
}

func TestProviderHostV2HelperProcess(t *testing.T) {
	if !containsString(os.Args, providerHostTestHelperArgument) {
		return
	}
	for _, entry := range providerHostTestExpectedEnvironment() {
		name, value, found := strings.Cut(entry, "=")
		if !found || os.Getenv(name) != value {
			fmt.Fprintf(os.Stderr, "Provider Host runtime environment %s = %q, want %q\n", name, os.Getenv(name), value)
			os.Exit(2)
		}
	}
	for _, name := range []string{
		"GO_WANT_PROVIDER_HOST_HELPER", "PROVIDER_HOST_TEST_MODE", "PROVIDER_HOST_TEST_COMMAND_LOG",
		"SYNARA_PROVIDER_HOST_BUILD_VERSION",
		"SECRET", "HOST_SECRET", "SYNARA_HOST_SECRET", "SYNARA_AUTH_TOKEN", "SYNARA_WORKER_REGISTRATION_TOKEN",
		"SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", "SYNARA_LEASE_TOKEN", "SYNARA_CONTROL_PLANE_URL",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"GITHUB_TOKEN", "GH_TOKEN", "DATABASE_URL", "PGPASSWORD", "POSTGRES_PASSWORD",
		"S3_ACCESS_KEY_ID", "MINIO_ROOT_PASSWORD", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_SECRET",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSH_AUTH_SOCK", "NODE_OPTIONS",
	} {
		if os.Getenv(name) != "" {
			fmt.Fprintln(os.Stderr, "ambient secret environment leaked to Provider Host:", name)
			os.Exit(2)
		}
	}
	if fd := strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_CREDENTIAL_FD")); fd != "" {
		file := os.NewFile(3, "provider-credential")
		var credential RunnerCredential
		if file == nil || json.NewDecoder(file).Decode(&credential) != nil || credential.Payload["apiKey"] != "provider-secret" {
			fmt.Fprintln(os.Stderr, "Provider credential was not delivered through FD 3")
			os.Exit(2)
		}
		_ = file.Close()
	}

	mode := providerHostTestArgument(providerHostTestModeArgument)
	commandLog := providerHostTestArgument(providerHostTestCommandLogArgument)
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var pendingSend *providerHostCommand
	for scanner.Scan() {
		var command providerHostCommand
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if commandLog != "" {
			file, err := os.OpenFile(commandLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			_, _ = fmt.Fprintln(file, command.CommandType)
			_ = file.Close()
		}
		if mode == "malformed" && command.CommandType == "Describe" {
			fmt.Fprintln(os.Stdout, "{not-json")
			continue
		}
		if mode == "crash" && command.CommandType == "Describe" {
			fmt.Fprintln(os.Stderr, "simulated Host crash")
			os.Exit(23)
		}
		if mode == "oversized" && command.CommandType == "Describe" {
			fmt.Fprintf(os.Stdout, "{\"padding\":\"%s\"}\n", strings.Repeat("x", 2<<10))
			continue
		}
		switch command.CommandType {
		case "Describe":
			provider, _ := command.Payload["provider"].(string)
			capabilities := map[string]any{"send-turn": "native", "steer-turn": "native"}
			if mode == "missing-capability" {
				capabilities = map[string]any{"send-turn": "unsupported"}
			}
			major := providerHostProtocolMajor
			if mode == "incompatible-major" {
				major = 3
			}
			minor := providerHostProtocolMinor
			if mode == "protocol-2.0" {
				minor = 0
			}
			supportTier := "experimental"
			requiresExplicitEnablement := true
			enabled := providerHostTestExperimentalProviderEnabled(provider)
			if mode == "local-only-policy-enabled" || mode == "local-only-policy-disabled" {
				supportTier = "local-only"
				requiresExplicitEnablement = false
				enabled = mode == "local-only-policy-enabled"
			}
			if mode == "policy-mismatch" {
				enabled = !enabled
			}
			capabilityDescriptor := map[string]any{
				"provider": provider, "supportTier": supportTier, "adapterVersion": "test-adapter",
				"providerCliVersion": "test-cli", "capabilities": capabilities,
			}
			if mode == "missing-provider-cli-version" {
				delete(capabilityDescriptor, "providerCliVersion")
			}
			descriptor := map[string]any{
				"protocolVersion":  map[string]any{"major": major, "minor": minor, "futureMinorField": true},
				"hostBuildVersion": "test-host", "maximumCommandBytes": providerHostCommandLimit,
				"maximumMessageBytes":     1 << 20,
				"runtimeEventVersions":    providerHostTestRuntimeEventVersions(mode),
				"credentialDeliveryModes": []string{"anonymous-fd"},
				"resumeStrategies":        []string{"native-cursor", "authoritative-history"},
				"capabilityDescriptor":    capabilityDescriptor,
			}
			if minor >= 1 {
				runtime := providerHostTestRuntime(provider, mode)
				releasePolicy := map[string]any{
					"requiresExplicitEnablement": requiresExplicitEnablement,
					"enabled":                    enabled,
				}
				if mode == "root-runtime-policy" {
					descriptor["runtime"] = runtime
					descriptor["releasePolicy"] = releasePolicy
				} else {
					capabilityDescriptor["runtime"] = runtime
					capabilityDescriptor["releasePolicy"] = releasePolicy
				}
			}
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{
				"descriptor": descriptor,
			}, nil)
		case "StartSession", "ResumeSession":
			if version, ok := integerField(command.Payload, "runtimeEventVersion"); !ok || version != 2 {
				fmt.Fprintln(os.Stderr, "Runtime Event negotiation was not carried into the session command")
				os.Exit(2)
			}
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{"started": true}, nil)
		case "SendTurn":
			if version, ok := integerField(command.Payload, "runtimeEventVersion"); !ok || version != 2 {
				fmt.Fprintln(os.Stderr, "Runtime Event negotiation was not carried into SendTurn")
				os.Exit(2)
			}
			if mode == "provider-error" {
				retryable, yes, no := true, true, false
				emitProviderHostTestMessage(encoder, command, "Error", nil, &providerHostWireError{
					Code: "provider_rate_limited", Message: "Provider is rate limited", Retryable: &retryable,
					RequiresNewExecution: &yes, RequiresUserAction: &no,
					CanReconstructFromHistory: &yes, CanMoveWorker: &yes,
				})
				continue
			}
			if mode == "interaction" {
				copy := command
				pendingSend = &copy
				emitProviderHostTestMessage(encoder, command, "InteractionRequest", map[string]any{
					"interactionType": "approval", "requestId": "approval-1", "requestKind": "command", "summary": "Run command",
				}, nil)
				continue
			}
			if mode == "interrupt" || mode == "steer" {
				copy := command
				pendingSend = &copy
				emitProviderHostTestMessage(encoder, command, "Progress", map[string]any{"ready": true}, nil)
				continue
			}
			eventPayload := map[string]any{
				"eventVersion": 2, "eventType": "content.delta",
				"payload": map[string]any{"streamKind": "assistant_text", "delta": "hello"},
			}
			switch mode {
			case "event-version-missing":
				delete(eventPayload, "eventVersion")
			case "event-version-v1":
				eventPayload["eventVersion"] = 1
			case "event-type-unknown":
				eventPayload["eventType"] = "runtime.output.delta"
			case "event-payload-not-object":
				eventPayload["payload"] = "hello"
			case "event-payload-invalid-shape":
				eventPayload["payload"] = map[string]any{"delta": "hello"}
			case "event-payload-too-large":
				eventPayload["payload"] = map[string]any{
					"streamKind": "assistant_text", "delta": strings.Repeat("x", executions.RuntimeEventMaxPayloadBytes),
				}
			}
			emitProviderHostTestMessage(encoder, command, "Event", eventPayload, nil)
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{
				"output": map[string]any{"text": "done"}, "providerResumeCursor": "cursor-next",
			}, nil)
			if mode == "output-after-terminal" {
				emitProviderHostTestMessage(encoder, command, "Progress", map[string]any{"late": true}, nil)
			}
		case "ResolveApproval":
			if mode != "interaction" || pendingSend == nil || command.Payload["requestId"] != "approval-1" {
				fmt.Fprintln(os.Stderr, "unexpected approval resolution")
				os.Exit(2)
			}
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{"acknowledged": true}, nil)
			emitProviderHostTestMessage(encoder, *pendingSend, "Result", map[string]any{
				"output": map[string]any{"text": "approved"}, "providerResumeCursor": "cursor-approved",
			}, nil)
			pendingSend = nil
		case "SteerTurn":
			if mode != "steer" || pendingSend == nil || command.Payload["targetCommandId"] != pendingSend.CommandID ||
				command.Payload["inputText"] != "focus on tests" {
				fmt.Fprintln(os.Stderr, "unexpected turn steer")
				os.Exit(2)
			}
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{"steered": true}, nil)
			emitProviderHostTestMessage(encoder, *pendingSend, "Result", map[string]any{
				"output": map[string]any{"text": "steered"}, "providerResumeCursor": "cursor-steered",
			}, nil)
			pendingSend = nil
		case "InterruptTurn":
			if mode != "interrupt" || pendingSend == nil || command.Payload["targetCommandId"] != pendingSend.CommandID {
				fmt.Fprintln(os.Stderr, "unexpected turn interrupt")
				os.Exit(2)
			}
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{
				"interrupted": true, "providerResumeCursor": "cursor-interrupted",
			}, nil)
			no := false
			yes := true
			emitProviderHostTestMessage(encoder, *pendingSend, "Error", nil, &providerHostWireError{
				Code: "interrupted", Message: "Provider turn was interrupted", Retryable: &no,
				RequiresNewExecution: &no, RequiresUserAction: &no,
				CanReconstructFromHistory: &yes, CanMoveWorker: &yes,
			})
			pendingSend = nil
		default:
			fmt.Fprintln(os.Stderr, "unexpected command", command.CommandType)
			os.Exit(2)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func emitProviderHostTestMessage(
	encoder *json.Encoder,
	command providerHostCommand,
	messageType string,
	payload map[string]any,
	wireError *providerHostWireError,
) {
	message := map[string]any{
		"requestId": command.RequestID, "protocolVersion": map[string]any{"major": 2, "minor": 9},
		"executionId": command.ExecutionID, "generation": command.Generation, "commandId": command.CommandID,
		"occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "messageType": messageType,
		"futureOptionalField": map[string]any{"ignored": true},
	}
	if payload != nil {
		message["payload"] = payload
	}
	if wireError != nil {
		message["error"] = wireError
	}
	if err := encoder.Encode(message); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

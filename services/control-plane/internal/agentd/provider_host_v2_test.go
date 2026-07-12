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
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "worker-secret")
	t.Setenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", uuid.NewString())
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
	if len(messages) != 1 || messages[0].Type != "event" || messages[0].EventType != "runtime.output.delta" ||
		messages[0].Payload["text"] != "hello" {
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
			if message.Type != "interaction" {
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
		if !ok || descriptor.ProtocolVersion.Major != providerHostProtocolMajor {
			t.Fatalf("built Provider Host descriptor for %s is incompatible: %#v", provider, providers[provider])
		}
		if provider == "codex" || provider == "claudeAgent" {
			if descriptor.CapabilityDescriptor.Capabilities["send-turn"] != "native" {
				t.Fatalf("remote Provider %s omitted send-turn: %#v", provider, descriptor)
			}
		} else if descriptor.CapabilityDescriptor.SupportTier != "local-only" {
			t.Fatalf("Provider %s was not kept Local-only: %#v", provider, descriptor)
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
	return &Runner{
		command:         []string{os.Args[0], "-test.run=TestProviderHostV2HelperProcess", "--"},
		maxMessageBytes: 1 << 20,
		protocol:        RunnerProtocolV2,
	}
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

func TestProviderHostV2HelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PROVIDER_HOST_HELPER") != "1" {
		return
	}
	if os.Getenv("SYNARA_WORKER_REGISTRATION_TOKEN") != "" || os.Getenv("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID") != "" {
		fmt.Fprintln(os.Stderr, "worker environment leaked to Provider Host")
		os.Exit(2)
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

	mode := os.Getenv("PROVIDER_HOST_TEST_MODE")
	commandLog := os.Getenv("PROVIDER_HOST_TEST_COMMAND_LOG")
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
			capabilities := map[string]any{"send-turn": "native"}
			if mode == "missing-capability" {
				capabilities = map[string]any{"send-turn": "unsupported"}
			}
			major := providerHostProtocolMajor
			if mode == "incompatible-major" {
				major = 3
			}
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{
				"descriptor": map[string]any{
					"protocolVersion":  map[string]any{"major": major, "minor": 7, "futureMinorField": true},
					"hostBuildVersion": "test-host", "maximumCommandBytes": providerHostCommandLimit,
					"maximumMessageBytes":     1 << 20,
					"runtimeEventVersions":    map[string]any{"minimum": 1, "maximum": 1},
					"credentialDeliveryModes": []string{"anonymous-fd"},
					"resumeStrategies":        []string{"native-cursor", "authoritative-history"},
					"capabilityDescriptor": map[string]any{
						"provider": provider, "supportTier": "experimental", "adapterVersion": "test-adapter",
						"providerCliVersion": "test-cli", "capabilities": capabilities,
					},
				},
			}, nil)
		case "StartSession", "ResumeSession":
			emitProviderHostTestMessage(encoder, command, "Result", map[string]any{"started": true}, nil)
		case "SendTurn":
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
					"interactionType": "approval", "requestId": "approval-1", "summary": "Run command",
				}, nil)
				continue
			}
			if mode == "interrupt" {
				copy := command
				pendingSend = &copy
				emitProviderHostTestMessage(encoder, command, "Progress", map[string]any{"ready": true}, nil)
				continue
			}
			emitProviderHostTestMessage(encoder, command, "Event", map[string]any{
				"eventType": "runtime.output.delta", "payload": map[string]any{"text": "hello"},
			}, nil)
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

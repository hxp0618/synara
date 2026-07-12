package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type RunnerProtocol string

const (
	RunnerProtocolV1 RunnerProtocol = "v1"
	RunnerProtocolV2 RunnerProtocol = "v2"

	providerHostProtocolMajor = 2
	providerHostProtocolMinor = 0
	providerHostCommandLimit  = 2 << 20
)

var providerHostProviders = []string{
	"codex", "claudeAgent", "cursor", "gemini", "grok", "kilo", "opencode", "pi",
}

type providerHostProtocolVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

type providerHostCapabilityDescriptor struct {
	Provider           string            `json:"provider"`
	SupportTier        string            `json:"supportTier"`
	AdapterVersion     string            `json:"adapterVersion"`
	ProviderCLIVersion *string           `json:"providerCliVersion,omitempty"`
	Capabilities       map[string]string `json:"capabilities"`
}

type providerHostVersionRange struct {
	Minimum int `json:"minimum"`
	Maximum int `json:"maximum"`
}

type providerHostDescriptor struct {
	ProtocolVersion        providerHostProtocolVersion      `json:"protocolVersion"`
	HostBuildVersion       string                           `json:"hostBuildVersion"`
	CapabilityDescriptor   providerHostCapabilityDescriptor `json:"capabilityDescriptor"`
	MaximumCommandBytes    int                              `json:"maximumCommandBytes"`
	MaximumMessageBytes    int                              `json:"maximumMessageBytes"`
	RuntimeEventVersions   providerHostVersionRange         `json:"runtimeEventVersions"`
	CredentialDeliveryMode []string                         `json:"credentialDeliveryModes"`
	ResumeStrategies       []string                         `json:"resumeStrategies"`
}

type providerHostCommand struct {
	RequestID       string                      `json:"requestId"`
	ProtocolVersion providerHostProtocolVersion `json:"protocolVersion"`
	ExecutionID     string                      `json:"executionId"`
	Generation      int64                       `json:"generation"`
	CommandType     string                      `json:"commandType"`
	CommandID       string                      `json:"commandId"`
	OccurredAt      string                      `json:"occurredAt"`
	Payload         map[string]any              `json:"payload"`
}

type providerHostWireError struct {
	Code                      string `json:"code"`
	Message                   string `json:"message"`
	Retryable                 *bool  `json:"retryable"`
	RequiresNewExecution      *bool  `json:"requiresNewExecution"`
	RequiresUserAction        *bool  `json:"requiresUserAction"`
	CanReconstructFromHistory *bool  `json:"canReconstructFromHistory"`
	CanMoveWorker             *bool  `json:"canMoveWorker"`
}

type providerHostMessage struct {
	RequestID       string                      `json:"requestId"`
	ProtocolVersion providerHostProtocolVersion `json:"protocolVersion"`
	ExecutionID     string                      `json:"executionId"`
	Generation      int64                       `json:"generation"`
	CommandID       string                      `json:"commandId"`
	OccurredAt      string                      `json:"occurredAt"`
	MessageType     string                      `json:"messageType"`
	Payload         map[string]any              `json:"payload,omitempty"`
	Error           *providerHostWireError      `json:"error,omitempty"`
}

type runnerFailure struct {
	code                      string
	message                   string
	retryable                 bool
	requiresNewExecution      bool
	requiresUserAction        bool
	canReconstructFromHistory bool
	canMoveWorker             bool
}

func (e *runnerFailure) Error() string { return e.message }

func runnerFailureCode(err error) string {
	var failure *runnerFailure
	if errors.As(err, &failure) && strings.TrimSpace(failure.code) != "" {
		return failure.code
	}
	return "runner_failed"
}

func (r *Runner) CapabilitySummary(ctx context.Context) (map[string]any, error) {
	if r.protocol != RunnerProtocolV2 {
		return map[string]any{
			"protocolVersion": map[string]any{"major": 1, "minor": 0},
			"legacy":          true,
		}, nil
	}
	providers := make(map[string]any, len(providerHostProviders))
	for _, provider := range providerHostProviders {
		descriptor, err := r.describeProviderHostV2(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("describe Provider Host for %s: %w", provider, err)
		}
		providers[provider] = descriptor
	}
	return map[string]any{
		"protocolVersion": map[string]any{"major": providerHostProtocolMajor, "minor": providerHostProtocolMinor},
		"legacy":          false,
		"providers":       providers,
	}, nil
}

func (r *Runner) describeProviderHostV2(ctx context.Context, provider string) (providerHostDescriptor, error) {
	process, err := r.startProviderHostV2(ctx, nil)
	if err != nil {
		return providerHostDescriptor{}, err
	}
	finished := false
	defer func() {
		if !finished {
			process.abort()
		}
	}()
	command := newProviderHostCommand(
		"worker-probe", 1, "Describe", "worker-probe:"+provider,
		map[string]any{"provider": provider},
	)
	terminal, err := process.executeContext(ctx, command, nil)
	if err != nil {
		return providerHostDescriptor{}, err
	}
	descriptor, err := descriptorFromResult(terminal)
	if err != nil {
		return providerHostDescriptor{}, err
	}
	if err := validateProviderHostDescriptorWire(descriptor, provider); err != nil {
		return providerHostDescriptor{}, err
	}
	process.maximumCommandBytes = min(process.maximumCommandBytes, descriptor.MaximumCommandBytes)
	process.maximumMessageBytes = min(process.maximumMessageBytes, descriptor.MaximumMessageBytes)
	if err := process.finish(); err != nil {
		return providerHostDescriptor{}, err
	}
	finished = true
	return descriptor, nil
}

func (r *Runner) runProviderHostV2(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	controls <-chan RunnerControl,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	process, err := r.startProviderHostV2(ctx, credential)
	if err != nil {
		return RunnerResult{}, err
	}
	finished := false
	defer func() {
		if !finished {
			process.abort()
		}
	}()

	executionID := input.Execution.ID.String()
	generation := input.Execution.Generation
	provider := strings.TrimSpace(input.Workload.Provider)
	describe := newProviderHostCommand(
		executionID, generation, "Describe", commandID(input, "describe"),
		map[string]any{"provider": provider},
	)
	describeResult, err := process.executeContext(ctx, describe, nil)
	if err != nil {
		return RunnerResult{}, err
	}
	descriptor, err := descriptorFromResult(describeResult)
	if err != nil {
		return RunnerResult{}, err
	}
	if err := validateProviderHostDescriptorForExecution(descriptor, input, credential); err != nil {
		return RunnerResult{}, err
	}
	process.maximumCommandBytes = min(process.maximumCommandBytes, descriptor.MaximumCommandBytes)
	process.maximumMessageBytes = min(process.maximumMessageBytes, descriptor.MaximumMessageBytes)

	sessionCommand := "StartSession"
	if input.ProviderResumeCursor != nil || len(input.Workload.ConversationHistory) > 0 {
		sessionCommand = "ResumeSession"
	}
	start := newProviderHostCommand(
		executionID, generation, sessionCommand, commandID(input, "session"),
		map[string]any{"runnerInput": input},
	)
	if _, err := process.executeContext(ctx, start, nil); err != nil {
		return RunnerResult{}, err
	}

	send := newProviderHostCommand(
		executionID, generation, "SendTurn", commandID(input, "send"),
		map[string]any{"inputText": input.Workload.InputText, "turnId": input.Workload.TurnID.String()},
	)
	sendExecution, err := process.startCommand(send, func(message RunnerMessage) error {
		if handle == nil {
			return protocolFailure("Provider Host emitted a non-terminal message without a Worker handler")
		}
		return handle(ctx, message)
	})
	if err != nil {
		return RunnerResult{}, err
	}
	for {
		select {
		case outcome := <-sendExecution.result:
			if outcome.err != nil {
				return RunnerResult{}, outcome.err
			}
			result, err := runnerResultFromTerminal(outcome.message)
			if err != nil {
				return RunnerResult{}, err
			}
			if err := process.finish(); err != nil {
				return RunnerResult{}, err
			}
			finished = true
			return result, nil
		case control, open := <-controls:
			if !open {
				controls = nil
				continue
			}
			controlErr := process.executeInteractionResolution(ctx, input, control)
			if control.Done != nil {
				control.Done <- controlErr
			}
			if controlErr != nil {
				return RunnerResult{}, controlErr
			}
		case <-ctx.Done():
			if err := process.interruptActiveTurn(input, send.CommandID); err == nil {
				timer := time.NewTimer(2 * time.Second)
				select {
				case <-sendExecution.result:
					if finishErr := process.finish(); finishErr == nil {
						finished = true
					}
				case <-timer.C:
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			return RunnerResult{}, ctx.Err()
		}
	}
}

func (p *providerHostV2Process) interruptActiveTurn(input RunnerInput, targetCommandID string) error {
	command := newProviderHostCommand(
		input.Execution.ID.String(), input.Execution.Generation, "InterruptTurn", commandID(input, "interrupt"),
		map[string]any{"targetCommandId": targetCommandID},
	)
	execution, err := p.startCommand(command, nil)
	if err != nil {
		return err
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case outcome := <-execution.result:
		return outcome.err
	case <-timer.C:
		return &runnerFailure{
			code: "interrupted", message: "Provider Host did not acknowledge InterruptTurn before the deadline",
			requiresNewExecution: true, canReconstructFromHistory: true, canMoveWorker: true,
		}
	}
}

func (p *providerHostV2Process) executeInteractionResolution(
	ctx context.Context,
	input RunnerInput,
	control RunnerControl,
) error {
	if control.Err != nil {
		return control.Err
	}
	delivery := control.Delivery
	if normalizeProvider(delivery.Provider) != normalizeProvider(input.Workload.Provider) {
		return protocolFailure("Interaction resolution Provider does not match the active Provider Session")
	}
	if delivery.CommandType != "ResolveApproval" && delivery.CommandType != "ResolveUserInput" {
		return protocolFailure("Interaction resolution uses an unsupported Provider Host command")
	}
	if strings.TrimSpace(delivery.CommandID) == "" || strings.TrimSpace(delivery.RequestID) == "" ||
		delivery.Resolution == nil {
		return protocolFailure("Interaction resolution omitted required command fields")
	}
	command := newProviderHostCommand(
		input.Execution.ID.String(), input.Execution.Generation, delivery.CommandType, delivery.CommandID,
		map[string]any{
			"interactionId": delivery.InteractionID.String(), "requestId": delivery.RequestID,
			"resolutionKind": delivery.ResolutionKind, "resolution": delivery.Resolution,
		},
	)
	execution, err := p.startCommand(command, nil)
	if err != nil {
		return err
	}
	if control.MarkDelivered == nil || control.Acknowledge == nil {
		return protocolFailure("Interaction resolution omitted Worker delivery callbacks")
	}
	if err := control.MarkDelivered(ctx); err != nil {
		return fmt.Errorf("mark interaction resolution delivered: %w", err)
	}
	_, terminalErr := execution.wait()
	if err := control.Acknowledge(ctx); err != nil {
		return fmt.Errorf("acknowledge interaction resolution: %w", err)
	}
	return terminalErr
}

func newProviderHostCommand(
	executionID string,
	generation int64,
	commandType string,
	commandID string,
	payload map[string]any,
) providerHostCommand {
	return providerHostCommand{
		RequestID: uuid.NewString(),
		ProtocolVersion: providerHostProtocolVersion{
			Major: providerHostProtocolMajor,
			Minor: providerHostProtocolMinor,
		},
		ExecutionID: executionID,
		Generation:  generation,
		CommandType: commandType,
		CommandID:   commandID,
		OccurredAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Payload:     payload,
	}
}

func commandID(input RunnerInput, operation string) string {
	return fmt.Sprintf("%s:%d:%s:%s", input.Execution.ID, input.Execution.Generation, input.Workload.TurnID, operation)
}

func descriptorFromResult(message providerHostMessage) (providerHostDescriptor, error) {
	if message.MessageType != "Result" {
		return providerHostDescriptor{}, protocolFailure("Describe did not return a Result message")
	}
	value, ok := message.Payload["descriptor"]
	if !ok {
		return providerHostDescriptor{}, protocolFailure("Describe Result omitted descriptor")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return providerHostDescriptor{}, protocolFailure("Describe descriptor is not encodable")
	}
	var descriptor providerHostDescriptor
	if err := json.Unmarshal(encoded, &descriptor); err != nil {
		return providerHostDescriptor{}, protocolFailure("Describe descriptor is invalid")
	}
	return descriptor, nil
}

func validateProviderHostDescriptorWire(descriptor providerHostDescriptor, requestedProvider string) error {
	if descriptor.ProtocolVersion.Major != providerHostProtocolMajor {
		return &runnerFailure{
			code:                 "provider_version_incompatible",
			message:              fmt.Sprintf("Provider Host Protocol major %d is not supported", descriptor.ProtocolVersion.Major),
			requiresNewExecution: true, requiresUserAction: true,
			canReconstructFromHistory: true, canMoveWorker: true,
		}
	}
	if descriptor.ProtocolVersion.Minor < 0 || strings.TrimSpace(descriptor.HostBuildVersion) == "" ||
		strings.TrimSpace(descriptor.CapabilityDescriptor.Provider) == "" ||
		strings.TrimSpace(descriptor.CapabilityDescriptor.SupportTier) == "" ||
		strings.TrimSpace(descriptor.CapabilityDescriptor.AdapterVersion) == "" ||
		descriptor.CapabilityDescriptor.Capabilities == nil || descriptor.MaximumCommandBytes <= 0 ||
		descriptor.MaximumMessageBytes <= 0 || descriptor.RuntimeEventVersions.Minimum <= 0 ||
		descriptor.RuntimeEventVersions.Maximum < descriptor.RuntimeEventVersions.Minimum {
		return protocolFailure("Provider Host descriptor omitted required compatibility fields")
	}
	if normalizeProvider(descriptor.CapabilityDescriptor.Provider) != normalizeProvider(requestedProvider) {
		return protocolFailure("Provider Host descriptor does not match the requested Provider")
	}
	if descriptor.RuntimeEventVersions.Minimum > 1 || descriptor.RuntimeEventVersions.Maximum < 1 {
		return &runnerFailure{
			code:                 "provider_version_incompatible",
			message:              "Provider Host does not support Runtime Event version 1",
			requiresNewExecution: true, requiresUserAction: true,
			canReconstructFromHistory: true, canMoveWorker: true,
		}
	}
	return nil
}

func validateProviderHostDescriptorForExecution(
	descriptor providerHostDescriptor,
	input RunnerInput,
	credential *RunnerCredential,
) error {
	if err := validateProviderHostDescriptorWire(descriptor, input.Workload.Provider); err != nil {
		return err
	}
	capability := descriptor.CapabilityDescriptor.Capabilities["send-turn"]
	if descriptor.CapabilityDescriptor.SupportTier == "local-only" ||
		(capability != "native" && capability != "emulated") {
		return &runnerFailure{
			code:               "capability_unsupported",
			message:            fmt.Sprintf("Provider %s is not supported by this remote Provider Host", input.Workload.Provider),
			requiresUserAction: true,
		}
	}
	if descriptor.CapabilityDescriptor.ProviderCLIVersion == nil ||
		strings.TrimSpace(*descriptor.CapabilityDescriptor.ProviderCLIVersion) == "" ||
		strings.EqualFold(strings.TrimSpace(*descriptor.CapabilityDescriptor.ProviderCLIVersion), "unavailable") {
		return &runnerFailure{
			code:               "provider_not_installed",
			message:            fmt.Sprintf("Provider CLI for %s is unavailable on this Worker", input.Workload.Provider),
			requiresUserAction: true, canMoveWorker: true,
		}
	}
	if credential != nil && !containsString(descriptor.CredentialDeliveryMode, "anonymous-fd") {
		return &runnerFailure{
			code:                 "credential_missing",
			message:              "Provider Host does not support the required anonymous credential descriptor",
			requiresNewExecution: true, requiresUserAction: true, canMoveWorker: true,
		}
	}
	if input.ProviderResumeCursor != nil && !containsString(descriptor.ResumeStrategies, "native-cursor") &&
		len(input.Workload.ConversationHistory) == 0 {
		return &runnerFailure{
			code:                 "session_resume_invalid",
			message:              "Provider Host cannot resume from the persisted Provider Cursor",
			requiresNewExecution: true, canReconstructFromHistory: false, canMoveWorker: true,
		}
	}
	if len(input.Workload.ConversationHistory) > 0 && !containsString(descriptor.ResumeStrategies, "authoritative-history") &&
		(input.ProviderResumeCursor == nil || !containsString(descriptor.ResumeStrategies, "native-cursor")) {
		return &runnerFailure{
			code:                 "session_resume_invalid",
			message:              "Provider Host cannot reconstruct the Session from authoritative history",
			requiresNewExecution: true, canMoveWorker: true,
		}
	}
	return nil
}

func runnerResultFromTerminal(message providerHostMessage) (RunnerResult, error) {
	if message.MessageType != "Result" {
		return RunnerResult{}, protocolFailure("SendTurn did not return a Result message")
	}
	output := map[string]any{}
	if value, found := message.Payload["output"]; found {
		decoded, ok := value.(map[string]any)
		if !ok {
			return RunnerResult{}, protocolFailure("SendTurn Result output is not an object")
		}
		output = decoded
	}
	var cursor *string
	if value, found := message.Payload["providerResumeCursor"]; found {
		decoded, ok := value.(string)
		if !ok || strings.TrimSpace(decoded) == "" {
			return RunnerResult{}, protocolFailure("SendTurn Result providerResumeCursor is invalid")
		}
		cursor = &decoded
	}
	return RunnerResult{Output: output, ProviderResumeCursor: cursor}, nil
}

type providerHostV2Process struct {
	command             *exec.Cmd
	stdin               io.WriteCloser
	stderr              *boundedBuffer
	credentialWrite     <-chan error
	maximumCommandBytes int
	maximumMessageBytes int

	mu             sync.Mutex
	writeMu        sync.Mutex
	commands       map[string]*providerHostCommandState
	completed      map[string]providerHostCompletedCommand
	readerDone     chan struct{}
	closing        bool
	fatalErr       error
	waitOnce       sync.Once
	waitErr        error
	credentialOnce sync.Once
	credentialErr  error
}

type providerHostCommandOutcome struct {
	message providerHostMessage
	err     error
}

type providerHostCommandState struct {
	command providerHostCommand
	handle  func(RunnerMessage) error
	result  chan providerHostCommandOutcome
}

type providerHostCompletedCommand struct {
	command providerHostCommand
	outcome providerHostCommandOutcome
}

type providerHostCommandExecution struct {
	result <-chan providerHostCommandOutcome
}

func (e *providerHostCommandExecution) wait() (providerHostMessage, error) {
	outcome := <-e.result
	return outcome.message, outcome.err
}

func (e *providerHostCommandExecution) waitContext(ctx context.Context) (providerHostMessage, error) {
	select {
	case outcome := <-e.result:
		return outcome.message, outcome.err
	case <-ctx.Done():
		return providerHostMessage{}, ctx.Err()
	}
}

func (r *Runner) startProviderHostV2(
	ctx context.Context,
	credential *RunnerCredential,
) (*providerHostV2Process, error) {
	if len(r.command) == 0 {
		return nil, &runnerFailure{code: "provider_unavailable", message: "Provider Host command is empty", canMoveWorker: true}
	}
	arguments := append([]string(nil), r.command[1:]...)
	if !containsString(arguments, "--protocol-v2") {
		arguments = append(arguments, "--protocol-v2")
	}
	command := exec.Command(r.command[0], arguments...)
	command.Env = runnerEnvironment(os.Environ())
	var credentialWrite <-chan error
	if credential != nil {
		readPipe, writePipe, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("open Provider Host credential pipe: %w", err)
		}
		command.ExtraFiles = []*os.File{readPipe}
		command.Env = append(command.Env, "SYNARA_PROVIDER_CREDENTIAL_FD=3")
		writeResult := make(chan error, 1)
		credentialWrite = writeResult
		go func() {
			defer close(writeResult)
			encoder := json.NewEncoder(writePipe)
			err := encoder.Encode(credential)
			if closeErr := writePipe.Close(); err == nil {
				err = closeErr
			}
			writeResult <- err
		}()
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		closeProviderHostFiles(command)
		return nil, fmt.Errorf("open Provider Host stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		closeProviderHostFiles(command)
		return nil, fmt.Errorf("open Provider Host stdout: %w", err)
	}
	stderr := &boundedBuffer{maximum: 64 << 10}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		closeProviderHostFiles(command)
		return nil, &runnerFailure{
			code: "provider_unavailable", message: safeRunnerMessage("start Provider Host: " + err.Error()),
			retryable: true, canReconstructFromHistory: true, canMoveWorker: true,
		}
	}
	closeProviderHostFiles(command)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), r.maxMessageBytes)
	process := &providerHostV2Process{
		command: command, stdin: stdin, stderr: stderr,
		credentialWrite: credentialWrite, maximumCommandBytes: providerHostCommandLimit,
		maximumMessageBytes: r.maxMessageBytes,
		commands:            make(map[string]*providerHostCommandState), completed: make(map[string]providerHostCompletedCommand),
		readerDone: make(chan struct{}),
	}
	go process.readLoop(scanner)
	return process, nil
}

func closeProviderHostFiles(command *exec.Cmd) {
	for _, file := range command.ExtraFiles {
		_ = file.Close()
	}
}

func (p *providerHostV2Process) execute(
	command providerHostCommand,
	handle func(RunnerMessage) error,
) (providerHostMessage, error) {
	execution, err := p.startCommand(command, handle)
	if err != nil {
		return providerHostMessage{}, err
	}
	return execution.wait()
}

func (p *providerHostV2Process) executeContext(
	ctx context.Context,
	command providerHostCommand,
	handle func(RunnerMessage) error,
) (providerHostMessage, error) {
	execution, err := p.startCommand(command, handle)
	if err != nil {
		return providerHostMessage{}, err
	}
	return execution.waitContext(ctx)
}

func (p *providerHostV2Process) startCommand(
	command providerHostCommand,
	handle func(RunnerMessage) error,
) (*providerHostCommandExecution, error) {
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("encode Provider Host command: %w", err)
	}
	if len(encoded) > p.maximumCommandBytes {
		return nil, protocolFailure("Provider Host command exceeds the negotiated size limit")
	}
	state := &providerHostCommandState{
		command: command, handle: handle, result: make(chan providerHostCommandOutcome, 1),
	}
	p.mu.Lock()
	if p.fatalErr != nil {
		err := p.fatalErr
		p.mu.Unlock()
		return nil, err
	}
	if p.closing {
		p.mu.Unlock()
		return nil, protocolFailure("Provider Host is shutting down")
	}
	if completed, found := p.completed[command.CommandID]; found {
		p.mu.Unlock()
		if !sameProviderHostCommand(completed.command, command) {
			return nil, protocolFailure("Provider Host commandId was reused for different command content")
		}
		state.result <- completed.outcome
		close(state.result)
		return &providerHostCommandExecution{result: state.result}, nil
	}
	if _, found := p.commands[command.CommandID]; found {
		p.mu.Unlock()
		return nil, protocolFailure("Provider Host commandId is already active")
	}
	p.commands[command.CommandID] = state
	p.mu.Unlock()

	p.writeMu.Lock()
	if _, err := p.stdin.Write(append(encoded, '\n')); err != nil {
		p.writeMu.Unlock()
		failure := &runnerFailure{
			code: "provider_unavailable", message: safeRunnerMessage("write Provider Host command: " + err.Error()),
			retryable: true, requiresNewExecution: true, canReconstructFromHistory: true, canMoveWorker: true,
		}
		p.fail(failure)
		return nil, failure
	}
	p.writeMu.Unlock()
	return &providerHostCommandExecution{result: state.result}, nil
}

func (p *providerHostV2Process) readLoop(scanner *bufio.Scanner) {
	defer close(p.readerDone)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := p.handleLine(append([]byte(nil), line...)); err != nil {
			p.fail(err)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		p.fail(protocolFailure("Provider Host emitted a malformed or oversized JSONL message"))
	}
	waitErr := p.waitProcess()
	p.mu.Lock()
	fatalErr := p.fatalErr
	closing := p.closing
	active := len(p.commands)
	p.mu.Unlock()
	if fatalErr != nil {
		return
	}
	if waitErr != nil {
		p.fail(p.processFailure(waitErr))
		return
	}
	if active > 0 {
		p.fail(protocolFailure("Provider Host exited before emitting a terminal message"))
		return
	}
	if !closing {
		p.fail(protocolFailure("Provider Host exited before Worker shutdown"))
	}
}

func (p *providerHostV2Process) handleLine(line []byte) error {
	message, err := p.decodeMessage(line)
	if err != nil {
		return err
	}
	p.mu.Lock()
	state := p.commands[message.CommandID]
	if state == nil {
		_, completed := p.completed[message.CommandID]
		p.mu.Unlock()
		if completed {
			return protocolFailure("Provider Host emitted output after the terminal message")
		}
		return protocolFailure("Provider Host message referenced an unknown commandId")
	}
	if err := validateProviderHostMessageCorrelation(message, state.command); err != nil {
		p.mu.Unlock()
		return err
	}
	switch message.MessageType {
	case "Result", "Error":
		outcome := providerHostCommandOutcome{message: message}
		if message.MessageType == "Error" {
			outcome.err = failureFromProviderHost(message.Error)
		}
		delete(p.commands, message.CommandID)
		p.completed[message.CommandID] = providerHostCompletedCommand{command: state.command, outcome: outcome}
		p.mu.Unlock()
		state.result <- outcome
		close(state.result)
		return nil
	case "Event", "ArtifactCandidate", "InteractionRequest", "Checkpoint", "Progress":
		handle := state.handle
		p.mu.Unlock()
		if handle == nil {
			return protocolFailure("Provider Host emitted a non-terminal message for a control command")
		}
		runnerMessage, err := runnerMessageFromProviderHost(message)
		if err != nil {
			return err
		}
		return handle(runnerMessage)
	default:
		p.mu.Unlock()
		return protocolFailure("Provider Host emitted an unknown message type")
	}
}

func (p *providerHostV2Process) decodeMessage(line []byte) (providerHostMessage, error) {
	if len(line) > p.maximumMessageBytes {
		return providerHostMessage{}, protocolFailure("Provider Host message exceeds the negotiated size limit")
	}
	var message providerHostMessage
	if err := json.Unmarshal(line, &message); err != nil {
		return providerHostMessage{}, protocolFailure("Provider Host emitted malformed JSONL")
	}
	if message.ProtocolVersion.Major != providerHostProtocolMajor {
		return providerHostMessage{}, &runnerFailure{
			code:                 "provider_version_incompatible",
			message:              fmt.Sprintf("Provider Host message uses unsupported Protocol major %d", message.ProtocolVersion.Major),
			requiresNewExecution: true, requiresUserAction: true,
			canReconstructFromHistory: true, canMoveWorker: true,
		}
	}
	if strings.TrimSpace(message.RequestID) == "" || strings.TrimSpace(message.ExecutionID) == "" ||
		message.Generation <= 0 || strings.TrimSpace(message.CommandID) == "" ||
		strings.TrimSpace(message.OccurredAt) == "" || strings.TrimSpace(message.MessageType) == "" {
		return providerHostMessage{}, protocolFailure("Provider Host message omitted required correlation fields")
	}
	if _, err := time.Parse(time.RFC3339Nano, message.OccurredAt); err != nil {
		return providerHostMessage{}, protocolFailure("Provider Host message occurredAt is invalid")
	}
	return message, nil
}

func validateProviderHostMessageCorrelation(message providerHostMessage, command providerHostCommand) error {
	if message.RequestID != command.RequestID || message.ExecutionID != command.ExecutionID ||
		message.Generation != command.Generation || message.CommandID != command.CommandID {
		return protocolFailure("Provider Host message correlation fields do not match the command")
	}
	return nil
}

func (p *providerHostV2Process) finish() error {
	p.mu.Lock()
	if p.fatalErr != nil {
		err := p.fatalErr
		p.mu.Unlock()
		return err
	}
	if len(p.commands) > 0 {
		p.mu.Unlock()
		return protocolFailure("Provider Host still has active commands during shutdown")
	}
	p.closing = true
	p.mu.Unlock()
	if err := p.stdin.Close(); err != nil {
		return fmt.Errorf("close Provider Host stdin: %w", err)
	}
	<-p.readerDone
	p.mu.Lock()
	fatalErr := p.fatalErr
	p.mu.Unlock()
	if fatalErr != nil {
		return fatalErr
	}
	if err := p.waitProcess(); err != nil {
		return p.processFailure(err)
	}
	if err := p.credentialResult(); err != nil {
		return &runnerFailure{
			code: "credential_invalid", message: "Provider credential could not be delivered to the Provider Host",
			requiresNewExecution: true, requiresUserAction: true, canMoveWorker: true,
		}
	}
	return nil
}

func (p *providerHostV2Process) abort() {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	_ = p.stdin.Close()
	if p.command.Process != nil {
		_ = p.command.Process.Kill()
	}
	<-p.readerDone
	_ = p.waitProcess()
	_ = p.credentialResult()
}

func (p *providerHostV2Process) fail(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.fatalErr != nil {
		p.mu.Unlock()
		return
	}
	p.fatalErr = err
	p.closing = true
	states := make([]*providerHostCommandState, 0, len(p.commands))
	for _, state := range p.commands {
		states = append(states, state)
	}
	p.commands = make(map[string]*providerHostCommandState)
	p.mu.Unlock()
	for _, state := range states {
		state.result <- providerHostCommandOutcome{err: err}
		close(state.result)
	}
	_ = p.stdin.Close()
	if p.command.Process != nil {
		_ = p.command.Process.Kill()
	}
}

func (p *providerHostV2Process) waitProcess() error {
	p.waitOnce.Do(func() { p.waitErr = p.command.Wait() })
	return p.waitErr
}

func (p *providerHostV2Process) credentialResult() error {
	p.credentialOnce.Do(func() {
		if p.credentialWrite != nil {
			p.credentialErr = <-p.credentialWrite
		}
	})
	return p.credentialErr
}

func (p *providerHostV2Process) processFailure(err error) error {
	message := strings.TrimSpace(p.stderr.String())
	if message == "" {
		message = err.Error()
	}
	return &runnerFailure{
		code: "provider_unavailable", message: safeRunnerMessage("Provider Host failed: " + message),
		retryable: true, requiresNewExecution: true, canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func sameProviderHostCommand(left, right providerHostCommand) bool {
	if left.ExecutionID != right.ExecutionID || left.Generation != right.Generation ||
		left.CommandType != right.CommandType || left.CommandID != right.CommandID {
		return false
	}
	leftPayload, leftErr := json.Marshal(left.Payload)
	rightPayload, rightErr := json.Marshal(right.Payload)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func runnerMessageFromProviderHost(message providerHostMessage) (RunnerMessage, error) {
	occurredAt, _ := time.Parse(time.RFC3339Nano, message.OccurredAt)
	switch message.MessageType {
	case "Event":
		eventType, _ := message.Payload["eventType"].(string)
		if strings.TrimSpace(eventType) == "" {
			return RunnerMessage{}, protocolFailure("Provider Host Event omitted eventType")
		}
		payload := map[string]any{}
		if value, found := message.Payload["payload"]; found {
			decoded, ok := value.(map[string]any)
			if !ok {
				return RunnerMessage{}, protocolFailure("Provider Host Event payload is not an object")
			}
			payload = decoded
		}
		return RunnerMessage{Type: "event", EventType: eventType, Payload: payload, OccurredAt: &occurredAt}, nil
	case "ArtifactCandidate":
		artifactPayload := message.Payload
		if value, found := message.Payload["artifact"]; found {
			decoded, ok := value.(map[string]any)
			if !ok {
				return RunnerMessage{}, protocolFailure("Provider Host ArtifactCandidate artifact is not an object")
			}
			artifactPayload = decoded
		}
		artifact := &RunnerArtifact{
			Path: stringField(artifactPayload, "path"), Kind: stringField(artifactPayload, "kind"),
			OriginalName: stringField(artifactPayload, "originalName"), ContentType: stringField(artifactPayload, "contentType"),
		}
		if strings.TrimSpace(artifact.Path) == "" || strings.TrimSpace(artifact.Kind) == "" ||
			strings.TrimSpace(artifact.ContentType) == "" {
			return RunnerMessage{}, protocolFailure("Provider Host ArtifactCandidate omitted required fields")
		}
		return RunnerMessage{Type: "artifact", Artifact: artifact, OccurredAt: &occurredAt}, nil
	case "InteractionRequest":
		return RunnerMessage{Type: "interaction", Payload: message.Payload, OccurredAt: &occurredAt}, nil
	case "Checkpoint":
		return RunnerMessage{Type: "checkpoint", Payload: message.Payload, OccurredAt: &occurredAt}, nil
	case "Progress":
		return RunnerMessage{Type: "progress", Payload: message.Payload, OccurredAt: &occurredAt}, nil
	default:
		return RunnerMessage{}, protocolFailure("Provider Host message type is unsupported")
	}
}

func failureFromProviderHost(value *providerHostWireError) error {
	if value == nil || !stableProviderHostErrorCode(value.Code) || strings.TrimSpace(value.Message) == "" ||
		value.Retryable == nil || value.RequiresNewExecution == nil || value.RequiresUserAction == nil ||
		value.CanReconstructFromHistory == nil || value.CanMoveWorker == nil {
		return protocolFailure("Provider Host Error omitted required stable error fields")
	}
	return &runnerFailure{
		code: value.Code, message: safeRunnerMessage(value.Message), retryable: *value.Retryable,
		requiresNewExecution: *value.RequiresNewExecution, requiresUserAction: *value.RequiresUserAction,
		canReconstructFromHistory: *value.CanReconstructFromHistory, canMoveWorker: *value.CanMoveWorker,
	}
}

func stableProviderHostErrorCode(value string) bool {
	switch value {
	case "provider_not_installed", "provider_version_incompatible", "capability_unsupported",
		"credential_missing", "credential_invalid", "authentication_required", "session_resume_invalid",
		"session_resume_expired", "provider_rate_limited", "provider_unavailable", "workspace_invalid",
		"protocol_violation", "cancelled", "interrupted", "internal_error":
		return true
	default:
		return false
	}
}

func protocolFailure(message string) error {
	return &runnerFailure{
		code: "protocol_violation", message: safeRunnerMessage(message), requiresNewExecution: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func safeRunnerMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 2_000 {
		value = value[:2_000]
	}
	if value == "" {
		return "Provider Host failed"
	}
	return value
}

func normalizeProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "claude" || value == "claudeagent" {
		return "claudeagent"
	}
	return value
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func stringField(value map[string]any, key string) string {
	decoded, _ := value[key].(string)
	return decoded
}

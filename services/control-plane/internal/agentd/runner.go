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
	"sort"
	"strings"
)

type Runner struct {
	command               []string
	maxMessageBytes       int
	protocol              RunnerProtocol
	experimentalProviders map[string]struct{}
}

func NewRunner(cfg Config) *Runner {
	experimentalProviders := make(map[string]struct{}, len(cfg.ExperimentalProviders))
	for _, provider := range cfg.ExperimentalProviders {
		experimentalProviders[provider] = struct{}{}
	}
	return &Runner{
		command: append([]string(nil), cfg.RunnerCommand...), maxMessageBytes: cfg.RunnerMessageBytes,
		protocol: cfg.RunnerProtocol, experimentalProviders: experimentalProviders,
	}
}

func (r *Runner) experimentalProviderEnabled(provider string) bool {
	_, enabled := r.experimentalProviders[provider]
	return enabled
}

func (r *Runner) experimentalProviderList() []string {
	providers := make([]string, 0, len(r.experimentalProviders))
	for provider := range r.experimentalProviders {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

func (r *Runner) Run(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	return r.RunControlled(ctx, input, credential, nil, handle)
}

func (r *Runner) RunControlled(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	controls <-chan RunnerControl,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	if r.protocol == RunnerProtocolV2 {
		return r.runProviderHostV2(ctx, input, credential, controls, handle)
	}
	return r.runLegacy(ctx, input, credential, handle)
}

func (r *Runner) runLegacy(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return RunnerResult{}, fmt.Errorf("encode runner input: %w", err)
	}
	command := exec.Command(r.command[0], r.command[1:]...)
	processTree, err := newProcessTree(command)
	if err != nil {
		return RunnerResult{}, fmt.Errorf("prepare runner process tree: %w", err)
	}
	defer processTree.release()
	command.Dir = input.WorkspaceDirectory
	command.Env = runnerEnvironment(os.Environ())
	command.Stdin = bytes.NewReader(append(encoded, '\n'))
	var credentialWrite <-chan error
	if credential != nil {
		readPipe, writePipe, err := os.Pipe()
		if err != nil {
			return RunnerResult{}, fmt.Errorf("open runner credential pipe: %w", err)
		}
		defer readPipe.Close()
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
	stderr := &boundedBuffer{maximum: 64 << 10}
	outputPipes, err := newProcessOutputPipes(command, stderr)
	if err != nil {
		return RunnerResult{}, fmt.Errorf("open runner output pipes: %w", err)
	}
	defer outputPipes.close()
	if err := ctx.Err(); err != nil {
		return RunnerResult{}, err
	}
	if err := command.Start(); err != nil {
		return RunnerResult{}, fmt.Errorf("start runner: %w", err)
	}
	if err := processTree.started(); err != nil {
		_ = processTree.terminate()
		_ = command.Wait()
		return RunnerResult{}, fmt.Errorf("isolate runner process tree: %w", err)
	}
	outputPipes.started()
	if len(command.ExtraFiles) > 0 {
		_ = command.ExtraFiles[0].Close()
	}
	waitResult := make(chan error, 1)
	go func() {
		err := command.Wait()
		_ = processTree.terminate()
		waitResult <- err
	}()
	stopCancellation := context.AfterFunc(ctx, func() { _ = processTree.terminate() })
	defer stopCancellation()
	waitAfterTermination := func() {
		_ = processTree.terminate()
		<-waitResult
		outputPipes.waitStderr()
	}

	var result *RunnerResult
	scanner := bufio.NewScanner(outputPipes.stdoutRead)
	scanner.Buffer(make([]byte, 64*1024), r.maxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var message RunnerMessage
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&message); err != nil {
			waitAfterTermination()
			return RunnerResult{}, fmt.Errorf("decode runner message: %w", err)
		}
		switch message.Type {
		case "event":
			if strings.TrimSpace(message.EventType) == "" {
				waitAfterTermination()
				return RunnerResult{}, errors.New("runner event message requires eventType")
			}
			if message.Payload == nil {
				message.Payload = map[string]any{}
			}
			if err := handle(ctx, message); err != nil {
				waitAfterTermination()
				return RunnerResult{}, err
			}
		case "artifact":
			if message.Artifact == nil || strings.TrimSpace(message.Artifact.Path) == "" ||
				strings.TrimSpace(message.Artifact.Kind) == "" || strings.TrimSpace(message.Artifact.ContentType) == "" {
				waitAfterTermination()
				return RunnerResult{}, errors.New("runner artifact message requires path, kind, and contentType")
			}
			if err := handle(ctx, message); err != nil {
				waitAfterTermination()
				return RunnerResult{}, err
			}
		case "result":
			if result != nil {
				waitAfterTermination()
				return RunnerResult{}, errors.New("runner emitted more than one result message")
			}
			output := message.Output
			if output == nil {
				output = map[string]any{}
			}
			result = &RunnerResult{Output: output, ProviderResumeCursor: message.ProviderResumeCursor}
		default:
			waitAfterTermination()
			return RunnerResult{}, fmt.Errorf("unsupported runner message type %q", message.Type)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		waitAfterTermination()
		return RunnerResult{}, fmt.Errorf("read runner output: %w", scanErr)
	}
	waitErr := <-waitResult
	outputPipes.waitStderr()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RunnerResult{}, ctxErr
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		return RunnerResult{}, fmt.Errorf("runner failed: %s", message)
	}
	if credentialWrite != nil {
		if err := <-credentialWrite; err != nil {
			return RunnerResult{}, fmt.Errorf("write runner credential: %w", err)
		}
	}
	if result == nil {
		return RunnerResult{}, errors.New("runner exited without a result message")
	}
	return *result, nil
}

func runnerEnvironment(source []string) []string {
	result := make([]string, 0, len(source))
	for _, entry := range source {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		normalized := strings.ToUpper(strings.TrimSpace(name))
		if normalized == "SYNARA_AUTH_TOKEN" || normalized == "SYNARA_CONTROL_PLANE_URL" ||
			strings.HasPrefix(normalized, "SYNARA_WORKER_") ||
			strings.HasPrefix(normalized, "SYNARA_AGENTD_") ||
			strings.HasPrefix(normalized, "SYNARA_EXECUTION_TARGET_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

type boundedBuffer struct {
	buffer  bytes.Buffer
	maximum int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if b.buffer.Len() < b.maximum {
		remaining := b.maximum - b.buffer.Len()
		_, _ = b.buffer.Write(value[:min(len(value), remaining)])
	}
	return len(value), nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

var _ io.Writer = (*boundedBuffer)(nil)

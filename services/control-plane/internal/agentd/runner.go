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
)

type Runner struct {
	command         []string
	maxMessageBytes int
	protocol        RunnerProtocol
}

func NewRunner(cfg Config) *Runner {
	return &Runner{
		command: append([]string(nil), cfg.RunnerCommand...), maxMessageBytes: cfg.RunnerMessageBytes,
		protocol: cfg.RunnerProtocol,
	}
}

func (r *Runner) Run(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	if r.protocol == RunnerProtocolV2 {
		return r.runProviderHostV2(ctx, input, credential, handle)
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
	command := exec.CommandContext(ctx, r.command[0], r.command[1:]...)
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
	stdout, err := command.StdoutPipe()
	if err != nil {
		return RunnerResult{}, fmt.Errorf("open runner stdout: %w", err)
	}
	stderr := &boundedBuffer{maximum: 64 << 10}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return RunnerResult{}, fmt.Errorf("start runner: %w", err)
	}
	if len(command.ExtraFiles) > 0 {
		_ = command.ExtraFiles[0].Close()
	}

	var result *RunnerResult
	scanner := bufio.NewScanner(stdout)
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
			_ = command.Process.Kill()
			_ = command.Wait()
			return RunnerResult{}, fmt.Errorf("decode runner message: %w", err)
		}
		switch message.Type {
		case "event":
			if strings.TrimSpace(message.EventType) == "" {
				_ = command.Process.Kill()
				_ = command.Wait()
				return RunnerResult{}, errors.New("runner event message requires eventType")
			}
			if message.Payload == nil {
				message.Payload = map[string]any{}
			}
			if err := handle(ctx, message); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return RunnerResult{}, err
			}
		case "artifact":
			if message.Artifact == nil || strings.TrimSpace(message.Artifact.Path) == "" ||
				strings.TrimSpace(message.Artifact.Kind) == "" || strings.TrimSpace(message.Artifact.ContentType) == "" {
				_ = command.Process.Kill()
				_ = command.Wait()
				return RunnerResult{}, errors.New("runner artifact message requires path, kind, and contentType")
			}
			if err := handle(ctx, message); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return RunnerResult{}, err
			}
		case "result":
			if result != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return RunnerResult{}, errors.New("runner emitted more than one result message")
			}
			output := message.Output
			if output == nil {
				output = map[string]any{}
			}
			result = &RunnerResult{Output: output, ProviderResumeCursor: message.ProviderResumeCursor}
		default:
			_ = command.Process.Kill()
			_ = command.Wait()
			return RunnerResult{}, fmt.Errorf("unsupported runner message type %q", message.Type)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return RunnerResult{}, fmt.Errorf("read runner output: %w", scanErr)
	}
	if err := command.Wait(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
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

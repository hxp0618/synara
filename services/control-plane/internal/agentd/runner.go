package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type Runner struct {
	command                  []string
	maxMessageBytes          int
	protocol                 RunnerProtocol
	experimentalProviders    map[string]struct{}
	cgroupV2Root             string
	cgroupV2ProviderIdentity *ProtectedCgroupIdentity
	cgroupV2ProviderLimits   *ProtectedCgroupResourceLimits
	instanceUID              uuid.UUID
	supervisorInstance       uuid.UUID
	protectedRootLease       *ProtectedCgroupRootLease
	logger                   *slog.Logger
	prestartMu               sync.Mutex
	providerHostPrestart     *providerHostV2PrestartManager
}

func NewRunner(cfg Config) *Runner {
	experimentalProviders := make(map[string]struct{}, len(cfg.ExperimentalProviders))
	for _, provider := range cfg.ExperimentalProviders {
		experimentalProviders[provider] = struct{}{}
	}
	var instanceUID uuid.UUID
	if parsed, err := uuid.Parse(cfg.InstanceUID); err == nil {
		instanceUID = parsed
	}
	var providerIdentity *ProtectedCgroupIdentity
	if cfg.CgroupV2ProviderIdentity != nil {
		identityCopy := *cfg.CgroupV2ProviderIdentity
		providerIdentity = &identityCopy
	}
	var providerLimits *ProtectedCgroupResourceLimits
	if cfg.CgroupV2ProviderLimits != nil {
		limitsCopy := *cfg.CgroupV2ProviderLimits
		providerLimits = &limitsCopy
	}
	return &Runner{
		command: append([]string(nil), cfg.RunnerCommand...), maxMessageBytes: cfg.RunnerMessageBytes,
		protocol: cfg.RunnerProtocol, experimentalProviders: experimentalProviders,
		cgroupV2Root:             cfg.CgroupV2Root,
		cgroupV2ProviderIdentity: providerIdentity,
		cgroupV2ProviderLimits:   providerLimits,
		instanceUID:              instanceUID,
		supervisorInstance:       uuid.New(),
	}
}

func (r *Runner) experimentalProviderEnabled(provider string) bool {
	normalized := normalizeProvider(provider)
	for enabled := range r.experimentalProviders {
		if normalizeProvider(enabled) == normalized {
			return true
		}
	}
	return false
}

func (r *Runner) experimentalProviderList() []string {
	providers := make([]string, 0, len(r.experimentalProviders))
	for provider := range r.experimentalProviders {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

func (r *Runner) processTreeOptions(executionID uuid.UUID, generation int64) processTreeOptions {
	options := processTreeOptions{CgroupV2Root: r.cgroupV2Root}
	if r.cgroupV2ProviderIdentity == nil {
		return options
	}
	identityCopy := *r.cgroupV2ProviderIdentity
	options.ProtectedProviderIdentity = &identityCopy
	if r.cgroupV2ProviderLimits != nil {
		limitsCopy := *r.cgroupV2ProviderLimits
		options.ProtectedProviderLimits = &limitsCopy
	}
	options.ContainmentFence = ProtectedCgroupFence{
		ExecutionID:       executionID,
		Generation:        generation,
		WorkerIncarnation: r.instanceUID,
	}
	options.SupervisorInstance = r.supervisorInstance
	options.RuntimeInstance = uuid.New()
	options.ProtectedRootLease = r.protectedRootLease
	return options
}

func (r *Runner) providerProbeExecutionID() uuid.UUID {
	namespace := r.instanceUID
	if namespace == uuid.Nil {
		namespace = uuid.NameSpaceOID
	}
	return uuid.NewSHA1(namespace, []byte("synara-provider-probe"))
}

func (r *Runner) Run(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	return r.RunControlled(ctx, input, credential, nil, nil, handle)
}

func (r *Runner) RunControlled(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	primary *RunnerPrimaryOperationControl,
	controls <-chan RunnerControl,
	handle func(context.Context, RunnerMessage) error,
) (RunnerResult, error) {
	if r.protocol == RunnerProtocolV2 {
		return r.runProviderHostV2(ctx, input, credential, primary, controls, handle)
	}
	if input.Workload.PrimaryOperation != nil || primary != nil {
		return RunnerResult{}, &runnerFailure{
			code: "capability_unsupported", message: "Primary Provider operations require Provider Host Protocol v2.",
			requiresNewExecution: true, requiresUserAction: true, canMoveWorker: true,
		}
	}
	return r.runLegacy(ctx, input, credential, handle)
}

func (r *Runner) runLegacy(
	ctx context.Context,
	input RunnerInput,
	credential *RunnerCredential,
	handle func(context.Context, RunnerMessage) error,
) (returned RunnerResult, err error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return RunnerResult{}, fmt.Errorf("encode runner input: %w", err)
	}
	command := exec.Command(r.command[0], r.command[1:]...)
	processTree, err := newProcessTree(
		command,
		r.processTreeOptions(input.Execution.ID, input.Execution.Generation),
	)
	if err != nil {
		return RunnerResult{}, fmt.Errorf("prepare runner process tree: %w", err)
	}
	processTreeReleased := false
	releaseProcessTree := func() error {
		if processTreeReleased {
			return nil
		}
		processTreeReleased = true
		return processTree.release()
	}
	defer func() {
		err = errors.Join(err, releaseProcessTree())
	}()
	command.Dir = input.WorkspaceDirectory
	command.Env = runnerEnvironment(os.Environ())
	for _, name := range providerHostPackageEnvironmentAllowlist {
		if value, found := input.ProviderEnvironment[name]; found {
			if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n\x00") {
				return RunnerResult{}, errors.New("Provider execution environment contains an unsupported value")
			}
			command.Env = replaceEnvironmentValue(command.Env, name, value)
		}
	}
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
		return RunnerResult{}, errors.Join(err, releaseProcessTree())
	}
	if err := command.Start(); err != nil {
		return RunnerResult{}, errors.Join(fmt.Errorf("start runner: %w", err), releaseProcessTree())
	}
	if err := processTree.started(); err != nil {
		terminateErr := processTree.terminate()
		waitErr := command.Wait()
		return RunnerResult{}, errors.Join(
			fmt.Errorf("isolate runner process tree: %w", err),
			terminateErr,
			waitErr,
			releaseProcessTree(),
		)
	}
	outputPipes.started()
	if len(command.ExtraFiles) > 0 {
		_ = command.ExtraFiles[0].Close()
	}
	type waitOutcome struct {
		waitErr      error
		terminateErr error
	}
	waitResult := make(chan waitOutcome, 1)
	go func() {
		waitErr := command.Wait()
		waitResult <- waitOutcome{waitErr: waitErr, terminateErr: processTree.terminate()}
	}()
	stopCancellation := context.AfterFunc(ctx, func() { _ = processTree.terminate() })
	defer stopCancellation()
	waitAfterTermination := func() waitOutcome {
		_ = processTree.terminate()
		outcome := <-waitResult
		outputPipes.waitStderr()
		return outcome
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
			outcome := waitAfterTermination()
			return RunnerResult{}, errors.Join(
				fmt.Errorf("decode runner message: %w", err),
				outcome.terminateErr,
				releaseProcessTree(),
			)
		}
		switch message.Type {
		case "event":
			if strings.TrimSpace(message.EventType) == "" {
				outcome := waitAfterTermination()
				return RunnerResult{}, errors.Join(
					errors.New("runner event message requires eventType"),
					outcome.terminateErr,
					releaseProcessTree(),
				)
			}
			if message.Payload == nil {
				message.Payload = map[string]any{}
			}
			if err := handle(ctx, message); err != nil {
				outcome := waitAfterTermination()
				return RunnerResult{}, errors.Join(err, outcome.terminateErr, releaseProcessTree())
			}
		case "artifact":
			if message.Artifact == nil || strings.TrimSpace(message.Artifact.Path) == "" ||
				strings.TrimSpace(message.Artifact.Kind) == "" || strings.TrimSpace(message.Artifact.ContentType) == "" {
				outcome := waitAfterTermination()
				return RunnerResult{}, errors.Join(
					errors.New("runner artifact message requires path, kind, and contentType"),
					outcome.terminateErr,
					releaseProcessTree(),
				)
			}
			if err := handle(ctx, message); err != nil {
				outcome := waitAfterTermination()
				return RunnerResult{}, errors.Join(err, outcome.terminateErr, releaseProcessTree())
			}
		case "result":
			if result != nil {
				outcome := waitAfterTermination()
				return RunnerResult{}, errors.Join(
					errors.New("runner emitted more than one result message"),
					outcome.terminateErr,
					releaseProcessTree(),
				)
			}
			output := message.Output
			if output == nil {
				output = map[string]any{}
			}
			result = &RunnerResult{Output: output, ProviderResumeCursor: message.ProviderResumeCursor}
		default:
			outcome := waitAfterTermination()
			return RunnerResult{}, errors.Join(
				fmt.Errorf("unsupported runner message type %q", message.Type),
				outcome.terminateErr,
				releaseProcessTree(),
			)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		outcome := waitAfterTermination()
		return RunnerResult{}, errors.Join(
			fmt.Errorf("read runner output: %w", scanErr),
			outcome.terminateErr,
			releaseProcessTree(),
		)
	}
	outcome := <-waitResult
	outputPipes.waitStderr()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RunnerResult{}, errors.Join(ctxErr, outcome.waitErr, outcome.terminateErr, releaseProcessTree())
	}
	if outcome.waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = outcome.waitErr.Error()
		}
		return RunnerResult{}, errors.Join(
			fmt.Errorf("runner failed: %s", message),
			outcome.terminateErr,
			releaseProcessTree(),
		)
	}
	if credentialWrite != nil {
		if err := <-credentialWrite; err != nil {
			return RunnerResult{}, errors.Join(fmt.Errorf("write runner credential: %w", err), releaseProcessTree())
		}
	}
	if result == nil {
		return RunnerResult{}, errors.Join(errors.New("runner exited without a result message"), releaseProcessTree())
	}
	return *result, errors.Join(outcome.terminateErr, releaseProcessTree())
}

func runnerEnvironment(source []string) []string {
	return selectProcessEnvironment(source, runnerEnvironmentAllowlist)
}

var runnerEnvironmentAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"USERNAME",
	"USERPROFILE",
	"HOMEDRIVE",
	"HOMEPATH",
	"TMPDIR",
	"TMP",
	"TEMP",
	"SYSTEMROOT",
	"WINDIR",
	"COMSPEC",
	"PATHEXT",
	"LANG",
	"LANGUAGE",
	"LC_ALL",
	"LC_CTYPE",
	"LC_COLLATE",
	"LC_MESSAGES",
	"LC_MONETARY",
	"LC_NUMERIC",
	"LC_TIME",
	"LC_PAPER",
	"LC_NAME",
	"LC_ADDRESS",
	"LC_TELEPHONE",
	"LC_MEASUREMENT",
	"LC_IDENTIFICATION",
	"TZ",
	"TERM",
	"COLORTERM",
	"TERM_PROGRAM",
	"TERM_PROGRAM_VERSION",
	"SHELL",
	"NO_COLOR",
	"FORCE_COLOR",
	"CLICOLOR",
	"CLICOLOR_FORCE",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"NODE_EXTRA_CA_CERTS",
}

func selectProcessEnvironment(source []string, allowlist []string) []string {
	values := make(map[string]string, len(source))
	for _, entry := range source {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		normalized := strings.ToUpper(strings.TrimSpace(name))
		if normalized != "" {
			values[normalized] = value
		}
	}
	result := make([]string, 0, len(allowlist))
	for _, name := range allowlist {
		if value, found := values[name]; found {
			result = append(result, name+"="+value)
		}
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

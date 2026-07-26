//go:build linux

package agentd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/synara-ai/synara/services/control-plane/internal/containmentattestation"
)

const protectedCgroupProbeDigestInput = "synara.agentd.protected-cgroup-probe.v1\ncredential-drop\nUseCgroupFD\ncgroup.kill\nsetsid-descendant\nempty-helper-and-child-environment"

var (
	protectedCgroupProbeHook    = buildProtectedCgroupPreflightReport
	protectedCgroupProbeCommand = defaultProtectedCgroupProbeCommand
	protectedCgroupKeyLoader    = loadProtectedCgroupAttestationKeyMaterial
)

type protectedCgroupProbeIdentityReport struct {
	UID                uint32 `json:"uid"`
	GID                uint32 `json:"gid"`
	EnvironmentEntries int    `json:"environmentEntries"`
}

type protectedCgroupProbeReadResult struct {
	identity protectedCgroupProbeIdentityReport
	err      error
}

type protectedCgroupProbePipes struct {
	reportRead    *os.File
	reportWrite   *os.File
	readyRead     *os.File
	readyWrite    *os.File
	sentinelRead  *os.File
	sentinelWrite *os.File
}

func RunProtectedCgroupCommand(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	if len(args) < 2 {
		return false, nil
	}
	switch args[1] {
	case protectedCgroupProbeHelperCommand:
		if len(args) != 2 {
			return true, errors.New("protected cgroup probe helper arguments are invalid")
		}
		return true, runProtectedCgroupProbeHelper()
	case protectedCgroupProbeChildCommand:
		if len(args) != 2 {
			return true, errors.New("protected cgroup probe child arguments are invalid")
		}
		return true, runProtectedCgroupProbeChild()
	case protectedCgroupPreflightCommand:
		cfg, err := LoadConfig()
		if err != nil {
			return true, err
		}
		report, err := protectedCgroupProbeHook(ctx, cfg)
		if err != nil {
			return true, err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return true, encoder.Encode(report)
	default:
		return false, nil
	}
}

func buildProtectedCgroupPreflightReport(ctx context.Context, cfg Config) (*ProtectedCgroupPreflightReport, error) {
	if strings.TrimSpace(cfg.CgroupV2Root) == "" || cfg.CgroupV2ProviderIdentity == nil {
		return &ProtectedCgroupPreflightReport{}, nil
	}
	probeContext, cancelProbe := protectedCgroupPreflightContext(ctx, cfg)
	defer cancelProbe()
	liveProbe, err := runProtectedCgroupLiveProbe(probeContext, cfg)
	if err != nil {
		return nil, err
	}
	report := &ProtectedCgroupPreflightReport{
		Enabled:                true,
		Mode:                   protectedCgroupContainmentMode,
		SupervisorVersion:      protectedCgroupSupervisorVersion,
		ProbeVersion:           protectedCgroupProbeVersion,
		ProbeSHA256:            protectedCgroupProbeSHA256(),
		SupervisorIdentity:     protectedCgroupIdentityString(currentProtectedCgroupSupervisorIdentity()),
		ProviderIdentity:       protectedCgroupIdentityString(*cfg.CgroupV2ProviderIdentity),
		UseCgroupFD:            liveProbe.UseCgroupFD,
		SetsidDescendantKilled: liveProbe.SetsidDescendantKilled,
		ProviderUID:            liveProbe.ProviderUID,
		ProviderGID:            liveProbe.ProviderGID,
	}
	if cfg.CgroupV2Attestation == nil {
		return report, nil
	}
	if strings.TrimSpace(cfg.BuildGitSHA) == "" || !validBuildGitSHA(cfg.BuildGitSHA) {
		return nil, errors.New("protected cgroup attestation requires a valid SYNARA_AGENTD_BUILD_GIT_SHA")
	}
	if strings.TrimSpace(cfg.ImageDigest) == "" || !validImageDigest(cfg.ImageDigest) {
		return nil, errors.New("protected cgroup attestation requires a valid SYNARA_AGENTD_IMAGE_DIGEST")
	}
	privateKey, publicKey, err := protectedCgroupKeyLoader(*cfg.CgroupV2Attestation)
	if err != nil {
		return nil, err
	}
	statement := containmentattestation.Statement{
		ExecutionTargetID:  cfg.ExecutionTargetID.String(),
		TargetKind:         string(cfg.TargetKind),
		InstanceUID:        cfg.InstanceUID,
		ClusterID:          cfg.ClusterID,
		Namespace:          cfg.Namespace,
		PodName:            cfg.PodName,
		WorkerBuildVersion: cfg.Version,
		WorkerBuildGitSHA:  cfg.BuildGitSHA,
		ImageDigest:        cfg.ImageDigest,
		OperatingSystem:    "linux",
		Architecture:       runtimeArchitecture(),
		Mode:               report.Mode,
		SupervisorVersion:  report.SupervisorVersion,
		ProbeVersion:       report.ProbeVersion,
		ProbeSHA256:        report.ProbeSHA256,
		SupervisorIdentity: report.SupervisorIdentity,
		ProviderIdentity:   report.ProviderIdentity,
	}
	envelope, err := containmentattestation.Sign(privateKey, cfg.CgroupV2Attestation.KeyID, statement)
	if err != nil {
		return nil, fmt.Errorf("sign protected cgroup attestation: %w", err)
	}
	publicKeySHA256, err := containmentattestation.PublicKeySHA256(publicKey)
	if err != nil {
		return nil, err
	}
	report.AttestationKeyID = cfg.CgroupV2Attestation.KeyID
	report.AttestationPublicKeySHA256 = publicKeySHA256
	report.AttestationPublicKeyBase64 = encodeProtectedCgroupPublicKeyBase64(publicKey)
	report.Attestation = &envelope
	return report, nil
}

func runProtectedCgroupLiveProbe(ctx context.Context, cfg Config) (*ProtectedCgroupPreflightReport, error) {
	workerIncarnation := probeWorkerIncarnation(cfg)
	fence := ProtectedCgroupFence{
		ExecutionID:       uuid.NewSHA1(workerIncarnation, []byte("synara-protected-cgroup-preflight")),
		Generation:        time.Now().UTC().UnixNano(),
		WorkerIncarnation: workerIncarnation,
	}
	pipes, err := openProtectedCgroupProbePipes()
	if err != nil {
		return nil, err
	}
	defer closeProtectedCgroupProbeFiles(
		pipes.reportRead,
		pipes.readyRead,
		pipes.sentinelRead,
		pipes.reportWrite,
		pipes.readyWrite,
		pipes.sentinelWrite,
	)
	command, err := protectedCgroupProbeCommand(pipes.reportWrite, pipes.readyWrite, pipes.sentinelWrite)
	if err != nil {
		return nil, err
	}
	tree, err := newProcessTree(command, processTreeOptions{
		CgroupV2Root:              cfg.CgroupV2Root,
		ProtectedProviderIdentity: cfg.CgroupV2ProviderIdentity,
		ContainmentFence:          fence,
		SupervisorInstance:        uuid.New(),
		RuntimeInstance:           uuid.New(),
		ProtectedRootLease:        protectedCgroupRootLeaseFromContext(ctx),
		ProtectedDiagnostic:       true,
	})
	if err != nil {
		return nil, err
	}
	started := false
	waited := false
	waitCommand := func() error {
		if !started || waited {
			return nil
		}
		waited = true
		return waitProtectedCgroupProbeCommand(command)
	}
	terminateAndWait := func() error {
		if !started {
			return nil
		}
		return errors.Join(tree.terminate(), waitCommand())
	}
	if err := validateProtectedCgroupProbeCommand(command, *cfg.CgroupV2ProviderIdentity); err != nil {
		return nil, errors.Join(err, tree.release())
	}
	if err := command.Start(); err != nil {
		_ = tree.release()
		return nil, fmt.Errorf("start protected cgroup probe: %w", err)
	}
	started = true
	if err := closeProtectedCgroupProbeFiles(pipes.reportWrite, pipes.readyWrite, pipes.sentinelWrite); err != nil {
		return nil, errors.Join(fmt.Errorf("close protected cgroup probe pipes: %w", err), terminateAndWait())
	}
	pipes.reportWrite = nil
	pipes.readyWrite = nil
	pipes.sentinelWrite = nil
	if err := tree.started(); err != nil {
		return nil, errors.Join(fmt.Errorf("isolate protected cgroup probe: %w", err), terminateAndWait())
	}
	identity, err := waitForProtectedCgroupProbeReady(ctx, pipes.reportRead, pipes.readyRead)
	if err != nil {
		return nil, errors.Join(err, terminateAndWait())
	}
	if identity.UID != cfg.CgroupV2ProviderIdentity.UID || identity.GID != cfg.CgroupV2ProviderIdentity.GID {
		return nil, errors.Join(fmt.Errorf(
			"protected cgroup probe child ran as uid=%d gid=%d, want uid=%d gid=%d",
			identity.UID,
			identity.GID,
			cfg.CgroupV2ProviderIdentity.UID,
			cfg.CgroupV2ProviderIdentity.GID,
		), terminateAndWait())
	}
	if identity.EnvironmentEntries != 0 {
		return nil, errors.Join(fmt.Errorf(
			"protected cgroup probe helper inherited %d environment entries",
			identity.EnvironmentEntries,
		), terminateAndWait())
	}
	if err := terminateAndWait(); err != nil {
		return nil, err
	}
	if err := assertProtectedCgroupProbeSentinelKilled(pipes.sentinelRead); err != nil {
		return nil, err
	}
	return &ProtectedCgroupPreflightReport{
		UseCgroupFD:            true,
		SetsidDescendantKilled: true,
		ProviderUID:            identity.UID,
		ProviderGID:            identity.GID,
	}, nil
}

func defaultProtectedCgroupProbeCommand(reportWrite, readyWrite, sentinelWrite *os.File) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve protected cgroup probe executable: %w", err)
	}
	command := exec.Command(executable, protectedCgroupProbeHelperCommand)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{reportWrite, readyWrite, sentinelWrite}
	return command, nil
}

func waitForProtectedCgroupProbeReady(
	ctx context.Context,
	reportReader io.Reader,
	readyReader io.Reader,
) (protectedCgroupProbeIdentityReport, error) {
	reportResultChannel := make(chan protectedCgroupProbeReadResult, 1)
	readyResultChannel := make(chan error, 1)
	go func() {
		reportResultChannel <- readProtectedCgroupProbeIdentityReport(reportReader)
	}()
	go func() {
		readyResultChannel <- readProtectedCgroupProbeReadySignal(readyReader)
	}()
	var (
		report    protectedCgroupProbeIdentityReport
		gotReport bool
		gotReady  bool
	)
	for !gotReport || !gotReady {
		select {
		case <-ctx.Done():
			return protectedCgroupProbeIdentityReport{}, fmt.Errorf("wait protected cgroup probe readiness: %w", ctx.Err())
		case result := <-reportResultChannel:
			if result.err != nil {
				return protectedCgroupProbeIdentityReport{}, result.err
			}
			report = result.identity
			gotReport = true
			reportResultChannel = nil
		case err := <-readyResultChannel:
			if err != nil {
				return protectedCgroupProbeIdentityReport{}, err
			}
			gotReady = true
			readyResultChannel = nil
		}
	}
	return report, nil
}

func readProtectedCgroupProbeIdentityReport(reader io.Reader) protectedCgroupProbeReadResult {
	reportBytes, err := io.ReadAll(reader)
	if err != nil {
		return protectedCgroupProbeReadResult{err: fmt.Errorf("read protected cgroup probe report: %w", err)}
	}
	var report protectedCgroupProbeIdentityReport
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		return protectedCgroupProbeReadResult{err: fmt.Errorf("decode protected cgroup probe report: %w", err)}
	}
	return protectedCgroupProbeReadResult{identity: report}
}

func readProtectedCgroupProbeReadySignal(reader io.Reader) error {
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read protected cgroup probe ready signal: %w", err)
	}
	if strings.TrimSpace(line) != "ready environmentEntries=0" {
		return errors.New("protected cgroup probe returned an invalid readiness signal")
	}
	return nil
}

func runProtectedCgroupProbeHelper() error {
	reportWriter := os.NewFile(uintptr(3), "protected-cgroup-probe-report")
	readyWriter := os.NewFile(uintptr(4), "protected-cgroup-probe-ready")
	sentinelWriter := os.NewFile(uintptr(5), "protected-cgroup-probe-sentinel")
	defer reportWriter.Close()
	defer readyWriter.Close()
	defer sentinelWriter.Close()
	identity := protectedCgroupProbeIdentityReport{
		UID:                uint32(os.Geteuid()),
		GID:                uint32(os.Getegid()),
		EnvironmentEntries: len(os.Environ()),
	}
	if err := json.NewEncoder(reportWriter).Encode(identity); err != nil {
		return err
	}
	if err := reportWriter.Close(); err != nil {
		return err
	}
	command := exec.Command(os.Args[0], protectedCgroupProbeChildCommand)
	command.Env = []string{}
	command.ExtraFiles = []*os.File{readyWriter, sentinelWriter}
	if err := command.Start(); err != nil {
		return err
	}
	if err := readyWriter.Close(); err != nil {
		return err
	}
	if err := sentinelWriter.Close(); err != nil {
		return err
	}
	for {
		time.Sleep(time.Hour)
	}
}

func runProtectedCgroupProbeChild() error {
	readyWriter := os.NewFile(uintptr(3), "protected-cgroup-probe-ready")
	sentinelWriter := os.NewFile(uintptr(4), "protected-cgroup-probe-sentinel")
	defer readyWriter.Close()
	defer sentinelWriter.Close()
	if _, err := unix.Setsid(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(readyWriter, "ready environmentEntries=%d\n", len(os.Environ())); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := io.WriteString(sentinelWriter, "escaped\n"); err != nil {
		return err
	}
	return nil
}

func protectedCgroupProbeSHA256() string {
	sum := sha256.Sum256([]byte(protectedCgroupProbeDigestInput))
	return hex.EncodeToString(sum[:])
}

func protectedCgroupIdentityString(identity ProtectedCgroupIdentity) string {
	return fmt.Sprintf("uid:%d gid:%d", identity.UID, identity.GID)
}

func probeWorkerIncarnation(cfg Config) uuid.UUID {
	if parsed, err := uuid.Parse(strings.TrimSpace(cfg.InstanceUID)); err == nil && parsed != uuid.Nil {
		return parsed
	}
	return uuid.New()
}

func loadProtectedCgroupAttestationKeyMaterial(
	config ProtectedCgroupAttestationConfig,
) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	supervisorIdentity := currentProtectedCgroupSupervisorIdentity()
	if supervisorIdentity.UID != 0 {
		return nil, nil, errors.New("protected cgroup attestation requires a root supervisor identity")
	}
	fd, err := unix.Open(config.PrivateKeyPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open protected cgroup attestation key: %w", err)
	}
	file := os.NewFile(uintptr(fd), config.PrivateKeyPath)
	defer file.Close()
	var stats unix.Stat_t
	if err := unix.Fstat(fd, &stats); err != nil {
		return nil, nil, fmt.Errorf("stat protected cgroup attestation key: %w", err)
	}
	if stats.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, nil, errors.New("protected cgroup attestation key must be a regular file")
	}
	if stats.Uid != 0 {
		return nil, nil, errors.New("protected cgroup attestation key must be owned by root")
	}
	if os.FileMode(stats.Mode)&0o077 != 0 {
		return nil, nil, errors.New("protected cgroup attestation key permissions must deny group and other access")
	}
	payload, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, fmt.Errorf("read protected cgroup attestation key: %w", err)
	}
	privateKey, err := parseProtectedCgroupAttestationPrivateKey(payload)
	if err != nil {
		return nil, nil, err
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, nil, errors.New("protected cgroup attestation key is not an Ed25519 key")
	}
	return append(ed25519.PrivateKey(nil), privateKey...), append(ed25519.PublicKey(nil), publicKey...), nil
}

func parseProtectedCgroupAttestationPrivateKey(payload []byte) (ed25519.PrivateKey, error) {
	if signer, err := ssh.ParseRawPrivateKey(payload); err == nil {
		switch typed := signer.(type) {
		case ed25519.PrivateKey:
			return append(ed25519.PrivateKey(nil), typed...), nil
		case *ed25519.PrivateKey:
			return append(ed25519.PrivateKey(nil), (*typed)...), nil
		}
	}
	block, _ := pem.Decode(payload)
	if block == nil {
		return nil, errors.New("protected cgroup attestation key is not a supported Ed25519 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("protected cgroup attestation key is not a supported Ed25519 private key")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("protected cgroup attestation key is not a supported Ed25519 private key")
	}
	return append(ed25519.PrivateKey(nil), privateKey...), nil
}

func runtimeArchitecture() string {
	return runtime.GOARCH
}

func openProtectedCgroupProbePipes() (*protectedCgroupProbePipes, error) {
	reportRead, reportWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open protected cgroup probe report pipe: %w", err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = closeProtectedCgroupProbeFiles(reportRead, reportWrite)
		return nil, fmt.Errorf("open protected cgroup probe ready pipe: %w", err)
	}
	sentinelRead, sentinelWrite, err := os.Pipe()
	if err != nil {
		_ = closeProtectedCgroupProbeFiles(reportRead, reportWrite, readyRead, readyWrite)
		return nil, fmt.Errorf("open protected cgroup probe sentinel pipe: %w", err)
	}
	return &protectedCgroupProbePipes{
		reportRead: reportRead, reportWrite: reportWrite,
		readyRead: readyRead, readyWrite: readyWrite,
		sentinelRead: sentinelRead, sentinelWrite: sentinelWrite,
	}, nil
}

func closeProtectedCgroupProbeFiles(files ...*os.File) error {
	var result error
	for _, file := range files {
		if file == nil {
			continue
		}
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func validateProtectedCgroupProbeCommand(
	command *exec.Cmd,
	providerIdentity ProtectedCgroupIdentity,
) error {
	if command.SysProcAttr == nil || command.SysProcAttr.Credential == nil ||
		command.SysProcAttr.Credential.Uid != providerIdentity.UID ||
		command.SysProcAttr.Credential.Gid != providerIdentity.GID ||
		!command.SysProcAttr.Credential.NoSetGroups ||
		!command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD <= 0 {
		return errors.New("protected cgroup probe did not configure Linux credential drop and UseCgroupFD")
	}
	return nil
}

func waitProtectedCgroupProbeCommand(command *exec.Cmd) error {
	err := command.Wait()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return fmt.Errorf("wait protected cgroup probe: %w", err)
}

func assertProtectedCgroupProbeSentinelKilled(reader io.Reader) error {
	payload, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read protected cgroup probe sentinel: %w", err)
	}
	if len(bytes.TrimSpace(payload)) != 0 {
		return errors.New("protected cgroup probe setsid descendant survived cgroup.kill")
	}
	return nil
}

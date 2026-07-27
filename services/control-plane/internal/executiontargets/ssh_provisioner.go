package executiontargets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/cgroupv2limits"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/workertiming"
)

type SSHProvisioningConfig struct {
	AgentdBinaryPath       string
	RegistrationToken      string
	PublicControlPlaneURL  string
	WorkerLeaseTTL         time.Duration
	WorkerHeartbeatTimeout time.Duration
	Timeout                time.Duration
}

const protectedCgroupSupervisorSubgroupName = "synara-agentd"

type SSHProvisionResult struct {
	TargetID     uuid.UUID `json:"targetId"`
	Operation    string    `json:"operation"`
	Status       string    `json:"status"`
	ServiceName  string    `json:"serviceName"`
	BinarySHA256 string    `json:"binarySha256,omitempty"`
}

type SSHWorkerAuthorityRevoker func(
	context.Context,
	*gorm.DB,
	identity.Principal,
	persistence.ExecutionTarget,
	int64,
	string,
	string,
	string,
) (func(), error)

type sshTargetOperationFence struct {
	Generation int64
	Kind       string
}

type sshTargetConfiguration struct {
	Host                       string   `json:"host"`
	Port                       int      `json:"port"`
	User                       string   `json:"user"`
	PrivateKey                 string   `json:"privateKey"`
	PrivateKeyPassphrase       string   `json:"privateKeyPassphrase"`
	HostKey                    string   `json:"hostKey"`
	ControlPlaneURL            string   `json:"controlPlaneUrl"`
	AllowInsecureControlPlane  bool     `json:"allowInsecureControlPlane"`
	RunnerCommand              []string `json:"runnerCommand"`
	WorkspaceRoot              string   `json:"workspaceRoot"`
	GitCacheRoot               string   `json:"gitCacheRoot"`
	InstallRoot                string   `json:"installRoot"`
	ServiceUser                string   `json:"serviceUser"`
	UseSudo                    *bool    `json:"useSudo"`
	AgentdVersion              string   `json:"agentdVersion"`
	AgentdBuildGitSHA          string   `json:"agentdBuildGitSha"`
	AgentdImageDigest          string   `json:"agentdImageDigest"`
	CgroupV2Root               string   `json:"cgroupV2Root"`
	CgroupV2ProviderUID        *int     `json:"cgroupV2ProviderUid"`
	CgroupV2ProviderGID        *int     `json:"cgroupV2ProviderGid"`
	CgroupV2ProviderPidsMax    *int64   `json:"cgroupV2ProviderPidsMax"`
	CgroupV2ProviderMemoryMax  *int64   `json:"cgroupV2ProviderMemoryMaxBytes"`
	CgroupV2ProviderCPUQuota   *int64   `json:"cgroupV2ProviderCpuQuotaMicros"`
	CgroupV2ProviderCPUPeriod  *int64   `json:"cgroupV2ProviderCpuPeriodMicros"`
	CgroupV2AttestationKeyID   string   `json:"cgroupV2AttestationKeyId"`
	CgroupV2AttestationKeyPath string   `json:"cgroupV2AttestationPrivateKeyPath"`
}

type sshDialInput struct {
	Address              string
	User                 string
	PrivateKey           []byte
	PrivateKeyPassphrase []byte
	HostKey              []byte
	Timeout              time.Duration
}

type sshRemote interface {
	Upload(context.Context, string, os.FileMode, io.Reader) error
	Run(context.Context, string) error
	Close() error
}

const sshInstallConflictExitStatus = 73

var errSSHInstallConflict = errors.New("SSH install target-scoped path conflict")

type sshRemoteCommandExitError struct {
	status int
}

func (e *sshRemoteCommandExitError) Error() string {
	return fmt.Sprintf("SSH remote command exited with status %d", e.status)
}

func (e *sshRemoteCommandExitError) ExitStatus() int {
	return e.status
}

type sshDialer interface {
	Dial(context.Context, sshDialInput) (sshRemote, error)
}

type SSHProvisioner struct {
	targets          *Service
	config           SSHProvisioningConfig
	dialer           sshDialer
	awaitWorkerReady func(context.Context, persistence.ExecutionTarget, sshTargetConfiguration, string) error
	checkWorkerReady func(context.Context, *gorm.DB, persistence.ExecutionTarget, sshTargetConfiguration, string) (bool, string, error)
	lockWorkerReady  func(context.Context, *gorm.DB, uuid.UUID, string) error
	revokeWorkers    SSHWorkerAuthorityRevoker
	readinessPoll    time.Duration
	now              func() time.Time
}

func (p *SSHProvisioner) SetWorkerAuthorityRevoker(revoker SSHWorkerAuthorityRevoker) {
	p.revokeWorkers = revoker
}

func NewSSHProvisioner(targets *Service, config SSHProvisioningConfig) *SSHProvisioner {
	provisioner := &SSHProvisioner{
		targets: targets, config: config, dialer: realSSHDialer{},
		readinessPoll: 250 * time.Millisecond,
		now:           func() time.Time { return time.Now().UTC() },
	}
	provisioner.awaitWorkerReady = provisioner.waitForSSHWorkerReady
	provisioner.checkWorkerReady = provisioner.sshWorkerReadyWithDB
	provisioner.lockWorkerReady = lockExactSSHWorkerReadiness
	return provisioner
}

func (p *SSHProvisioner) Install(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	requestID, ipAddress string,
) (SSHProvisionResult, error) {
	return p.apply(ctx, principal, tenantID, targetID, "install", requestID, ipAddress)
}

func (p *SSHProvisioner) Upgrade(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	requestID, ipAddress string,
) (SSHProvisionResult, error) {
	return p.apply(ctx, principal, tenantID, targetID, "upgrade", requestID, ipAddress)
}

func (p *SSHProvisioner) Revoke(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	requestID, ipAddress string,
) (SSHProvisionResult, error) {
	target, err := p.loadTargetMetadata(ctx, principal, tenantID, targetID)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	if p.revokeWorkers == nil {
		return SSHProvisionResult{}, problem.New(503, "ssh_worker_revocation_unavailable", "SSH Worker authority revocation is not configured.")
	}
	fence, err := p.beginSSHRevokeOperation(ctx, target, principal, requestID, ipAddress)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	fail := func(primary error) error {
		return errors.Join(primary, p.failSSHOperation(ctx, target, principal.UserID, fence, requestID, ipAddress))
	}
	configuration, err := decryptSSHConfiguration(p.targets, target.ConfigurationEncrypted)
	if err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	configuration, paths, err := p.normalize(target, configuration)
	if err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	remote, err := p.connect(ctx, configuration)
	if err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	defer remote.Close()
	operationContext, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	command := paths.prefix + "sh -c " + shellQuote(strings.Join([]string{
		"if systemctl cat " + shellQuote(paths.serviceName) + " >/dev/null 2>&1; then systemctl disable --now " + shellQuote(paths.serviceName) + "; fi",
		"rm -f " + shellQuote(paths.unitPath) + " " + shellQuote(paths.envPath) + " " + shellQuote(paths.binaryPath),
		"systemctl daemon-reload",
		"systemctl reset-failed " + shellQuote(paths.serviceName) + " >/dev/null 2>&1 || true",
	}, " && "))
	if err := remote.Run(operationContext, command); err != nil {
		return SSHProvisionResult{}, fail(problem.Wrap(502, "ssh_revoke_failed", "SSH Worker revocation failed.", err))
	}
	if err := p.finishSSHOperation(ctx, target, principal.UserID, fence, "completed", "disabled", requestID, ipAddress); err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	return SSHProvisionResult{
		TargetID: target.ID, Operation: "revoke", Status: "disabled", ServiceName: paths.serviceName,
	}, nil
}

func (p *SSHProvisioner) apply(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	operation, requestID, ipAddress string,
) (SSHProvisionResult, error) {
	target, configuration, err := p.load(ctx, principal, tenantID, targetID)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	if strings.TrimSpace(p.config.RegistrationToken) == "" {
		return SSHProvisionResult{}, problem.New(503, "worker_registration_unavailable", "Worker registration is not configured.")
	}
	configuration, paths, err := p.normalize(target, configuration)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	binary, err := os.Open(strings.TrimSpace(p.config.AgentdBinaryPath))
	if err != nil {
		return SSHProvisionResult{}, problem.Wrap(503, "agentd_binary_unavailable", "The synara-agentd binary is unavailable.", err)
	}
	defer binary.Close()
	workerInstanceUID := uuid.New()
	fence, err := p.beginSSHOperation(
		ctx, target, principal.UserID, operation, &workerInstanceUID, requestID, ipAddress,
	)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	fail := func(primary error) error {
		return errors.Join(primary, p.failSSHOperation(ctx, target, principal.UserID, fence, requestID, ipAddress))
	}
	remote, err := p.connect(ctx, configuration)
	if err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	defer remote.Close()
	operationContext, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	if operation == "install" {
		if err := ensureSSHInstallPathsAvailable(operationContext, remote, paths); err != nil {
			if !errors.Is(err, errSSHInstallConflict) {
				return SSHProvisionResult{}, fail(problem.Wrap(
					502,
					"ssh_install_preflight_failed",
					"SSH Worker installation preflight failed.",
					err,
				))
			}
			return SSHProvisionResult{}, fail(problem.Wrap(
				409,
				"ssh_install_conflict",
				"SSH Worker installation refused existing target-scoped paths.",
				err,
			))
		}
		if err := ensureSSHInstallProtectedCgroupPaths(operationContext, remote, paths, configuration); err != nil {
			return SSHProvisionResult{}, fail(problem.Wrap(
				502,
				"ssh_install_preflight_failed",
				"SSH Worker installation preflight failed.",
				err,
			))
		}
	}
	defer cleanupSSHTemporaryFiles(remote, paths)
	hash := sha256.New()
	if err := remote.Upload(operationContext, paths.temporaryBinaryPath, 0o700, io.TeeReader(binary, hash)); err != nil {
		return SSHProvisionResult{}, fail(problem.Wrap(502, "ssh_agentd_upload_failed", "synara-agentd could not be uploaded.", err))
	}
	environment, err := p.environmentFile(target, configuration, paths, workerInstanceUID.String(), fence.Generation)
	if err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	if err := remote.Upload(operationContext, paths.temporaryEnvPath, 0o600, bytes.NewReader(environment)); err != nil {
		return SSHProvisionResult{}, fail(problem.Wrap(502, "ssh_agentd_upload_failed", "The synara-agentd environment could not be uploaded.", err))
	}
	unit := []byte(systemdUnitWithDelegate(paths, configuration.ServiceUser, configuration.protectedCgroupEnabled()))
	if err := remote.Upload(operationContext, paths.temporaryUnitPath, 0o600, bytes.NewReader(unit)); err != nil {
		return SSHProvisionResult{}, fail(problem.Wrap(502, "ssh_agentd_upload_failed", "The synara-agentd service unit could not be uploaded.", err))
	}
	commands := append(sshInstallDirectoryCommands(paths, configuration),
		"install -m 0755 "+shellQuote(paths.temporaryBinaryPath)+" "+shellQuote(paths.binaryPath),
		"install -m 0600 "+shellQuote(paths.temporaryEnvPath)+" "+shellQuote(paths.envPath),
		"install -m 0644 "+shellQuote(paths.temporaryUnitPath)+" "+shellQuote(paths.unitPath),
		"rm -f "+shellQuote(paths.temporaryBinaryPath)+" "+shellQuote(paths.temporaryEnvPath)+" "+shellQuote(paths.temporaryUnitPath),
		"systemctl daemon-reload",
		"systemctl enable "+shellQuote(paths.serviceName),
		"systemctl restart "+shellQuote(paths.serviceName),
		sshServiceStableCommand(paths.serviceName),
	)
	if configuration.protectedCgroupEnabled() {
		commands = append(commands,
			"test \"$(systemctl show "+shellQuote(paths.serviceName)+" --property=ControlGroup --value)\" = "+shellQuote("/system.slice/"+paths.serviceName),
			"test \"$(systemctl show "+shellQuote(paths.serviceName)+" --property=DelegateSubgroup --value)\" = "+shellQuote(protectedCgroupSupervisorSubgroupName),
			"test -d "+shellQuote(configuration.CgroupV2Root),
			"test -d "+shellQuote(configuration.CgroupV2Root+"/"+protectedCgroupSupervisorSubgroupName),
			"test \"$(stat -fc %T "+shellQuote(configuration.CgroupV2Root)+")\" = cgroup2fs",
			"test -z \"$(cat "+shellQuote(configuration.CgroupV2Root+"/cgroup.procs")+")\"",
			"test \"$(cat "+shellQuote(configuration.CgroupV2Root+"/"+protectedCgroupSupervisorSubgroupName+"/cgroup.procs")+")\" = \"$(systemctl show "+shellQuote(paths.serviceName)+" --property=MainPID --value)\"",
		)
	}
	command := paths.prefix + "sh -c " + shellQuote(strings.Join(commands, " && "))
	if err := remote.Run(operationContext, command); err != nil {
		return SSHProvisionResult{}, fail(problem.Wrap(502, "ssh_provision_failed", "SSH Worker provisioning failed.", err))
	}
	if err := p.awaitWorkerReady(operationContext, target, configuration, workerInstanceUID.String()); err != nil {
		return SSHProvisionResult{}, fail(problem.Wrap(
			502,
			"ssh_worker_readiness_failed",
			"SSH Worker did not establish its exact registered runtime readiness.",
			err,
		))
	}
	if err := p.activateReadySSHWorker(
		ctx,
		target,
		configuration,
		workerInstanceUID.String(),
		fence,
		principal.UserID,
		requestID,
		ipAddress,
	); err != nil {
		return SSHProvisionResult{}, fail(err)
	}
	return SSHProvisionResult{
		TargetID: target.ID, Operation: operation, Status: "active", ServiceName: paths.serviceName,
		BinarySHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func sshServiceStableCommand(serviceName string) string {
	service := shellQuote(serviceName)
	checks := strings.Join([]string{
		"sleep 1",
		"test \"$(systemctl show " + service + " --property=ActiveState --value)\" = active",
		"test \"$(systemctl show " + service + " --property=SubState --value)\" = running",
		"test \"$(systemctl show " + service + " --property=MainPID --value)\" -gt 0",
		"test \"$(systemctl show " + service + " --property=NRestarts --value)\" = \"$service_restarts\"",
	}, " && ")
	return "service_restarts=$(systemctl show " + service + " --property=NRestarts --value) && " +
		"test \"$service_restarts\" -ge 0 && " +
		"for attempt in 1 2 3; do " + checks + " || exit 1; done"
}

func ensureSSHInstallPathsAvailable(ctx context.Context, remote sshRemote, paths sshProvisionPaths) error {
	pathChecks := []string{
		"test -e " + shellQuote(paths.unitPath),
		"test -e " + shellQuote(paths.installRoot),
		"test -e " + shellQuote(paths.workspaceRoot),
		"test -e " + shellQuote(paths.gitCacheRoot),
		"test -e " + shellQuote(paths.temporaryBinaryPath),
		"test -e " + shellQuote(paths.temporaryEnvPath),
		"test -e " + shellQuote(paths.temporaryUnitPath),
	}
	script := strings.Join([]string{
		"set -eu",
		"command -v systemctl >/dev/null",
		"systemctl show-environment >/dev/null",
		"if systemctl cat " + shellQuote(paths.serviceName) + " >/dev/null 2>&1; then exit " + strconv.Itoa(sshInstallConflictExitStatus) + "; fi",
		"if " + strings.Join(pathChecks, " || ") + "; then exit " + strconv.Itoa(sshInstallConflictExitStatus) + "; fi",
	}, "\n")
	err := remote.Run(ctx, paths.prefix+"sh -c "+shellQuote(script))
	if err == nil {
		return nil
	}
	var exitError interface{ ExitStatus() int }
	if errors.As(err, &exitError) && exitError.ExitStatus() == sshInstallConflictExitStatus {
		return fmt.Errorf("%w: %v", errSSHInstallConflict, err)
	}
	return err
}

func ensureSSHInstallProtectedCgroupPaths(
	ctx context.Context,
	remote sshRemote,
	paths sshProvisionPaths,
	configuration sshTargetConfiguration,
) error {
	if !configuration.protectedCgroupEnabled() {
		return nil
	}
	script := strings.Join([]string{
		"set -eu",
		"test \"$(stat -fc %T /sys/fs/cgroup)\" = cgroup2fs",
		"test -f " + shellQuote(configuration.CgroupV2AttestationKeyPath),
		"test \"$(stat -c %u " + shellQuote(configuration.CgroupV2AttestationKeyPath) + ")\" = 0",
		"test \"$(stat -c %F " + shellQuote(configuration.CgroupV2AttestationKeyPath) + ")\" = " + shellQuote("regular file"),
		"case \"$(stat -c %a " + shellQuote(configuration.CgroupV2AttestationKeyPath) + ")\" in 000|?00|??00) ;; *) exit 1 ;; esac",
	}, "\n")
	return remote.Run(ctx, paths.prefix+"sh -c "+shellQuote(script))
}

type sshProvisionPaths struct {
	prefix              string
	serviceName         string
	installRoot         string
	workspaceRoot       string
	gitCacheRoot        string
	binaryPath          string
	envPath             string
	unitPath            string
	temporaryBinaryPath string
	temporaryEnvPath    string
	temporaryUnitPath   string
}

func (p *SSHProvisioner) connect(ctx context.Context, configuration sshTargetConfiguration) (sshRemote, error) {
	remote, err := p.dialer.Dial(ctx, sshDialInput{
		Address: net.JoinHostPort(configuration.Host, strconv.Itoa(configuration.Port)), User: configuration.User,
		PrivateKey: []byte(configuration.PrivateKey), PrivateKeyPassphrase: []byte(configuration.PrivateKeyPassphrase),
		HostKey: []byte(configuration.HostKey), Timeout: p.timeout(),
	})
	if err != nil {
		return nil, problem.Wrap(502, "ssh_connection_failed", "The SSH execution target could not be reached.", err)
	}
	return remote, nil
}

func (p *SSHProvisioner) load(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
) (persistence.ExecutionTarget, sshTargetConfiguration, error) {
	target, err := p.loadTargetMetadata(ctx, principal, tenantID, targetID)
	if err != nil {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, err
	}
	configuration, err := decryptSSHConfiguration(p.targets, target.ConfigurationEncrypted)
	if err != nil {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, err
	}
	return target, configuration, nil
}

func (p *SSHProvisioner) loadTargetMetadata(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
) (persistence.ExecutionTarget, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return persistence.ExecutionTarget{}, err
	}
	if _, err := p.targets.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return persistence.ExecutionTarget{}, err
	}
	target, err := p.targets.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return persistence.ExecutionTarget{}, err
	}
	if target.Kind != "ssh" {
		return persistence.ExecutionTarget{}, problem.New(409, "execution_target_kind_mismatch", "SSH provisioning requires an SSH execution target.")
	}
	if target.TenantID == nil || *target.TenantID != tenantID {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	return target, nil
}

func decryptSSHConfiguration(service *Service, encrypted []byte) (sshTargetConfiguration, error) {
	if len(encrypted) == 0 || service.cipher == nil {
		return sshTargetConfiguration{}, problem.New(409, "ssh_configuration_missing", "SSH execution target configuration is missing.")
	}
	decoded, err := service.cipher.Decrypt(encrypted)
	if err != nil {
		return sshTargetConfiguration{}, problem.Wrap(503, "ssh_configuration_unavailable", "SSH execution target configuration could not be decrypted.", err)
	}
	var configuration sshTargetConfiguration
	decoder := json.NewDecoder(strings.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return sshTargetConfiguration{}, problem.New(400, "invalid_ssh_configuration", "SSH execution target configuration is invalid.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return sshTargetConfiguration{}, problem.New(400, "invalid_ssh_configuration", "SSH execution target configuration is invalid.")
	}
	return configuration, nil
}

var (
	remotePathPattern  = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	serviceUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)
)

func (p *SSHProvisioner) normalize(
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
) (sshTargetConfiguration, sshProvisionPaths, error) {
	configuration.Host = strings.TrimSpace(configuration.Host)
	configuration.User = strings.TrimSpace(configuration.User)
	configuration.PrivateKey = strings.TrimSpace(configuration.PrivateKey)
	configuration.HostKey = strings.TrimSpace(configuration.HostKey)
	if configuration.Port == 0 {
		configuration.Port = 22
	}
	if configuration.ServiceUser = strings.TrimSpace(configuration.ServiceUser); configuration.ServiceUser == "" {
		configuration.ServiceUser = configuration.User
	}
	if configuration.InstallRoot = strings.TrimSpace(configuration.InstallRoot); configuration.InstallRoot == "" {
		configuration.InstallRoot = "/opt/synara/targets/" + target.ID.String()
	}
	if configuration.WorkspaceRoot = strings.TrimSpace(configuration.WorkspaceRoot); configuration.WorkspaceRoot == "" {
		configuration.WorkspaceRoot = "/var/lib/synara/targets/" + target.ID.String() + "/workspaces"
	}
	if configuration.GitCacheRoot = strings.TrimSpace(configuration.GitCacheRoot); configuration.GitCacheRoot == "" {
		configuration.GitCacheRoot = "/var/lib/synara/targets/" + target.ID.String() + "/git-cache"
	}
	configuration.AgentdVersion = strings.TrimSpace(configuration.AgentdVersion)
	configuration.AgentdBuildGitSHA = strings.TrimSpace(configuration.AgentdBuildGitSHA)
	configuration.AgentdImageDigest = strings.TrimSpace(configuration.AgentdImageDigest)
	configuration.CgroupV2Root = strings.TrimSpace(configuration.CgroupV2Root)
	configuration.CgroupV2AttestationKeyID = strings.TrimSpace(configuration.CgroupV2AttestationKeyID)
	configuration.CgroupV2AttestationKeyPath = strings.TrimSpace(configuration.CgroupV2AttestationKeyPath)
	configuration.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(configuration.ControlPlaneURL), "/")
	if configuration.ControlPlaneURL == "" {
		configuration.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(p.config.PublicControlPlaneURL), "/")
	}
	if configuration.Host == "" || len(configuration.Host) > 253 || strings.ContainsAny(configuration.Host, "\r\n\t\x00") ||
		configuration.User == "" || configuration.PrivateKey == "" || configuration.HostKey == "" {
		return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH host, user, privateKey, and hostKey are required.")
	}
	if configuration.Port < 1 || configuration.Port > 65535 || !serviceUserPattern.MatchString(configuration.User) ||
		!serviceUserPattern.MatchString(configuration.ServiceUser) {
		return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH port or user is invalid.")
	}
	if len(configuration.RunnerCommand) == 0 {
		return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH runnerCommand is required.")
	}
	for _, value := range configuration.RunnerCommand {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH runnerCommand is invalid.")
		}
	}
	parsedURL, err := url.Parse(configuration.ControlPlaneURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" ||
		(parsedURL.Scheme != "https" && !(parsedURL.Scheme == "http" && configuration.AllowInsecureControlPlane)) {
		return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH controlPlaneUrl must use HTTPS unless allowInsecureControlPlane is explicitly enabled.")
	}
	for _, value := range []string{configuration.InstallRoot, configuration.WorkspaceRoot, configuration.GitCacheRoot} {
		if !remotePathPattern.MatchString(value) || strings.Contains(value, "//") || strings.Contains(value, "..") {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH installRoot, workspaceRoot, and gitCacheRoot must be safe absolute paths.")
		}
	}
	if configuration.WorkspaceRoot == configuration.GitCacheRoot || strings.HasPrefix(configuration.WorkspaceRoot, configuration.GitCacheRoot+"/") ||
		strings.HasPrefix(configuration.GitCacheRoot, configuration.WorkspaceRoot+"/") {
		return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH workspaceRoot and gitCacheRoot must be separate.")
	}
	serviceName := "synara-agentd-" + target.ID.String() + ".service"
	if configuration.protectedCgroupEnabled() {
		expectedCgroupV2Root := "/sys/fs/cgroup/system.slice/" + serviceName
		if configuration.CgroupV2Root == "" {
			configuration.CgroupV2Root = expectedCgroupV2Root
		}
		if configuration.CgroupV2ProviderUID == nil || configuration.CgroupV2ProviderGID == nil ||
			configuration.CgroupV2ProviderPidsMax == nil || configuration.CgroupV2ProviderMemoryMax == nil ||
			configuration.CgroupV2ProviderCPUQuota == nil || configuration.CgroupV2ProviderCPUPeriod == nil ||
			configuration.CgroupV2AttestationKeyID == "" || configuration.CgroupV2AttestationKeyPath == "" ||
			configuration.AgentdVersion == "" || configuration.AgentdBuildGitSHA == "" || configuration.AgentdImageDigest == "" {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup supervision requires cgroup root, provider uid/gid, finite pids/memory/cpu limits, attestation key, and explicit agentd build identity.",
			)
		}
		if configuration.ServiceUser != "root" {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup supervision requires serviceUser=root.",
			)
		}
		if !remotePathPattern.MatchString(configuration.CgroupV2Root) ||
			strings.Contains(configuration.CgroupV2Root, "//") || strings.Contains(configuration.CgroupV2Root, "..") ||
			!remotePathPattern.MatchString(configuration.CgroupV2AttestationKeyPath) ||
			strings.Contains(configuration.CgroupV2AttestationKeyPath, "//") ||
			strings.Contains(configuration.CgroupV2AttestationKeyPath, "..") {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup root and attestation key path must be safe absolute paths.",
			)
		}
		if configuration.CgroupV2Root != expectedCgroupV2Root {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup root must be the managed systemd service ControlGroup.",
			)
		}
		if *configuration.CgroupV2ProviderUID <= 0 || *configuration.CgroupV2ProviderUID > math.MaxUint32 ||
			*configuration.CgroupV2ProviderGID < 0 || *configuration.CgroupV2ProviderGID > math.MaxUint32 {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup provider uid/gid are invalid.",
			)
		}
		if *configuration.CgroupV2ProviderPidsMax <= 0 || *configuration.CgroupV2ProviderMemoryMax <= 0 ||
			*configuration.CgroupV2ProviderCPUQuota <= 0 || *configuration.CgroupV2ProviderCPUPeriod <= 0 ||
			cgroupv2limits.Validate(cgroupv2limits.Limits{
				PidsMax:         uint64(*configuration.CgroupV2ProviderPidsMax),
				MemoryMaxBytes:  uint64(*configuration.CgroupV2ProviderMemoryMax),
				CPUQuotaMicros:  uint64(*configuration.CgroupV2ProviderCPUQuota),
				CPUPeriodMicros: uint64(*configuration.CgroupV2ProviderCPUPeriod),
			}) != nil {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup Provider resource limits are invalid.",
			)
		}
		if !validSSHProvisionBuildGitSHA(configuration.AgentdBuildGitSHA) || !validSSHProvisionImageDigest(configuration.AgentdImageDigest) {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(
				400,
				"invalid_ssh_configuration",
				"SSH protected cgroup build identity is invalid.",
			)
		}
	}
	useSudo := true
	if configuration.UseSudo != nil {
		useSudo = *configuration.UseSudo
	}
	prefix := ""
	if useSudo {
		prefix = "sudo -n "
	}
	temporaryPrefix := "/tmp/synara-agentd-" + target.ID.String()
	paths := sshProvisionPaths{
		prefix: prefix, serviceName: serviceName, installRoot: configuration.InstallRoot,
		workspaceRoot:       configuration.WorkspaceRoot,
		gitCacheRoot:        configuration.GitCacheRoot,
		binaryPath:          configuration.InstallRoot + "/synara-agentd",
		envPath:             configuration.InstallRoot + "/agentd.env",
		unitPath:            "/etc/systemd/system/" + serviceName,
		temporaryBinaryPath: temporaryPrefix,
		temporaryEnvPath:    temporaryPrefix + ".env",
		temporaryUnitPath:   temporaryPrefix + ".service",
	}
	return configuration, paths, nil
}

func (p *SSHProvisioner) environmentFile(
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
	paths sshProvisionPaths,
	workerInstanceUID string,
	bootstrapGeneration int64,
) ([]byte, error) {
	parsedWorkerInstanceUID, err := uuid.Parse(strings.TrimSpace(workerInstanceUID))
	if err != nil || parsedWorkerInstanceUID == uuid.Nil {
		return nil, problem.New(500, "ssh_worker_instance_uid_invalid", "SSH Worker instance identity could not be generated.")
	}
	if bootstrapGeneration <= 0 {
		return nil, problem.New(500, "ssh_bootstrap_generation_invalid", "SSH Worker bootstrap generation could not be generated.")
	}
	runnerCommand, err := json.Marshal(configuration.RunnerCommand)
	if err != nil {
		return nil, problem.New(400, "invalid_ssh_configuration", "SSH runnerCommand is invalid.")
	}
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return nil, problem.New(400, "invalid_execution_target_capabilities", "Execution target capabilities are invalid.")
	}
	values := [][2]string{
		{"SYNARA_CONTROL_PLANE_URL", configuration.ControlPlaneURL},
		{"SYNARA_WORKER_REGISTRATION_TOKEN", p.config.RegistrationToken},
		{"SYNARA_EXECUTION_TARGET_ID", target.ID.String()},
		{"SYNARA_EXECUTION_TARGET_KIND", "ssh"},
		{"SYNARA_AGENTD_CLUSTER_ID", "ssh"},
		{"SYNARA_AGENTD_NAMESPACE", "default"},
		{"SYNARA_AGENTD_INSTANCE_ID", "ssh-" + target.ID.String()},
		{"SYNARA_AGENTD_INSTANCE_UID", parsedWorkerInstanceUID.String()},
		{"SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION", strconv.FormatInt(bootstrapGeneration, 10)},
		{"SYNARA_AGENTD_VERSION", envDefaultString(configuration.AgentdVersion, "managed")},
		{"SYNARA_AGENTD_CAPABILITIES_JSON", string(capabilities)},
		{"SYNARA_AGENTD_RUNNER_COMMAND_JSON", string(runnerCommand)},
		{"SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "v2"},
		{"SYNARA_AGENTD_LEASE_RENEW_INTERVAL", workertiming.LeaseRenewInterval(p.config.WorkerLeaseTTL).String()},
		{"SYNARA_AGENTD_DRAIN_TIMEOUT", "20s"},
		{"SYNARA_AGENTD_WORKSPACE_ROOT", paths.workspaceRoot},
		{"SYNARA_AGENTD_GIT_CACHE_ROOT", paths.gitCacheRoot},
	}
	if configuration.AgentdBuildGitSHA != "" {
		values = append(values, [2]string{"SYNARA_AGENTD_BUILD_GIT_SHA", configuration.AgentdBuildGitSHA})
	}
	if configuration.AgentdImageDigest != "" {
		values = append(values, [2]string{"SYNARA_AGENTD_IMAGE_DIGEST", configuration.AgentdImageDigest})
	}
	if configuration.protectedCgroupEnabled() {
		values = append(values,
			[2]string{"SYNARA_AGENTD_CGROUP_V2_ROOT", configuration.CgroupV2Root},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID", strconv.Itoa(*configuration.CgroupV2ProviderUID)},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID", strconv.Itoa(*configuration.CgroupV2ProviderGID)},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_PROVIDER_PIDS_MAX", strconv.FormatInt(*configuration.CgroupV2ProviderPidsMax, 10)},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_PROVIDER_MEMORY_MAX_BYTES", strconv.FormatInt(*configuration.CgroupV2ProviderMemoryMax, 10)},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_QUOTA_MICROS", strconv.FormatInt(*configuration.CgroupV2ProviderCPUQuota, 10)},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_PERIOD_MICROS", strconv.FormatInt(*configuration.CgroupV2ProviderCPUPeriod, 10)},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID", configuration.CgroupV2AttestationKeyID},
			[2]string{"SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE", configuration.CgroupV2AttestationKeyPath},
		)
	}
	var output strings.Builder
	for _, item := range values {
		output.WriteString(item[0])
		output.WriteByte('=')
		output.WriteString(strconv.Quote(item[1]))
		output.WriteByte('\n')
	}
	return []byte(output.String()), nil
}

func systemdUnitWithDelegate(paths sshProvisionPaths, serviceUser string, delegate bool) string {
	lines := []string{
		"[Unit]",
		"Description=Synara agentd for " + paths.serviceName,
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"User=" + serviceUser,
		"EnvironmentFile=" + paths.envPath,
		"WorkingDirectory=" + paths.workspaceRoot,
		"ExecStart=" + paths.binaryPath,
		"Restart=always",
		"RestartSec=2",
		"KillSignal=SIGTERM",
		"TimeoutStopSec=30",
		"NoNewPrivileges=true",
	}
	if delegate {
		lines = append(lines, "Delegate=yes", "DelegateSubgroup="+protectedCgroupSupervisorSubgroupName)
	}
	lines = append(lines,
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	)
	return strings.Join(lines, "\n")
}

func (p *SSHProvisioner) beginSSHOperation(
	ctx context.Context,
	target persistence.ExecutionTarget,
	actorID uuid.UUID,
	operation string,
	expectedInstanceUID *uuid.UUID,
	requestID, ipAddress string,
) (sshTargetOperationFence, error) {
	if (operation == "install" || operation == "upgrade") &&
		(expectedInstanceUID == nil || *expectedInstanceUID == uuid.Nil) {
		return sshTargetOperationFence{}, problem.New(500, "ssh_bootstrap_authority_missing", "SSH Worker bootstrap authority is missing its expected instance UID.")
	}
	if operation == "revoke" {
		expectedInstanceUID = nil
	}
	fence := sshTargetOperationFence{}
	err := persistence.InTransaction(ctx, p.targets.db, func(tx *gorm.DB) error {
		started, _, err := p.beginSSHOperationLocked(
			ctx, tx, target, actorID, operation, expectedInstanceUID, requestID, ipAddress,
		)
		fence = started
		return err
	})
	return fence, err
}

func (p *SSHProvisioner) beginSSHRevokeOperation(
	ctx context.Context,
	target persistence.ExecutionTarget,
	principal identity.Principal,
	requestID, ipAddress string,
) (sshTargetOperationFence, error) {
	fence := sshTargetOperationFence{}
	var postCommit func()
	err := persistence.InTransaction(ctx, p.targets.db, func(tx *gorm.DB) error {
		started, current, err := p.beginSSHOperationLocked(
			ctx, tx, target, principal.UserID, "revoke", nil, requestID, ipAddress,
		)
		if err != nil {
			return err
		}
		fence = started
		reason := fmt.Sprintf("SSH Target revoke generation %d", fence.Generation)
		callback, err := p.revokeWorkers(
			ctx, tx, principal, current, fence.Generation, reason, requestID, ipAddress,
		)
		postCommit = callback
		return err
	})
	if err != nil {
		return sshTargetOperationFence{}, err
	}
	if postCommit != nil {
		postCommit()
	}
	return fence, nil
}

func (p *SSHProvisioner) beginSSHOperationLocked(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	actorID uuid.UUID,
	operation string,
	expectedInstanceUID *uuid.UUID,
	requestID, ipAddress string,
) (sshTargetOperationFence, persistence.ExecutionTarget, error) {
	var current persistence.ExecutionTarget
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND kind = ? AND tenant_id = ?", target.ID, "ssh", *target.TenantID).
		Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return sshTargetOperationFence{}, persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "SSH execution target not found.")
	} else if err != nil {
		return sshTargetOperationFence{}, persistence.ExecutionTarget{}, problem.Wrap(500, "ssh_operation_target_lookup_failed", "SSH Target operation could not be started.", err)
	}
	if operation == "revoke" && current.Status == "offline" && current.SSHOperationKind != nil &&
		*current.SSHOperationKind == "revoke" && current.SSHOperationStartedAt != nil &&
		current.SSHExpectedInstanceUID == nil {
		return sshTargetOperationFence{Generation: current.SSHOperationGeneration, Kind: operation}, current, nil
	}
	now := p.now()
	if current.SSHOperationKind != nil && current.SSHOperationStartedAt != nil &&
		current.SSHOperationStartedAt.Add(p.timeout()+30*time.Second).After(now) {
		return sshTargetOperationFence{}, persistence.ExecutionTarget{}, problem.New(409, "ssh_operation_in_progress", "Another SSH Target operation is already in progress.")
	}
	fence := sshTargetOperationFence{Generation: current.SSHOperationGeneration + 1, Kind: operation}
	result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
		Where("id = ? AND kind = ? AND ssh_operation_generation = ?", target.ID, "ssh", current.SSHOperationGeneration).
		Updates(map[string]any{
			"status": "offline", "ssh_operation_generation": fence.Generation,
			"ssh_operation_kind": operation, "ssh_operation_started_at": now,
			"ssh_expected_instance_uid": expectedInstanceUID, "updated_at": now,
		})
	if result.Error != nil || result.RowsAffected != 1 {
		return sshTargetOperationFence{}, persistence.ExecutionTarget{}, problem.Wrap(409, "ssh_operation_start_conflict", "SSH Target operation changed concurrently.", result.Error)
	}
	current.Status = "offline"
	current.SSHOperationGeneration = fence.Generation
	current.SSHOperationKind = &operation
	current.SSHOperationStartedAt = &now
	current.SSHExpectedInstanceUID = expectedInstanceUID
	current.UpdatedAt = now
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: *target.TenantID, ActorType: "user", ActorID: &actorID,
		Action:       "execution_target.ssh_" + operation + "_started",
		ResourceType: "execution_target", ResourceID: &target.ID,
		OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{
			"kind": "ssh", "operation": operation, "status": "offline", "operationGeneration": fence.Generation,
		},
	}); err != nil {
		return sshTargetOperationFence{}, persistence.ExecutionTarget{}, err
	}
	return fence, current, nil
}

func (p *SSHProvisioner) finishSSHOperation(
	ctx context.Context,
	target persistence.ExecutionTarget,
	actorID uuid.UUID,
	fence sshTargetOperationFence,
	phase string,
	status string,
	requestID string,
	ipAddress string,
) error {
	return persistence.InTransaction(ctx, p.targets.db, func(tx *gorm.DB) error {
		now := p.now()
		result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where(
				"id = ? AND kind = ? AND ssh_operation_generation = ? AND ssh_operation_kind = ?",
				target.ID, "ssh", fence.Generation, fence.Kind,
			).
			Updates(map[string]any{
				"status": status, "ssh_operation_kind": nil, "ssh_operation_started_at": nil,
				"ssh_expected_instance_uid": nil, "updated_at": now,
			})
		if result.Error != nil {
			return problem.Wrap(500, "ssh_operation_finish_failed", "SSH Target operation status could not be persisted.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "ssh_operation_superseded", "SSH Target operation was superseded and cannot change Target status.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *target.TenantID, ActorType: "user", ActorID: &actorID,
			Action:       "execution_target.ssh_" + fence.Kind + "_" + phase,
			ResourceType: "execution_target", ResourceID: &target.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"kind": "ssh", "operation": fence.Kind, "status": status, "operationGeneration": fence.Generation,
			},
		})
	})
}

func (p *SSHProvisioner) failSSHOperation(
	ctx context.Context,
	target persistence.ExecutionTarget,
	actorID uuid.UUID,
	fence sshTargetOperationFence,
	requestID string,
	ipAddress string,
) error {
	failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.finishSSHOperation(failureContext, target, actorID, fence, "failed", "offline", requestID, ipAddress)
}

func (p *SSHProvisioner) waitForSSHWorkerReady(
	ctx context.Context,
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
	instanceUID string,
) error {
	pollInterval := p.readinessPoll
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	lastState := "exact Worker instance has not registered"
	for {
		ready, state, err := p.checkWorkerReady(ctx, p.targets.db, target, configuration, instanceUID)
		if err != nil {
			return err
		}
		lastState = state
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for exact SSH Worker instance readiness: %w (%s)", ctx.Err(), lastState)
		case <-ticker.C:
		}
	}
}

func (p *SSHProvisioner) sshWorkerReady(
	ctx context.Context,
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
	instanceUID string,
) (bool, string, error) {
	return p.sshWorkerReadyWithDB(ctx, p.targets.db, target, configuration, instanceUID)
}

func (p *SSHProvisioner) sshWorkerReadyWithDB(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
	instanceUID string,
) (bool, string, error) {
	var worker persistence.WorkerInstance
	err := db.WithContext(ctx).
		Where(
			"execution_target_id = ? AND target_kind = ? AND instance_uid = ? AND administrative_status = ?",
			target.ID,
			"ssh",
			instanceUID,
			"active",
		).
		Order("registered_at DESC").
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, "exact Worker instance has not registered", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("load exact SSH Worker instance readiness: %w", err)
	}
	if worker.Status != "online" {
		return false, "exact Worker instance is not online", nil
	}
	if worker.ProtocolVersion != 2 || !worker.LeaseSupported || !worker.FencingSupported {
		return false, "", errors.New("exact SSH Worker instance did not register the required remote Worker protocol")
	}
	if worker.CompatibilityStatus != "compatible" {
		if worker.CompatibilityStatus == "unknown" || strings.TrimSpace(worker.CompatibilityStatus) == "" {
			return false, "exact Worker compatibility is not resolved", nil
		}
		return false, "", fmt.Errorf("exact SSH Worker instance is not compatible: %s", worker.CompatibilityStatus)
	}
	if worker.CurrentManifestID == nil {
		return false, "exact Worker has not persisted its current Manifest", nil
	}
	now := p.now()
	if worker.LastHeartbeatAt.IsZero() || !worker.LastHeartbeatAt.After(worker.RegisteredAt) {
		return false, "exact Worker has not completed a post-registration heartbeat", nil
	}
	if worker.LastHeartbeatAt.After(now) {
		return false, "", errors.New("exact Worker heartbeat timestamp is in the future")
	}
	if now.Sub(worker.LastHeartbeatAt) > p.workerHeartbeatTimeout() {
		return false, "exact Worker heartbeat is stale", nil
	}
	var manifest persistence.WorkerManifest
	if err := db.WithContext(ctx).Where("id = ?", *worker.CurrentManifestID).Take(&manifest).Error; err != nil {
		return false, "", fmt.Errorf("load exact SSH Worker Manifest readiness: %w", err)
	}
	if err := validateSSHWorkerManifestReadiness(target, configuration, manifest); err != nil {
		return false, "", err
	}
	return true, "exact Worker runtime is ready", nil
}

func (p *SSHProvisioner) activateReadySSHWorker(
	ctx context.Context,
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
	instanceUID string,
	fence sshTargetOperationFence,
	actorID uuid.UUID,
	requestID string,
	ipAddress string,
) error {
	return persistence.InTransaction(ctx, p.targets.db, func(tx *gorm.DB) error {
		var currentTarget persistence.ExecutionTarget
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"id = ? AND kind = ? AND status = ? AND ssh_operation_generation = ? AND ssh_operation_kind = ? AND ssh_expected_instance_uid = ?",
				target.ID, "ssh", "offline", fence.Generation, fence.Kind, instanceUID,
			).
			Take(&currentTarget).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "ssh_worker_activation_conflict", "SSH Target status changed before Worker readiness could be committed.")
		} else if err != nil {
			return problem.Wrap(500, "ssh_worker_activation_lookup_failed", "SSH Target readiness could not be committed.", err)
		}
		if err := p.lockWorkerReady(ctx, tx, currentTarget.ID, instanceUID); err != nil {
			return err
		}
		ready, state, err := p.checkWorkerReady(ctx, tx, currentTarget, configuration, instanceUID)
		if err != nil {
			return problem.Wrap(409, "ssh_worker_readiness_changed", "SSH Worker readiness changed before Target activation.", err)
		}
		if !ready {
			return problem.New(409, "ssh_worker_readiness_changed", "SSH Worker readiness changed before Target activation: "+state+".")
		}
		result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where(
				"id = ? AND kind = ? AND status = ? AND ssh_operation_generation = ? AND ssh_operation_kind = ? AND ssh_expected_instance_uid = ?",
				target.ID, "ssh", "offline", fence.Generation, fence.Kind, instanceUID,
			).
			Updates(map[string]any{
				"status": "active", "ssh_operation_kind": nil, "ssh_operation_started_at": nil,
				"ssh_expected_instance_uid": nil, "updated_at": p.now(),
			})
		if result.Error != nil || result.RowsAffected != 1 {
			return problem.Wrap(409, "ssh_worker_activation_conflict", "SSH Target status changed before Worker readiness could be committed.", result.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *target.TenantID, ActorType: "user", ActorID: &actorID,
			Action:       "execution_target.ssh_" + fence.Kind + "_completed",
			ResourceType: "execution_target", ResourceID: &target.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"kind": "ssh", "operation": fence.Kind, "status": "active", "operationGeneration": fence.Generation,
			},
		})
	})
}

func lockExactSSHWorkerReadiness(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	instanceUID string,
) error {
	var worker persistence.WorkerInstance
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where(
			"execution_target_id = ? AND target_kind = ? AND instance_uid = ? AND administrative_status = ?",
			targetID,
			"ssh",
			instanceUID,
			"active",
		).
		Order("registered_at DESC").
		Take(&worker).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(409, "ssh_worker_readiness_changed", "Exact SSH Worker registration disappeared before Target activation.")
	} else if err != nil {
		return problem.Wrap(500, "ssh_worker_readiness_lock_failed", "Exact SSH Worker readiness could not be locked.", err)
	}
	return nil
}

func validateSSHWorkerManifestReadiness(
	target persistence.ExecutionTarget,
	configuration sshTargetConfiguration,
	manifest persistence.WorkerManifest,
) error {
	expectedVersion := envDefaultString(configuration.AgentdVersion, "managed")
	if manifest.WorkerBuildVersion != expectedVersion {
		return errors.New("exact SSH Worker Manifest build version does not match provisioning configuration")
	}
	if optionalSSHWorkerManifestValue(manifest.WorkerBuildGitSHA) != configuration.AgentdBuildGitSHA {
		return errors.New("exact SSH Worker Manifest git SHA does not match provisioning configuration")
	}
	if optionalSSHWorkerManifestValue(manifest.ImageDigest) != configuration.AgentdImageDigest {
		return errors.New("exact SSH Worker Manifest image digest does not match provisioning configuration")
	}
	if manifest.WorkerProtocolMinimum > 2 || manifest.WorkerProtocolMaximum < 2 {
		return errors.New("exact SSH Worker Manifest does not support Worker Protocol v2")
	}
	if !configuration.protectedCgroupEnabled() {
		return nil
	}
	if !sshWorkerManifestHasStrictCgroupV2Containment(manifest) ||
		manifest.ProcessContainmentTrustMode != ProcessContainmentTrustSignedV1 ||
		manifest.ProcessContainmentAttestationKeyID == nil ||
		manifest.ProcessContainmentAttestationKeySHA256 == nil {
		return errors.New("exact SSH Worker Manifest protected-cgroup trustState is not verified")
	}
	policy, err := ParseProcessContainmentPolicy(target.Capabilities)
	if err != nil {
		return fmt.Errorf("load exact SSH Worker process-containment policy: %w", err)
	}
	if policy.TrustMode != ProcessContainmentTrustSignedV1 ||
		policy.KeyID != *manifest.ProcessContainmentAttestationKeyID ||
		policy.PublicKeySHA256 != *manifest.ProcessContainmentAttestationKeySHA256 {
		return errors.New("exact SSH Worker Manifest protected-cgroup trustState is not verified")
	}
	return nil
}

func sshWorkerManifestHasStrictCgroupV2Containment(manifest persistence.WorkerManifest) bool {
	return WorkerManifestHasSupportedStrictCgroupV2Containment(manifest)
}

func optionalSSHWorkerManifestValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func (p *SSHProvisioner) workerHeartbeatTimeout() time.Duration {
	if p.config.WorkerHeartbeatTimeout > 0 {
		return p.config.WorkerHeartbeatTimeout
	}
	return 45 * time.Second
}

func (p *SSHProvisioner) timeout() time.Duration {
	if p.config.Timeout <= 0 {
		return 2 * time.Minute
	}
	return p.config.Timeout
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func cleanupSSHTemporaryFiles(remote sshRemote, paths sshProvisionPaths) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = remote.Run(ctx, "rm -f "+shellQuote(paths.temporaryBinaryPath)+" "+shellQuote(paths.temporaryEnvPath)+" "+shellQuote(paths.temporaryUnitPath))
}

func sshInstallDirectoryCommands(paths sshProvisionPaths, configuration sshTargetConfiguration) []string {
	if !configuration.protectedCgroupEnabled() {
		return []string{
			"install -d -m 0755 " + shellQuote(paths.installRoot) + " " + shellQuote(paths.workspaceRoot) + " " + shellQuote(paths.gitCacheRoot),
			"chown " + shellQuote(configuration.ServiceUser+":") + " " + shellQuote(paths.workspaceRoot) + " " + shellQuote(paths.gitCacheRoot),
		}
	}
	return []string{
		"install -d -m 0755 " + shellQuote(paths.installRoot),
		"install -d -m 0711 " + shellQuote(paths.workspaceRoot),
		"install -d -m 0700 " + shellQuote(paths.gitCacheRoot),
		"chown " + shellQuote(configuration.ServiceUser+":") + " " + shellQuote(paths.workspaceRoot) + " " + shellQuote(paths.gitCacheRoot),
	}
}

func envDefaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (c sshTargetConfiguration) protectedCgroupEnabled() bool {
	return c.CgroupV2Root != "" ||
		c.CgroupV2ProviderUID != nil ||
		c.CgroupV2ProviderGID != nil ||
		c.CgroupV2ProviderPidsMax != nil ||
		c.CgroupV2ProviderMemoryMax != nil ||
		c.CgroupV2ProviderCPUQuota != nil ||
		c.CgroupV2ProviderCPUPeriod != nil ||
		c.CgroupV2AttestationKeyID != "" ||
		c.CgroupV2AttestationKeyPath != ""
}

func validSSHProvisionBuildGitSHA(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validSSHProvisionImageDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type realSSHDialer struct{}

func (realSSHDialer) Dial(ctx context.Context, input sshDialInput) (sshRemote, error) {
	var signer ssh.Signer
	var err error
	if len(input.PrivateKeyPassphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(input.PrivateKey, input.PrivateKeyPassphrase)
	} else {
		signer, err = ssh.ParsePrivateKey(input.PrivateKey)
	}
	if err != nil {
		return nil, errors.New("SSH private key is invalid")
	}
	expectedHostKey, _, _, _, err := ssh.ParseAuthorizedKey(input.HostKey)
	if err != nil {
		return nil, errors.New("SSH host key is invalid")
	}
	clientConfig, err := newSSHClientConfig(input.User, signer, expectedHostKey)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: input.Timeout}
	connection, err := dialer.DialContext(ctx, "tcp", input.Address)
	if err != nil {
		return nil, err
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, input.Address, clientConfig)
	if err != nil {
		connection.Close()
		return nil, err
	}
	return &realSSHRemote{client: ssh.NewClient(clientConnection, channels, requests)}, nil
}

func newSSHClientConfig(user string, signer ssh.Signer, expectedHostKey ssh.PublicKey) (*ssh.ClientConfig, error) {
	hostKeyAlgorithms, err := supportedSSHHostKeyAlgorithms(expectedHostKey)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: hostKeyAlgorithms,
		HostKeyCallback:   pinnedSSHHostKeyCallback(expectedHostKey),
	}, nil
}

func supportedSSHHostKeyAlgorithms(expectedHostKey ssh.PublicKey) ([]string, error) {
	candidates := []string{expectedHostKey.Type()}
	switch expectedHostKey.Type() {
	case ssh.KeyAlgoRSA:
		candidates = []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}
	case ssh.CertAlgoRSAv01:
		candidates = []string{ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01}
	}
	supported := ssh.SupportedAlgorithms().HostKeys
	algorithms := make([]string, 0, len(candidates))
	for _, supportedAlgorithm := range supported {
		for _, candidate := range candidates {
			if supportedAlgorithm == candidate {
				algorithms = append(algorithms, supportedAlgorithm)
				break
			}
		}
	}
	if len(algorithms) == 0 {
		return nil, fmt.Errorf("SSH host key algorithm %q is unsupported", expectedHostKey.Type())
	}
	return algorithms, nil
}

func pinnedSSHHostKeyCallback(expectedHostKey ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if key.Type() != expectedHostKey.Type() || !bytes.Equal(key.Marshal(), expectedHostKey.Marshal()) {
			return errors.New("SSH host key mismatch")
		}
		return nil
	}
}

type realSSHRemote struct {
	client *ssh.Client
}

func (r *realSSHRemote) Upload(ctx context.Context, path string, mode os.FileMode, source io.Reader) error {
	session, err := r.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdin = source
	command := "umask 077 && cat > " + shellQuote(path) + " && chmod " + fmt.Sprintf("%04o", mode.Perm()) + " " + shellQuote(path)
	result := make(chan error, 1)
	go func() { result <- session.Run(command) }()
	select {
	case <-ctx.Done():
		_ = session.Close()
		return ctx.Err()
	case err := <-result:
		if err != nil {
			return errors.New("SSH upload command failed")
		}
		return nil
	}
}

func (r *realSSHRemote) Run(ctx context.Context, command string) error {
	session, err := r.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	result := make(chan error, 1)
	go func() { result <- session.Run(command) }()
	select {
	case <-ctx.Done():
		_ = session.Close()
		return ctx.Err()
	case err := <-result:
		if err != nil {
			var exitError *ssh.ExitError
			if errors.As(err, &exitError) {
				return &sshRemoteCommandExitError{status: exitError.ExitStatus()}
			}
			return errors.New("SSH remote command failed")
		}
		return nil
	}
}

func (r *realSSHRemote) Close() error { return r.client.Close() }

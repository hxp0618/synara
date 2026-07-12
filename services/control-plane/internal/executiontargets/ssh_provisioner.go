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
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type SSHProvisioningConfig struct {
	AgentdBinaryPath      string
	RegistrationToken     string
	PublicControlPlaneURL string
	Timeout               time.Duration
}

type SSHProvisionResult struct {
	TargetID     uuid.UUID `json:"targetId"`
	Operation    string    `json:"operation"`
	Status       string    `json:"status"`
	ServiceName  string    `json:"serviceName"`
	BinarySHA256 string    `json:"binarySha256,omitempty"`
}

type sshTargetConfiguration struct {
	Host                      string   `json:"host"`
	Port                      int      `json:"port"`
	User                      string   `json:"user"`
	PrivateKey                string   `json:"privateKey"`
	PrivateKeyPassphrase      string   `json:"privateKeyPassphrase"`
	HostKey                   string   `json:"hostKey"`
	ControlPlaneURL           string   `json:"controlPlaneUrl"`
	AllowInsecureControlPlane bool     `json:"allowInsecureControlPlane"`
	RunnerCommand             []string `json:"runnerCommand"`
	WorkspaceRoot             string   `json:"workspaceRoot"`
	InstallRoot               string   `json:"installRoot"`
	ServiceUser               string   `json:"serviceUser"`
	UseSudo                   *bool    `json:"useSudo"`
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

type sshDialer interface {
	Dial(context.Context, sshDialInput) (sshRemote, error)
}

type SSHProvisioner struct {
	targets *Service
	config  SSHProvisioningConfig
	dialer  sshDialer
}

func NewSSHProvisioner(targets *Service, config SSHProvisioningConfig) *SSHProvisioner {
	return &SSHProvisioner{targets: targets, config: config, dialer: realSSHDialer{}}
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
	target, configuration, err := p.load(ctx, principal, tenantID, targetID)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	configuration, paths, err := p.normalize(target, configuration)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	if err := p.recordOperation(ctx, target, principal.UserID, "revoke", "started", "offline", requestID, ipAddress); err != nil {
		return SSHProvisionResult{}, err
	}
	remote, err := p.connect(ctx, configuration)
	if err != nil {
		p.recordFailure(ctx, target, principal.UserID, "revoke", requestID, ipAddress)
		return SSHProvisionResult{}, err
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
		p.recordFailure(ctx, target, principal.UserID, "revoke", requestID, ipAddress)
		return SSHProvisionResult{}, problem.Wrap(502, "ssh_revoke_failed", "SSH Worker revocation failed.", err)
	}
	if err := p.recordOperation(ctx, target, principal.UserID, "revoke", "completed", "disabled", requestID, ipAddress); err != nil {
		return SSHProvisionResult{}, err
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
	if err := p.recordOperation(ctx, target, principal.UserID, operation, "started", "offline", requestID, ipAddress); err != nil {
		return SSHProvisionResult{}, err
	}
	remote, err := p.connect(ctx, configuration)
	if err != nil {
		p.recordFailure(ctx, target, principal.UserID, operation, requestID, ipAddress)
		return SSHProvisionResult{}, err
	}
	defer remote.Close()
	defer cleanupSSHTemporaryFiles(remote, paths)
	operationContext, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	hash := sha256.New()
	if err := remote.Upload(operationContext, paths.temporaryBinaryPath, 0o700, io.TeeReader(binary, hash)); err != nil {
		p.recordFailure(ctx, target, principal.UserID, operation, requestID, ipAddress)
		return SSHProvisionResult{}, problem.Wrap(502, "ssh_agentd_upload_failed", "synara-agentd could not be uploaded.", err)
	}
	environment, err := p.environmentFile(target, configuration, paths)
	if err != nil {
		p.recordFailure(ctx, target, principal.UserID, operation, requestID, ipAddress)
		return SSHProvisionResult{}, err
	}
	if err := remote.Upload(operationContext, paths.temporaryEnvPath, 0o600, bytes.NewReader(environment)); err != nil {
		p.recordFailure(ctx, target, principal.UserID, operation, requestID, ipAddress)
		return SSHProvisionResult{}, problem.Wrap(502, "ssh_agentd_upload_failed", "The synara-agentd environment could not be uploaded.", err)
	}
	unit := []byte(systemdUnit(paths, configuration.ServiceUser))
	if err := remote.Upload(operationContext, paths.temporaryUnitPath, 0o600, bytes.NewReader(unit)); err != nil {
		p.recordFailure(ctx, target, principal.UserID, operation, requestID, ipAddress)
		return SSHProvisionResult{}, problem.Wrap(502, "ssh_agentd_upload_failed", "The synara-agentd service unit could not be uploaded.", err)
	}
	commands := []string{
		"install -d -m 0755 " + shellQuote(paths.installRoot) + " " + shellQuote(paths.workspaceRoot),
		"chown " + shellQuote(configuration.ServiceUser+":") + " " + shellQuote(paths.workspaceRoot),
		"install -m 0755 " + shellQuote(paths.temporaryBinaryPath) + " " + shellQuote(paths.binaryPath),
		"install -m 0600 " + shellQuote(paths.temporaryEnvPath) + " " + shellQuote(paths.envPath),
		"install -m 0644 " + shellQuote(paths.temporaryUnitPath) + " " + shellQuote(paths.unitPath),
		"rm -f " + shellQuote(paths.temporaryBinaryPath) + " " + shellQuote(paths.temporaryEnvPath) + " " + shellQuote(paths.temporaryUnitPath),
		"systemctl daemon-reload",
		"systemctl enable " + shellQuote(paths.serviceName),
		"systemctl restart " + shellQuote(paths.serviceName),
	}
	command := paths.prefix + "sh -c " + shellQuote(strings.Join(commands, " && "))
	if err := remote.Run(operationContext, command); err != nil {
		p.recordFailure(ctx, target, principal.UserID, operation, requestID, ipAddress)
		return SSHProvisionResult{}, problem.Wrap(502, "ssh_provision_failed", "SSH Worker provisioning failed.", err)
	}
	if err := p.recordOperation(ctx, target, principal.UserID, operation, "completed", "active", requestID, ipAddress); err != nil {
		return SSHProvisionResult{}, err
	}
	return SSHProvisionResult{
		TargetID: target.ID, Operation: operation, Status: "active", ServiceName: paths.serviceName,
		BinarySHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

type sshProvisionPaths struct {
	prefix              string
	serviceName         string
	installRoot         string
	workspaceRoot       string
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
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, err
	}
	if _, err := p.targets.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, err
	}
	target, err := p.targets.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, err
	}
	if target.Kind != "ssh" {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, problem.New(409, "execution_target_kind_mismatch", "SSH provisioning requires an SSH execution target.")
	}
	if target.TenantID == nil || *target.TenantID != tenantID {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	configuration, err := decryptSSHConfiguration(p.targets, target.ConfigurationEncrypted)
	if err != nil {
		return persistence.ExecutionTarget{}, sshTargetConfiguration{}, err
	}
	return target, configuration, nil
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
	for _, value := range []string{configuration.InstallRoot, configuration.WorkspaceRoot} {
		if !remotePathPattern.MatchString(value) || strings.Contains(value, "//") || strings.Contains(value, "..") {
			return sshTargetConfiguration{}, sshProvisionPaths{}, problem.New(400, "invalid_ssh_configuration", "SSH installRoot and workspaceRoot must be safe absolute paths.")
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
	serviceName := "synara-agentd-" + target.ID.String() + ".service"
	temporaryPrefix := "/tmp/synara-agentd-" + target.ID.String()
	paths := sshProvisionPaths{
		prefix: prefix, serviceName: serviceName, installRoot: configuration.InstallRoot,
		workspaceRoot:       configuration.WorkspaceRoot,
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
) ([]byte, error) {
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
		{"SYNARA_AGENTD_VERSION", "managed"},
		{"SYNARA_AGENTD_CAPABILITIES_JSON", string(capabilities)},
		{"SYNARA_AGENTD_RUNNER_COMMAND_JSON", string(runnerCommand)},
		{"SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "v2"},
		{"SYNARA_AGENTD_WORKSPACE_ROOT", paths.workspaceRoot},
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

func systemdUnit(paths sshProvisionPaths, serviceUser string) string {
	return strings.Join([]string{
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
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

func (p *SSHProvisioner) recordOperation(
	ctx context.Context,
	target persistence.ExecutionTarget,
	actorID uuid.UUID,
	operation, phase, status, requestID, ipAddress string,
) error {
	return persistence.InTransaction(ctx, p.targets.db, func(tx *gorm.DB) error {
		result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where("id = ? AND kind = ?", target.ID, "ssh").Update("status", status)
		if result.Error != nil || result.RowsAffected != 1 {
			return problem.Wrap(409, "execution_target_status_update_failed", "Execution target status could not be updated.", result.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *target.TenantID, ActorType: "user", ActorID: &actorID,
			Action:       "execution_target.ssh_" + operation + "_" + phase,
			ResourceType: "execution_target", ResourceID: &target.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"kind": "ssh", "operation": operation, "status": status},
		})
	})
}

func (p *SSHProvisioner) recordFailure(
	ctx context.Context,
	target persistence.ExecutionTarget,
	actorID uuid.UUID,
	operation, requestID, ipAddress string,
) {
	failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = p.recordOperation(failureContext, target, actorID, operation, "failed", "offline", requestID, ipAddress)
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
	clientConfig := &ssh.ClientConfig{
		User: input.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if key.Type() != expectedHostKey.Type() || !bytes.Equal(key.Marshal(), expectedHostKey.Marshal()) {
				return errors.New("SSH host key mismatch")
			}
			return nil
		},
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
			return errors.New("SSH remote command failed")
		}
		return nil
	}
}

func (r *realSSHRemote) Close() error { return r.client.Close() }

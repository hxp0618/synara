package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSSHProvisionerInstallsUpgradesAndRevokesWithoutLeakingSecrets(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	remote := &fakeSSHRemote{uploads: map[string][]byte{}}
	dialer := &fakeSSHDialer{remote: remote}
	fixture.provisioner.dialer = dialer

	installed, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-install", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Status != "active" || installed.Operation != "install" || installed.BinarySHA256 == "" {
		t.Fatalf("unexpected install result: %#v", installed)
	}
	if dialer.input.Address != "ssh.example.com:2222" || dialer.input.User != "root" {
		t.Fatalf("unexpected SSH dial input: %#v", dialer.input)
	}
	if string(dialer.input.PrivateKey) != "ssh-private-key-secret" {
		t.Fatal("encrypted SSH private key was not delivered to the SSH transport")
	}
	expectedWorkspaceRoot := "/var/lib/synara/test/workspaces"
	expectedGitCacheRoot := "/var/lib/synara/targets/" + fixture.targetID.String() + "/git-cache"
	var environment []byte
	for path, payload := range remote.uploads {
		if strings.HasSuffix(path, ".env") {
			environment = payload
		}
	}
	if !bytes.Contains(environment, []byte("worker-registration-secret")) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_RUNNER_COMMAND_JSON="[\"provider-host\",\"run\",\"--jsonl\"]"`)) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL="v2"`)) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_DRAIN_TIMEOUT="20s"`)) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_WORKSPACE_ROOT="`+expectedWorkspaceRoot+`"`)) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_GIT_CACHE_ROOT="`+expectedGitCacheRoot+`"`)) {
		t.Fatalf("uploaded agentd environment is incomplete: %s", environment)
	}
	if !commandsContainAll(remote.commands, "install -d -m 0755", expectedWorkspaceRoot, expectedGitCacheRoot) {
		t.Fatalf("SSH provisioning did not create both storage roots: %#v", remote.commands)
	}
	if !commandsContainAll(remote.commands, "chown", expectedWorkspaceRoot, expectedGitCacheRoot) {
		t.Fatalf("SSH provisioning did not assign both storage roots to the service user: %#v", remote.commands)
	}
	for _, command := range remote.commands {
		if strings.Contains(command, "worker-registration-secret") || strings.Contains(command, "ssh-private-key-secret") {
			t.Fatalf("SSH secret leaked into a remote command: %s", command)
		}
	}

	upgraded, err := fixture.provisioner.Upgrade(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-upgrade", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Status != "active" || upgraded.Operation != "upgrade" {
		t.Fatalf("unexpected upgrade result: %#v", upgraded)
	}
	revoked, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != "disabled" || revoked.Operation != "revoke" {
		t.Fatalf("unexpected revoke result: %#v", revoked)
	}

	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "disabled" {
		t.Fatalf("revoked target status is %q", target.Status)
	}
	var audits []persistence.AuditLog
	if err := fixture.db.Where("resource_id = ? AND action LIKE ?", fixture.targetID, "execution_target.ssh_%").Order("occurred_at, event_id").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 6 {
		t.Fatalf("expected start/completion audit for three operations, got %d", len(audits))
	}
	encoded, err := json.Marshal(struct {
		Install SSHProvisionResult
		Upgrade SSHProvisionResult
		Revoke  SSHProvisionResult
		Audits  []persistence.AuditLog
	}{installed, upgraded, revoked, audits})
	if err != nil {
		t.Fatal(err)
	}
	for _, secretValue := range []string{"worker-registration-secret", "ssh-private-key-secret"} {
		if bytes.Contains(encoded, []byte(secretValue)) {
			t.Fatalf("SSH provisioning response/audit leaked %q: %s", secretValue, encoded)
		}
	}
}

func TestSSHProvisionerFailsClosedBeforeDialForUnsafeControlPlaneURL(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "http://control-plane.example.com")
	dialer := &fakeSSHDialer{remote: &fakeSSHRemote{uploads: map[string][]byte{}}}
	fixture.provisioner.dialer = dialer

	_, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-invalid", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "invalid_ssh_configuration")
	if dialer.calls != 0 {
		t.Fatal("unsafe SSH configuration reached the network dialer")
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("resource_id = ? AND action LIKE ?", fixture.targetID, "execution_target.ssh_%").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 {
		t.Fatalf("invalid preflight wrote %d provisioning audits", auditCount)
	}
}

func TestSSHProvisionerRejectsOverlappingWorkspaceAndGitCacheRoots(t *testing.T) {
	provisioner := &SSHProvisioner{}
	target := persistence.ExecutionTarget{ID: uuid.New()}
	for _, test := range []struct {
		name          string
		workspaceRoot string
		gitCacheRoot  string
	}{
		{name: "same root", workspaceRoot: "/var/lib/synara/shared", gitCacheRoot: "/var/lib/synara/shared"},
		{name: "cache inside workspace", workspaceRoot: "/var/lib/synara/workspaces", gitCacheRoot: "/var/lib/synara/workspaces/git-cache"},
		{name: "workspace inside cache", workspaceRoot: "/var/lib/synara/git-cache/workspaces", gitCacheRoot: "/var/lib/synara/git-cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := provisioner.normalize(target, sshTargetConfiguration{
				Host: "ssh.example.com", User: "root", PrivateKey: "private-key", HostKey: "host-key",
				ControlPlaneURL: "https://control-plane.example.com", RunnerCommand: []string{"runner"},
				WorkspaceRoot: test.workspaceRoot, GitCacheRoot: test.gitCacheRoot,
			})
			assertExecutionTargetProblemCode(t, err, "invalid_ssh_configuration")
		})
	}
}

func TestSSHProvisionerRecordsFailedConnectionAndLeavesTargetOffline(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	fixture.provisioner.dialer = &fakeSSHDialer{err: errors.New("connection refused")}

	_, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-failed", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_connection_failed")
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "offline" {
		t.Fatalf("failed SSH target status is %q", target.Status)
	}
	var actions []string
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("resource_id = ?", fixture.targetID).Order("occurred_at, event_id").Pluck("action", &actions).Error; err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0] != "execution_target.ssh_install_started" || actions[1] != "execution_target.ssh_install_failed" {
		t.Fatalf("unexpected failed provisioning audit actions: %#v", actions)
	}
}

type sshProvisionFixture struct {
	db          *gorm.DB
	provisioner *SSHProvisioner
	principal   identity.Principal
	tenantID    uuid.UUID
	targetID    uuid.UUID
}

func newSSHProvisionFixture(t *testing.T, controlPlaneURL string) sshProvisionFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "ssh-provision-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := NewService(store.DB(), platformConfig, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := targetService.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID,
		Kind:           "ssh",
		Name:           "managed-ssh",
		Configuration: map[string]any{
			"host": "ssh.example.com", "port": 2222, "user": "root",
			"privateKey": "ssh-private-key-secret", "hostKey": "ssh-ed25519 fake-host-key",
			"controlPlaneUrl": controlPlaneURL,
			"runnerCommand":   []string{"provider-host", "run", "--jsonl"},
			"installRoot":     "/opt/synara/test", "workspaceRoot": "/var/lib/synara/test/workspaces",
			"serviceUser": "root", "useSudo": false,
		},
		Capabilities: map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(t.TempDir(), "synara-agentd")
	if err := os.WriteFile(binaryPath, []byte("test-agentd-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	provisioner := NewSSHProvisioner(targetService, SSHProvisioningConfig{
		AgentdBinaryPath: binaryPath, RegistrationToken: "worker-registration-secret", Timeout: time.Second,
	})
	return sshProvisionFixture{
		db: store.DB(), provisioner: provisioner, principal: principal,
		tenantID: domain.TenantID, targetID: target.ID,
	}
}

type fakeSSHDialer struct {
	input  sshDialInput
	remote sshRemote
	err    error
	calls  int
}

func (d *fakeSSHDialer) Dial(_ context.Context, input sshDialInput) (sshRemote, error) {
	d.calls++
	d.input = input
	return d.remote, d.err
}

type fakeSSHRemote struct {
	uploads  map[string][]byte
	commands []string
	closed   bool
}

func (r *fakeSSHRemote) Upload(_ context.Context, path string, _ os.FileMode, source io.Reader) error {
	payload, err := io.ReadAll(source)
	if err != nil {
		return err
	}
	r.uploads[path] = payload
	return nil
}

func (r *fakeSSHRemote) Run(_ context.Context, command string) error {
	r.commands = append(r.commands, command)
	return nil
}

func (r *fakeSSHRemote) Close() error {
	r.closed = true
	return nil
}

func assertExecutionTargetProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

func commandsContainAll(commands []string, fragments ...string) bool {
	for _, command := range commands {
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(command, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

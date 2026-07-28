package executiontargets

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
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
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_LEASE_RENEW_INTERVAL="2s"`)) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_DRAIN_TIMEOUT="20s"`)) ||
		!bytes.Contains(environment, []byte(`SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION="1"`)) ||
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
	if !commandsContainAll(
		remote.commands,
		"--property=ActiveState",
		"--property=SubState",
		"--property=MainPID",
		"--property=NRestarts",
		"for attempt in 1 2 3",
		"sleep 1",
	) {
		t.Fatalf("SSH provisioning did not prove a stable post-start service window: %#v", remote.commands)
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

func TestSSHProvisionerRevokesAuthorityBeforeRemoteCleanup(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	order := make([]string, 0, 2)
	remote := &fakeSSHRemote{uploads: map[string][]byte{}, onRun: func(string) {
		order = append(order, "remote")
	}}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: remote}
	fixture.provisioner.revokeWorkers = func(
		_ context.Context,
		_ *gorm.DB,
		_ identity.Principal,
		target persistence.ExecutionTarget,
		generation int64,
		_, _, _ string,
	) (func(), error) {
		if target.Status != "offline" || target.SSHOperationGeneration != generation ||
			target.SSHOperationKind == nil || *target.SSHOperationKind != "revoke" {
			return nil, fmt.Errorf("authority callback observed invalid fence: %#v", target)
		}
		order = append(order, "authority")
		return nil, nil
	}

	result, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke-order", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "disabled" || !reflect.DeepEqual(order, []string{"authority", "remote"}) {
		t.Fatalf("SSH revoke order/result = %#v / %#v", order, result)
	}
}

func TestSSHProvisionerRevokerFailureDoesNotTouchRemote(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	dialer := &fakeSSHDialer{remote: &fakeSSHRemote{uploads: map[string][]byte{}}}
	fixture.provisioner.dialer = dialer
	fixture.provisioner.revokeWorkers = func(
		context.Context, *gorm.DB, identity.Principal, persistence.ExecutionTarget, int64, string, string, string,
	) (func(), error) {
		return nil, problem.New(500, "worker_revocation_failed", "Worker authority revocation failed.")
	}

	_, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke-authority-failure", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "worker_revocation_failed")
	if dialer.calls != 0 {
		t.Fatalf("revoker failure reached SSH remote %d times", dialer.calls)
	}
	assertSSHTargetStatusAndNoOperation(t, fixture.db, fixture.targetID, "offline")
}

func TestSSHProvisionerRevokesAuthorityBeforeConfigurationDecrypt(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	dialer := &fakeSSHDialer{remote: &fakeSSHRemote{uploads: map[string][]byte{}}}
	fixture.provisioner.dialer = dialer
	authorityRevoked := false
	fixture.provisioner.revokeWorkers = func(
		context.Context, *gorm.DB, identity.Principal, persistence.ExecutionTarget, int64, string, string, string,
	) (func(), error) {
		authorityRevoked = true
		return nil, nil
	}
	var originalTarget persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&originalTarget).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("configuration_encrypted", []byte("unreadable-kms-ciphertext")).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke-kms-failure", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_configuration_unavailable")
	if !authorityRevoked {
		t.Fatal("SSH configuration decrypt failed before local Worker authority was revoked")
	}
	if dialer.calls != 0 {
		t.Fatalf("SSH configuration decrypt failure reached remote %d times", dialer.calls)
	}
	assertSSHTargetStatusAndNoOperation(t, fixture.db, fixture.targetID, "offline")
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("configuration_encrypted", originalTarget.ConfigurationEncrypted).Error; err != nil {
		t.Fatal(err)
	}
	retried, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke-kms-retry", "127.0.0.1",
	)
	if err != nil || retried.Status != "disabled" {
		t.Fatalf("SSH remote cleanup retry after decrypt recovery = %#v, %v", retried, err)
	}
}

func TestSSHProvisionerAtomicRevokeRollsBackBeforeCommitFailure(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	dialer := &fakeSSHDialer{remote: &fakeSSHRemote{uploads: map[string][]byte{}}}
	fixture.provisioner.dialer = dialer
	var original persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	fixture.provisioner.revokeWorkers = func(
		ctx context.Context, tx *gorm.DB, _ identity.Principal, target persistence.ExecutionTarget, _ int64, _, _, _ string,
	) (func(), error) {
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
			Update("name", "must-roll-back").Error; err != nil {
			return nil, err
		}
		return nil, problem.New(500, "worker_revocation_failed", "Worker authority revocation failed.")
	}

	_, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke-double-failure", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "worker_revocation_failed")
	if dialer.calls != 0 {
		t.Fatalf("pre-commit failure reached SSH remote %d times", dialer.calls)
	}
	var after persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.Name != original.Name || after.Status != original.Status ||
		after.SSHOperationGeneration != original.SSHOperationGeneration || after.SSHOperationKind != nil {
		t.Fatalf("atomic SSH revoke failure did not roll back: before=%#v after=%#v", original, after)
	}
}

func TestSSHProvisionerRemoteRevokeFailureKeepsRevokedAuthorityOffline(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	authorityRevoked := false
	fixture.provisioner.revokeWorkers = func(
		context.Context, *gorm.DB, identity.Principal, persistence.ExecutionTarget, int64, string, string, string,
	) (func(), error) {
		authorityRevoked = true
		return nil, nil
	}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: &fakeSSHRemote{
		uploads: map[string][]byte{}, runErrors: []error{errors.New("remote unavailable")},
	}}

	_, err := fixture.provisioner.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-revoke-remote-failure", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_revoke_failed")
	if !authorityRevoked {
		t.Fatal("remote failure occurred before Worker authority was revoked")
	}
	assertSSHTargetStatusAndNoOperation(t, fixture.db, fixture.targetID, "offline")
}

func TestSSHProvisionerOperationGenerationRejectsConcurrentAndStaleCompletion(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	ctx := context.Background()
	target, _, err := fixture.provisioner.load(ctx, fixture.principal, fixture.tenantID, fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	fixture.provisioner.now = func() time.Time { return now }
	firstInstanceUID, concurrentInstanceUID := uuid.New(), uuid.New()
	first, err := fixture.provisioner.beginSSHOperation(
		ctx, target, fixture.principal.UserID, "install", &firstInstanceUID, "ssh-operation-first", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.provisioner.beginSSHOperation(
		ctx, target, fixture.principal.UserID, "upgrade", &concurrentInstanceUID, "ssh-operation-concurrent", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_operation_in_progress")
	staleStartedAt := now.Add(-fixture.provisioner.timeout() - 31*time.Second)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("ssh_operation_started_at", staleStartedAt).Error; err != nil {
		t.Fatal(err)
	}
	second, err := fixture.provisioner.beginSSHOperation(
		ctx, target, fixture.principal.UserID, "revoke", nil, "ssh-operation-takeover", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 || second.Kind != "revoke" {
		t.Fatalf("stale operation takeover fence = %#v after %#v", second, first)
	}
	if err := fixture.provisioner.finishSSHOperation(
		ctx, target, fixture.principal.UserID, second, "completed", "disabled", "ssh-operation-takeover", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.provisioner.failSSHOperation(
		ctx, target, fixture.principal.UserID, first, "ssh-operation-stale-failure", "127.0.0.1",
	); err == nil {
		t.Fatal("stale failure overwrote newer disabled result")
	} else {
		assertExecutionTargetProblemCode(t, err, "ssh_operation_superseded")
	}
	assertSSHTargetStatusAndNoOperation(t, fixture.db, fixture.targetID, "disabled")
}

func TestSSHProvisionerResumesCommittedRevokeFenceAfterCrash(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	target, err := fixture.provisioner.loadTargetMetadata(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
	)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	fixture.provisioner.revokeWorkers = func(
		context.Context, *gorm.DB, identity.Principal, persistence.ExecutionTarget, int64, string, string, string,
	) (func(), error) {
		calls++
		return nil, nil
	}
	first, err := fixture.provisioner.beginSSHRevokeOperation(
		context.Background(), target, fixture.principal, "revoke-before-crash", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.provisioner.beginSSHRevokeOperation(
		context.Background(), target, fixture.principal, "revoke-after-crash", "127.0.0.1",
	)
	if err != nil {
		t.Fatalf("committed revoke fence could not resume: %v", err)
	}
	if resumed != first || calls != 2 {
		t.Fatalf("revoke resume fence/callback = %#v/%d, want %#v/2", resumed, calls, first)
	}
}

func TestSSHProvisionerKeepsTargetOfflineUntilExactWorkerReady(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	remote := &fakeSSHRemote{uploads: map[string][]byte{}}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: remote}
	var observedInstanceUID string
	fixture.provisioner.awaitWorkerReady = func(
		ctx context.Context,
		target persistence.ExecutionTarget,
		_ sshTargetConfiguration,
		instanceUID string,
	) error {
		var current persistence.ExecutionTarget
		if err := fixture.db.WithContext(ctx).Where("id = ?", target.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != "offline" {
			return fmt.Errorf("target status during Worker readiness = %s", current.Status)
		}
		observedInstanceUID = instanceUID
		return nil
	}

	result, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-ready-boundary", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	environment := remote.uploads["/tmp/synara-agentd-"+fixture.targetID.String()+".env"]
	if observedInstanceUID == "" || observedInstanceUID != environmentValue(t, environment, "SYNARA_AGENTD_INSTANCE_UID") {
		t.Fatalf("readiness instance UID %q did not match installed environment", observedInstanceUID)
	}
	if result.Status != "active" {
		t.Fatalf("install result = %#v", result)
	}
}

func TestSSHProvisionerReadinessFailureLeavesTargetOffline(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	fixture.provisioner.dialer = &fakeSSHDialer{remote: &fakeSSHRemote{uploads: map[string][]byte{}}}
	fixture.provisioner.awaitWorkerReady = func(
		context.Context,
		persistence.ExecutionTarget,
		sshTargetConfiguration,
		string,
	) error {
		return errors.New("exact Worker instance remained untrusted")
	}

	_, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-ready-failure", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_worker_readiness_failed")
	var target persistence.ExecutionTarget
	if loadErr := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; loadErr != nil {
		t.Fatal(loadErr)
	}
	if target.Status != "offline" {
		t.Fatalf("failed readiness target status = %q", target.Status)
	}
}

func TestSSHProvisionerAtomicallyRechecksWorkerBeforeActivation(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	fixture.provisioner.dialer = &fakeSSHDialer{remote: &fakeSSHRemote{uploads: map[string][]byte{}}}
	fixture.provisioner.awaitWorkerReady = func(
		context.Context,
		persistence.ExecutionTarget,
		sshTargetConfiguration,
		string,
	) error {
		return nil
	}
	fixture.provisioner.checkWorkerReady = func(
		context.Context,
		*gorm.DB,
		persistence.ExecutionTarget,
		sshTargetConfiguration,
		string,
	) (bool, string, error) {
		return false, "exact Worker heartbeat became stale", nil
	}

	_, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-ready-race", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_worker_readiness_changed")
	var target persistence.ExecutionTarget
	if loadErr := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; loadErr != nil {
		t.Fatal(loadErr)
	}
	if target.Status != "offline" {
		t.Fatalf("readiness race target status = %q", target.Status)
	}
}

func TestOfflineSSHTargetRequiresLockedExactBootstrapAuthority(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	target, _, err := fixture.provisioner.load(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
	)
	if err != nil {
		t.Fatal(err)
	}
	expectedInstanceUID := uuid.New()
	fence, err := fixture.provisioner.beginSSHOperation(
		context.Background(), target, fixture.principal.UserID, "install", &expectedInstanceUID,
		"ssh-bootstrap-authority", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.provisioner.targets.ResolveWorkerBootstrapTarget(
		context.Background(), fixture.targetID, "ssh",
	); err == nil {
		t.Fatal("offline SSH Target resolved without transaction-bound bootstrap authority")
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, kind, resolveErr := fixture.provisioner.targets.ResolveWorkerRegistrationTargetInTransaction(
			context.Background(), tx, fixture.targetID, "ssh", expectedInstanceUID.String(), &fence.Generation, false,
		)
		if resolveErr != nil {
			return resolveErr
		}
		if kind != platform.TargetSSH {
			return fmt.Errorf("bootstrap target kind = %q", kind)
		}
		return nil
	}); err != nil {
		t.Fatalf("exact SSH bootstrap authority did not resolve: %v", err)
	}
}

func TestSSHWorkerReadyRequiresExactFreshCompatibleManifest(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	ctx := context.Background()
	target, configuration, err := fixture.provisioner.load(ctx, fixture.principal, fixture.tenantID, fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err = fixture.provisioner.normalize(target, configuration)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	fixture.provisioner.now = func() time.Time { return now }
	fixture.provisioner.config.WorkerHeartbeatTimeout = 30 * time.Second
	manifest := persistence.WorkerManifest{
		ID: uuid.New(), ManifestHash: strings.Repeat("a", 64), WorkerBuildVersion: "managed",
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "arm64", ProcessContainmentMode: "none",
		ProcessContainmentTrustMode: "none", FeatureFlags: map[string]any{}, CreatedAt: now.Add(-time.Minute),
	}
	if err := fixture.db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	exactInstanceUID := uuid.NewString()
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: exactInstanceUID,
		ExecutionTargetID: fixture.targetID, TargetKind: "ssh", WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: "ssh", Namespace: "default", PodName: "ssh-" + fixture.targetID.String(),
		Version: "managed", ProtocolVersion: 2, Capabilities: map[string]any{}, CurrentManifestID: &manifest.ID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: func() *time.Time { value := now.Add(-2 * time.Minute); return &value }(),
		LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte("hash"), Status: "online", AdministrativeStatus: "active",
		RegisteredAt: now.Add(-2 * time.Minute), LastHeartbeatAt: now.Add(-5 * time.Second),
	}
	if err := fixture.db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	ready, state, err := fixture.provisioner.sshWorkerReady(ctx, target, configuration, exactInstanceUID)
	if err != nil || !ready {
		t.Fatalf("exact Worker readiness = ready:%t state:%q err:%v", ready, state, err)
	}
	fence, err := fixture.provisioner.beginSSHOperation(
		ctx, target, fixture.principal.UserID, "install", func() *uuid.UUID {
			value := uuid.MustParse(exactInstanceUID)
			return &value
		}(), "atomic-ready", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.provisioner.checkWorkerReady = fixture.provisioner.sshWorkerReadyWithDB
	fixture.provisioner.lockWorkerReady = lockExactSSHWorkerReadiness
	if err := fixture.provisioner.activateReadySSHWorker(
		ctx, target, configuration, exactInstanceUID, fence, fixture.principal.UserID,
		"atomic-ready", "127.0.0.1",
	); err != nil {
		t.Fatalf("atomically activate exact ready Worker: %v", err)
	}
	ready, _, err = fixture.provisioner.sshWorkerReady(ctx, target, configuration, uuid.NewString())
	if err != nil || ready {
		t.Fatalf("stale/other instance readiness = ready:%t err:%v", ready, err)
	}
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("last_heartbeat_at", now.Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	ready, state, err = fixture.provisioner.sshWorkerReady(ctx, target, configuration, exactInstanceUID)
	if err != nil || ready || !strings.Contains(state, "stale") {
		t.Fatalf("stale heartbeat readiness = ready:%t state:%q err:%v", ready, state, err)
	}
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("last_heartbeat_at", now.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	ready, _, err = fixture.provisioner.sshWorkerReady(ctx, target, configuration, exactInstanceUID)
	if err == nil || ready || !strings.Contains(err.Error(), "future") {
		t.Fatalf("future heartbeat readiness = ready:%t err:%v", ready, err)
	}
}

func TestSSHWorkerReadyRejectsPersistedSignedV1ProtectedCgroupManifest(t *testing.T) {
	publicKey := processContainmentTestPublicKeyBase64()
	target := persistence.ExecutionTarget{
		Capabilities: map[string]any{"processContainmentPolicy": map[string]any{
			"trustMode": ProcessContainmentTrustSignedV1, "keyId": "trusted-key", "ed25519PublicKey": publicKey,
		}},
	}
	providerUID, providerGID := 10001, 10002
	configuration := sshTargetConfiguration{
		AgentdVersion: "agentd-1", AgentdBuildGitSHA: "abcdef0", AgentdImageDigest: "sha256:" + strings.Repeat("1", 64),
		CgroupV2Root:        "/sys/fs/cgroup/system.slice/synara-agentd.service",
		CgroupV2ProviderUID: &providerUID, CgroupV2ProviderGID: &providerGID,
		CgroupV2AttestationKeyID: "trusted-key", CgroupV2AttestationKeyPath: "/etc/synara/key",
	}
	version, probe, supervisor, provider := "v1", 1, "uid:0 gid:0", "uid:10001 gid:10002"
	gitSHA, imageDigest := configuration.AgentdBuildGitSHA, configuration.AgentdImageDigest
	manifest := persistence.WorkerManifest{
		WorkerBuildVersion: configuration.AgentdVersion, WorkerBuildGitSHA: &gitSHA,
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, OperatingSystem: "linux",
		ImageDigest: &imageDigest, ProcessContainmentMode: "cgroup-v2",
		ProcessContainmentSupervisorVersion: &version, ProcessContainmentProbeVersion: &probe,
		ProcessContainmentProbeSHA256:        func() *string { value := strings.Repeat("a", 64); return &value }(),
		ProcessContainmentSupervisorIdentity: &supervisor, ProcessContainmentProviderIdentity: &provider,
		ProcessContainmentTrustMode:        ProcessContainmentTrustSignedV1,
		ProcessContainmentAttestationKeyID: func() *string { value := "trusted-key"; return &value }(),
		ProcessContainmentAttestationKeySHA256: func() *string {
			value := processContainmentPublicKeySHA256(processContainmentTestPublicKey())
			return &value
		}(),
	}
	if err := validateSSHWorkerManifestReadiness(target, configuration, manifest); err == nil || !strings.Contains(err.Error(), "trustState") {
		t.Fatalf("persisted signed v1 protected-cgroup Manifest error = %v", err)
	}
	version = "agentd-protected-cgroup-supervisor-v2"
	if err := validateSSHWorkerManifestReadiness(target, configuration, manifest); err == nil || !strings.Contains(err.Error(), "trustState") {
		t.Fatalf("persisted signed v2 protected-cgroup Manifest error = %v", err)
	}
	version = ProtectedCgroupSupervisorVersionV3
	probe = ProtectedCgroupProbeVersionV3
	if err := validateSSHWorkerManifestReadiness(target, configuration, manifest); err != nil {
		t.Fatalf("signed v3 protected-cgroup Manifest error = %v", err)
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

func TestSSHProvisionerInstallRefusesExistingManagedPathsBeforeUpload(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	remote := &fakeSSHRemote{
		uploads:   map[string][]byte{},
		runErrors: []error{&sshRemoteCommandExitError{status: sshInstallConflictExitStatus}},
	}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: remote}

	_, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-install-conflict", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_install_conflict")
	if len(remote.uploads) != 0 {
		t.Fatalf("conflicting SSH install uploaded %d artifacts", len(remote.uploads))
	}
	if len(remote.commands) != 1 ||
		!commandsContainAll(
			remote.commands,
			"systemctl cat",
			"systemctl show-environment",
			"exit 73",
			"/etc/systemd/system/synara-agentd-"+fixture.targetID.String()+".service",
			"/opt/synara/test",
			"/var/lib/synara/test/workspaces",
		) {
		t.Fatalf("SSH install conflict preflight was incomplete: %#v", remote.commands)
	}
}

func TestSSHProvisionerInstallPreflightReportsRemoteFailureSeparatelyFromConflict(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	remote := &fakeSSHRemote{
		uploads:   map[string][]byte{},
		runErrors: []error{errors.New("SSH session lost")},
	}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: remote}

	_, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-install-preflight-failed", "127.0.0.1",
	)
	assertExecutionTargetProblemCode(t, err, "ssh_install_preflight_failed")
	if len(remote.uploads) != 0 {
		t.Fatalf("failed SSH install preflight uploaded %d artifacts", len(remote.uploads))
	}
	if _, _, resolveErr := fixture.provisioner.targets.ResolveWorkerTarget(
		context.Background(), fixture.targetID, "ssh",
	); resolveErr == nil {
		t.Fatal("failed SSH install preflight left Target routable")
	}
}

func TestSSHProvisionerProtectedCgroupInstallAddsDelegateAndSignedContainmentEnv(t *testing.T) {
	fixture := newSSHProvisionFixtureWithConfiguration(t, "https://control-plane.example.com", map[string]any{
		"agentdVersion":                     "agentd-1.2.3",
		"agentdBuildGitSha":                 "abcdef0123456789abcdef0123456789abcdef01",
		"agentdImageDigest":                 "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"cgroupV2ProviderUid":               10001,
		"cgroupV2ProviderGid":               10002,
		"cgroupV2ProviderPidsMax":           512,
		"cgroupV2ProviderMemoryMaxBytes":    8589934592,
		"cgroupV2ProviderCpuQuotaMicros":    400000,
		"cgroupV2ProviderCpuPeriodMicros":   100000,
		"cgroupV2AttestationKeyId":          "ssh-protected-key",
		"cgroupV2AttestationPrivateKeyPath": "/etc/synara/keys/process-containment.ed25519",
	})
	remote := &fakeSSHRemote{uploads: map[string][]byte{}}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: remote}

	if _, err := fixture.provisioner.Install(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"ssh-protected-install", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	var environment, unit []byte
	for path, payload := range remote.uploads {
		switch {
		case strings.HasSuffix(path, ".env"):
			environment = payload
		case strings.HasSuffix(path, ".service"):
			unit = payload
		}
	}
	expectedCgroupV2Root := "/sys/fs/cgroup/system.slice/synara-agentd-" + fixture.targetID.String() + ".service"
	for _, fragment := range []string{
		`SYNARA_AGENTD_VERSION="agentd-1.2.3"`,
		`SYNARA_AGENTD_BUILD_GIT_SHA="abcdef0123456789abcdef0123456789abcdef01"`,
		`SYNARA_AGENTD_IMAGE_DIGEST="sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`,
		`SYNARA_AGENTD_CGROUP_V2_ROOT="` + expectedCgroupV2Root + `"`,
		`SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID="10001"`,
		`SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID="10002"`,
		`SYNARA_AGENTD_CGROUP_V2_PROVIDER_PIDS_MAX="512"`,
		`SYNARA_AGENTD_CGROUP_V2_PROVIDER_MEMORY_MAX_BYTES="8589934592"`,
		`SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_QUOTA_MICROS="400000"`,
		`SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_PERIOD_MICROS="100000"`,
		`SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID="ssh-protected-key"`,
		`SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE="/etc/synara/keys/process-containment.ed25519"`,
	} {
		if !bytes.Contains(environment, []byte(fragment)) {
			t.Fatalf("protected SSH env omitted %q: %s", fragment, environment)
		}
	}
	instanceUID := environmentValue(t, environment, "SYNARA_AGENTD_INSTANCE_UID")
	if parsed, err := uuid.Parse(instanceUID); err != nil || parsed == uuid.Nil {
		t.Fatalf("protected SSH env instance UID = %q", instanceUID)
	}
	if !bytes.Contains(unit, []byte("User=root\n")) || !bytes.Contains(unit, []byte("Delegate=yes\n")) ||
		!bytes.Contains(unit, []byte("DelegateSubgroup=synara-agentd\n")) {
		t.Fatalf("protected SSH unit omitted root+Delegate: %s", unit)
	}
	if !commandsContainAll(
		remote.commands,
		"stat -fc %T",
		"/sys/fs/cgroup",
		"test -f",
		"/etc/synara/keys/process-containment.ed25519",
		"stat -c %u",
		"stat -c %F",
		"stat -c %a",
	) {
		t.Fatalf("protected SSH install omitted cgroup/key preflight: %#v", remote.commands)
	}
	if !commandsContainAll(
		remote.commands,
		"--property=ActiveState",
		"--property=SubState",
		"--property=MainPID",
		"--property=NRestarts",
		"for attempt in 1 2 3",
		"--property=ControlGroup",
		expectedCgroupV2Root,
		"test -d",
	) {
		t.Fatalf("protected SSH install omitted post-start ControlGroup proof: %#v", remote.commands)
	}
	if !commandsContainAll(
		remote.commands,
		"install -d -m 0711",
		"/var/lib/synara/test/workspaces",
		"install -d -m 0700",
		"/var/lib/synara/targets/"+fixture.targetID.String()+"/git-cache",
	) {
		t.Fatalf("protected SSH install omitted protected storage permissions: %#v", remote.commands)
	}
}

func TestSSHProvisionerProtectedCgroupRequiresRootServiceUserAndFullBuildIdentity(t *testing.T) {
	provisioner := &SSHProvisioner{}
	target := persistence.ExecutionTarget{ID: uuid.New()}
	_, _, err := provisioner.normalize(target, sshTargetConfiguration{
		Host: "ssh.example.com", User: "root", PrivateKey: "private-key", HostKey: "host-key",
		ControlPlaneURL: "https://control-plane.example.com", RunnerCommand: []string{"runner"},
		ServiceUser:  "synara",
		CgroupV2Root: "/sys/fs/cgroup/system.slice/synara-agentd.service",
		CgroupV2ProviderUID: func() *int {
			value := 10001
			return &value
		}(),
		CgroupV2ProviderGID: func() *int {
			value := 10002
			return &value
		}(),
		CgroupV2AttestationKeyID:   "ssh-protected-key",
		CgroupV2AttestationKeyPath: "/etc/synara/keys/process-containment.ed25519",
		AgentdVersion:              "agentd-1.2.3",
	})
	assertExecutionTargetProblemCode(t, err, "invalid_ssh_configuration")
}

func TestSSHServiceStableCommandExecutesThreeHealthySamples(t *testing.T) {
	script := `
systemctl() {
  case "$*" in
    *--property=NRestarts*) printf '%s\n' 0 ;;
    *--property=ActiveState*) printf '%s\n' active ;;
    *--property=SubState*) printf '%s\n' running ;;
    *--property=MainPID*) printf '%s\n' 123 ;;
    *) return 1 ;;
  esac
}
sleep() { :; }
` + sshServiceStableCommand("synara-agentd-test.service")
	if output, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("stable service command failed: %v: %s", err, output)
	}
}

func TestSSHServiceStableCommandRejectsUnhealthySample(t *testing.T) {
	script := `
systemctl() {
  case "$*" in
    *--property=NRestarts*) printf '%s\n' 0 ;;
    *--property=ActiveState*) printf '%s\n' activating ;;
    *--property=SubState*) printf '%s\n' auto-restart ;;
    *--property=MainPID*) printf '%s\n' 0 ;;
    *) return 1 ;;
  esac
}
sleep() { :; }
` + sshServiceStableCommand("synara-agentd-test.service")
	if output, err := exec.Command("sh", "-c", script).CombinedOutput(); err == nil {
		t.Fatalf("unstable service command unexpectedly passed: %s", output)
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

func TestNewSSHClientConfigPinsEd25519HostKeyAlgorithmAndRejectsMismatches(t *testing.T) {
	clientSigner := mustNewEd25519Signer(t)
	expectedHostKey := mustNewEd25519Signer(t).PublicKey()
	otherHostKey := mustNewEd25519Signer(t).PublicKey()

	clientConfig, err := newSSHClientConfig("root", clientSigner, expectedHostKey)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{ssh.KeyAlgoED25519}; !reflect.DeepEqual(clientConfig.HostKeyAlgorithms, want) {
		t.Fatalf("unexpected Ed25519 host key algorithms: got %v want %v", clientConfig.HostKeyAlgorithms, want)
	}
	if err := clientConfig.HostKeyCallback("ssh.example.com:22", dummyAddr("127.0.0.1:22"), expectedHostKey); err != nil {
		t.Fatalf("expected pinned host key to be accepted: %v", err)
	}
	if err := clientConfig.HostKeyCallback("ssh.example.com:22", dummyAddr("127.0.0.1:22"), otherHostKey); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected mismatched host key rejection, got %v", err)
	}
}

func TestSupportedSSHHostKeyAlgorithmsRestrictsRSAKeysToSupportedSHA2(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	algorithms, err := supportedSSHHostKeyAlgorithms(rsaSigner.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}
	if !reflect.DeepEqual(algorithms, want) {
		t.Fatalf("unexpected RSA host key algorithms: got %v want %v", algorithms, want)
	}
}

func TestSupportedSSHHostKeyAlgorithmsRestrictsRSACertsToSupportedSHA2(t *testing.T) {
	algorithms, err := supportedSSHHostKeyAlgorithms(fakeSSHPublicKey{typ: ssh.CertAlgoRSAv01})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01}
	if !reflect.DeepEqual(algorithms, want) {
		t.Fatalf("unexpected RSA cert host key algorithms: got %v want %v", algorithms, want)
	}
}

func TestSupportedSSHHostKeyAlgorithmsRejectsUnsupportedTypes(t *testing.T) {
	_, err := supportedSSHHostKeyAlgorithms(fakeSSHPublicKey{typ: "ssh-unsupported"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported host key algorithm error, got %v", err)
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
	return newSSHProvisionFixtureWithConfiguration(t, controlPlaneURL, nil)
}

func newSSHProvisionFixtureWithConfiguration(
	t *testing.T,
	controlPlaneURL string,
	configurationOverrides map[string]any,
) sshProvisionFixture {
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
	configuration := map[string]any{
		"host": "ssh.example.com", "port": 2222, "user": "root",
		"privateKey": "ssh-private-key-secret", "hostKey": "ssh-ed25519 fake-host-key",
		"controlPlaneUrl": controlPlaneURL,
		"runnerCommand":   []string{"provider-host", "run", "--jsonl"},
		"installRoot":     "/opt/synara/test", "workspaceRoot": "/var/lib/synara/test/workspaces",
		"serviceUser": "root", "useSudo": false,
	}
	for key, value := range configurationOverrides {
		configuration[key] = value
	}
	target, err := targetService.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID,
		Kind:           "ssh",
		Name:           "managed-ssh",
		Configuration:  configuration,
		Capabilities:   map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(t.TempDir(), "synara-agentd")
	if err := os.WriteFile(binaryPath, []byte("test-agentd-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	provisioner := NewSSHProvisioner(targetService, SSHProvisioningConfig{
		AgentdBinaryPath: binaryPath, RegistrationToken: "worker-registration-secret",
		WorkerLeaseTTL: 6 * time.Second, Timeout: time.Second,
	})
	provisioner.awaitWorkerReady = func(
		context.Context,
		persistence.ExecutionTarget,
		sshTargetConfiguration,
		string,
	) error {
		return nil
	}
	provisioner.checkWorkerReady = func(
		context.Context,
		*gorm.DB,
		persistence.ExecutionTarget,
		sshTargetConfiguration,
		string,
	) (bool, string, error) {
		return true, "fixture Worker is ready", nil
	}
	provisioner.lockWorkerReady = func(context.Context, *gorm.DB, uuid.UUID, string) error { return nil }
	provisioner.revokeWorkers = func(
		context.Context,
		*gorm.DB,
		identity.Principal,
		persistence.ExecutionTarget,
		int64,
		string,
		string,
		string,
	) (func(), error) {
		return nil, nil
	}
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
	uploads   map[string][]byte
	commands  []string
	runErrors []error
	onRun     func(string)
	closed    bool
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
	if r.onRun != nil {
		r.onRun(command)
	}
	if len(r.runErrors) > 0 {
		err := r.runErrors[0]
		r.runErrors = r.runErrors[1:]
		return err
	}
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

func assertSSHTargetStatusAndNoOperation(t *testing.T, db *gorm.DB, targetID uuid.UUID, status string) {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != status || target.SSHOperationKind != nil || target.SSHOperationStartedAt != nil ||
		target.SSHExpectedInstanceUID != nil {
		t.Fatalf("SSH Target state = %#v, want status %q with no active operation", target, status)
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

func environmentValue(t *testing.T, environment []byte, name string) string {
	t.Helper()
	prefix := name + "="
	for _, line := range strings.Split(string(environment), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.Unquote(strings.TrimPrefix(line, prefix))
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return value
	}
	t.Fatalf("environment omitted %s", name)
	return ""
}

func mustNewEd25519Signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

type fakeSSHPublicKey struct {
	typ string
}

func (k fakeSSHPublicKey) Type() string { return k.typ }

func (k fakeSSHPublicKey) Marshal() []byte { return []byte(k.typ) }

func (k fakeSSHPublicKey) Verify(_ []byte, _ *ssh.Signature) error { return nil }

type dummyAddr string

func (a dummyAddr) Network() string { return "tcp" }

func (a dummyAddr) String() string { return string(a) }

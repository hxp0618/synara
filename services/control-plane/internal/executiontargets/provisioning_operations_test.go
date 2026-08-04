package executiontargets

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProvisioningOperationDurableIdempotencyAndClaimTakeover(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "provisioning-operation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipherWithKeyring(secret.CipherKey{ID: "runtime-v2", Key: bytes.Repeat([]byte{0x42}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "ssh", Name: "async-build-host",
		Configuration: map[string]any{"host": "build.example", "privateKey": "never-project-me"},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, replayed, err := service.CreateProvisioningOperation(
		ctx, principal, domain.TenantID, target.ID, "install", "provision-once", "request-1", "127.0.0.1",
	)
	if err != nil || replayed || first.State != "accepted" {
		t.Fatalf("first operation = %#v replayed=%t err=%v", first, replayed, err)
	}
	second, replayed, err := service.CreateProvisioningOperation(
		ctx, principal, domain.TenantID, target.ID, "install", "provision-once", "request-2", "127.0.0.1",
	)
	if err != nil || !replayed || second.ID != first.ID {
		t.Fatalf("replayed operation = %#v replayed=%t err=%v", second, replayed, err)
	}
	_, _, err = service.CreateProvisioningOperation(
		ctx, principal, domain.TenantID, target.ID, "upgrade", "provision-once", "request-3", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 409, "idempotency_conflict")
	_, _, err = service.CreateProvisioningOperation(
		ctx, principal, domain.TenantID, target.ID, "upgrade", "provision-second", "request-4", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 409, "provisioning_in_progress")

	claim, found, err := service.ClaimNextProvisioningOperation(ctx, "control-plane-a", time.Minute)
	if err != nil || !found || claim.Operation.ID != first.ID || claim.Operation.AttemptGeneration != 1 {
		t.Fatalf("first claim = %#v found=%t err=%v", claim, found, err)
	}
	if _, found, err := service.ClaimNextProvisioningOperation(ctx, "control-plane-b", time.Minute); err != nil || found {
		t.Fatalf("unexpired operation was claimed again: found=%t err=%v", found, err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := store.DB().Model(&persistence.ExecutionTargetProvisioningOperation{}).
		Where("id = ?", first.ID).Update("claim_expires_at", past).Error; err != nil {
		t.Fatal(err)
	}
	takeover, found, err := service.ClaimNextProvisioningOperation(ctx, "control-plane-b", time.Minute)
	if err != nil || !found || takeover.Operation.ID != first.ID || takeover.Operation.AttemptGeneration != 2 || takeover.ClaimHolder != "control-plane-b" {
		t.Fatalf("takeover claim = %#v found=%t err=%v", takeover, found, err)
	}
	_, err = service.CompleteProvisioningSuccess(ctx, claim, SSHProvisionResult{
		TargetID: target.ID, Operation: "install", Status: "active", BinarySHA256: strings.Repeat("a", 64),
	})
	assertExecutionTargetProblem(t, err, 409, "provisioning_claim_lost")
	completed, err := service.CompleteProvisioningSuccess(ctx, takeover, SSHProvisionResult{
		TargetID: target.ID, Operation: "install", Status: "active", BinarySHA256: strings.Repeat("b", 64),
	})
	if err != nil || completed.State != "succeeded" || completed.Result["binarySha256"] != strings.Repeat("b", 64) {
		t.Fatalf("completed operation = %#v err=%v", completed, err)
	}
	loaded, err := service.GetProvisioningOperation(ctx, principal, domain.TenantID, target.ID, first.ID)
	if err != nil || loaded.State != "succeeded" || loaded.Error != nil {
		t.Fatalf("loaded operation = %#v err=%v", loaded, err)
	}
	failedOperation, replayed, err := service.CreateProvisioningOperation(
		ctx, principal, domain.TenantID, target.ID, "upgrade", "provision-upgrade", "request-5", "127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("upgrade operation = %#v replayed=%t err=%v", failedOperation, replayed, err)
	}
	reconciler, err := NewProvisioningReconciler(service, provisioningExecutorFunc(func(context.Context, ProvisioningClaim) (SSHProvisionResult, error) {
		return SSHProvisionResult{}, errors.New("ssh stderr contains private-key-material")
	}), "control-plane-reconciler", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled, err := reconciler.ReconcileOnce(ctx); err != nil || !reconciled {
		t.Fatalf("reconcile failure projection: reconciled=%t err=%v", reconciled, err)
	}
	failed, err := service.GetProvisioningOperation(ctx, principal, domain.TenantID, target.ID, failedOperation.ID)
	if err != nil || failed.State != "failed" || failed.Error == nil || failed.Error.Code != "provisioning_failed" || strings.Contains(failed.Error.Message, "private-key") {
		t.Fatalf("safe failed projection = %#v err=%v", failed, err)
	}
	if StableProvisioningWorkerInstanceUID(failed.ID) != StableProvisioningWorkerInstanceUID(failed.ID) {
		t.Fatal("stable provisioning Worker identity changed for the same operation")
	}

	var auditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action = ?", domain.TenantID, first.ID, "execution_target.provisioning_accepted").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("accepted audit count = %d, want 1", auditCount)
	}
}

func TestSSHProvisioningOperationResumeKeepsFenceAndWorkerIdentity(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	operationID := uuid.New()
	workerUID := StableProvisioningWorkerInstanceUID(operationID)
	first, resumed, err := fixture.provisioner.beginSSHOperationForProvisioning(
		context.Background(), target, fixture.principal.UserID, "install", &workerUID,
		"first-attempt", "127.0.0.1", &operationID,
	)
	if err != nil || resumed || first.ProvisioningOperationID == nil {
		t.Fatalf("first operation fence = %#v resumed=%t err=%v", first, resumed, err)
	}
	second, resumed, err := fixture.provisioner.beginSSHOperationForProvisioning(
		context.Background(), target, fixture.principal.UserID, "install", &workerUID,
		"takeover-attempt", "127.0.0.1", &operationID,
	)
	if err != nil || !resumed || second.Generation != first.Generation || second.ProvisioningOperationID == nil || *second.ProvisioningOperationID != operationID {
		t.Fatalf("resumed operation fence = %#v resumed=%t err=%v", second, resumed, err)
	}
	differentOperationID := uuid.New()
	differentWorkerUID := StableProvisioningWorkerInstanceUID(differentOperationID)
	_, _, err = fixture.provisioner.beginSSHOperationForProvisioning(
		context.Background(), target, fixture.principal.UserID, "install", &differentWorkerUID,
		"competing-attempt", "127.0.0.1", &differentOperationID,
	)
	assertExecutionTargetProblem(t, err, 409, "ssh_operation_in_progress")
}

func TestSSHProvisioningOperationRecoversAfterRemoteSuccessAndProcessCancellation(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	operationID := uuid.New()
	firstRemote := &fakeSSHRemote{uploads: map[string][]byte{}}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: firstRemote}
	crashedContext, cancelCrash := context.WithCancel(context.Background())
	fixture.provisioner.awaitWorkerReady = func(context.Context, persistence.ExecutionTarget, sshTargetConfiguration, string) error {
		cancelCrash()
		return crashedContext.Err()
	}
	_, err := fixture.provisioner.ApplyProvisioningOperation(
		crashedContext, fixture.principal, fixture.tenantID, fixture.targetID,
		"install", operationID, "crashed-attempt", "127.0.0.1",
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first attempt error = %v, want cancellation", err)
	}
	var interrupted persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&interrupted).Error; err != nil {
		t.Fatal(err)
	}
	if interrupted.SSHProvisioningOperationID == nil || *interrupted.SSHProvisioningOperationID != operationID || interrupted.SSHOperationKind == nil {
		t.Fatalf("canceled operation lost its recovery fence: %#v", interrupted)
	}

	secondRemote := &fakeSSHRemote{uploads: map[string][]byte{}}
	fixture.provisioner.dialer = &fakeSSHDialer{remote: secondRemote}
	fixture.provisioner.awaitWorkerReady = func(context.Context, persistence.ExecutionTarget, sshTargetConfiguration, string) error { return nil }
	recovered, err := fixture.provisioner.ApplyProvisioningOperation(
		context.Background(), fixture.principal, fixture.tenantID, fixture.targetID,
		"install", operationID, "takeover-attempt", "127.0.0.1",
	)
	if err != nil || recovered.Status != "active" {
		t.Fatalf("recovered provisioning = %#v err=%v", recovered, err)
	}
	if len(secondRemote.commands) == 0 {
		t.Fatal("same-operation recovery did not run the idempotent install command")
	}
	for _, command := range secondRemote.commands {
		if strings.Contains(command, "exit 73") {
			t.Fatalf("same-operation recovery reran first-install path conflict preflight: %#v", secondRemote.commands)
		}
	}
	var completed persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&completed).Error; err != nil {
		t.Fatal(err)
	}
	if completed.Status != "active" || completed.SSHProvisioningOperationID != nil || completed.SSHOperationKind != nil {
		t.Fatalf("recovered target state = %#v", completed)
	}
}

func TestSSHProvisioningExecutorReauthorizesServiceAccountAtExecutionTime(t *testing.T) {
	fixture := newSSHProvisionFixture(t, "https://control-plane.example.com")
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	account := persistence.ServiceAccount{
		ID: uuid.New(), TenantID: fixture.tenantID,
		Name: "provisioner", Status: "active", Role: "owner", Scopes: []string{"api.access"},
		RateLimitPerMinute: 600, CreatedBy: fixture.principal.UserID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := fixture.db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	executor, err := NewSSHProvisioningOperationExecutor(fixture.provisioner.targets, fixture.provisioner)
	if err != nil {
		t.Fatal(err)
	}
	claim := ProvisioningClaim{
		Operation: ProvisioningOperation{ID: uuid.New(), TargetID: fixture.targetID, Action: "install", AttemptGeneration: 1},
		TenantID:  fixture.tenantID, OrganizationID: target.OrganizationID, ActorType: "service_account", ActorID: account.ID,
	}
	principal, authorizedContext, err := executor.resolvePrincipal(context.Background(), claim)
	if err != nil || principal.ServiceAccountID == nil || *principal.ServiceAccountID != account.ID {
		t.Fatalf("resolved machine principal = %#v err=%v", principal, err)
	}
	machine, ok := authorization.MachinePrincipalFromContext(authorizedContext)
	if !ok || machine.ActorID != account.ID || machine.Role != "owner" {
		t.Fatalf("resolved machine authorization = %#v found=%t", machine, ok)
	}
	if machine.OrganizationID != nil {
		t.Fatalf("tenant-scoped provisioning machine unexpectedly became organization-scoped: %#v", machine)
	}
	now := time.Now().UTC()
	if err := fixture.db.Model(&persistence.ServiceAccount{}).Where("id = ?", account.ID).
		Updates(map[string]any{"status": "revoked", "revoked_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err = executor.resolvePrincipal(context.Background(), claim)
	assertExecutionTargetProblem(t, err, 403, "provisioning_actor_revoked")
}

type provisioningExecutorFunc func(context.Context, ProvisioningClaim) (SSHProvisionResult, error)

func (function provisioningExecutorFunc) Execute(ctx context.Context, claim ProvisioningClaim) (SSHProvisionResult, error) {
	return function(ctx, claim)
}

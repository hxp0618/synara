package executions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestPostgresWorkerRevocationLinearizesWithLeaseRenewal(t *testing.T) {
	fixture := newPostgresWorkerRevocationRaceFixture(t, "worker-revoke-renew-race")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var revokeErr, renewErr error
	go func() {
		defer wait.Done()
		<-start
		_, revokeErr = fixture.services[0].RevokeWorker(
			ctx, fixture.principal, fixture.execution.TenantID, fixture.worker.ID,
			RevokeWorkerInput{ExpectedIncarnation: fixture.worker.Incarnation, Reason: "concurrent lease renewal fencing"},
			"worker-revoke-renew-"+uuid.NewString(), "worker-revoke-renew", "127.0.0.1",
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, renewErr = fixture.services[1].Renew(
			ctx, fixture.worker, fixture.execution.ExecutionID,
			RenewLeaseInput{LeaseInput: fixture.leaseInput}, "worker-renew-race-"+uuid.NewString(),
		)
	}()
	close(start)
	wait.Wait()

	if revokeErr != nil {
		t.Fatalf("concurrent Worker revocation failed: %v", revokeErr)
	}
	assertWorkerRaceError(t, renewErr, "worker_token_revoked")
	assertPostgresRevokedWorkerRaceState(t, fixture, "recovering")
}

func TestPostgresWorkerRevocationLinearizesWithExecutionCompletion(t *testing.T) {
	fixture := newPostgresWorkerRevocationRaceFixture(t, "worker-revoke-complete-race")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var revokeErr, completeErr error
	go func() {
		defer wait.Done()
		<-start
		_, revokeErr = fixture.services[0].RevokeWorker(
			ctx, fixture.principal, fixture.execution.TenantID, fixture.worker.ID,
			RevokeWorkerInput{ExpectedIncarnation: fixture.worker.Incarnation, Reason: "concurrent execution completion fencing"},
			"worker-revoke-complete-"+uuid.NewString(), "worker-revoke-complete", "127.0.0.1",
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, completeErr = fixture.services[1].Complete(
			ctx, fixture.worker, fixture.execution.ExecutionID,
			CompleteExecutionInput{LeaseInput: fixture.leaseInput, Output: map[string]any{"race": "complete"}},
			"worker-complete-race-"+uuid.NewString(),
		)
	}()
	close(start)
	wait.Wait()

	if revokeErr != nil {
		t.Fatalf("concurrent Worker revocation failed: %v", revokeErr)
	}
	assertWorkerRaceError(t, completeErr, "worker_token_revoked")
	assertPostgresRevokedWorkerRaceState(t, fixture, "recovering", "completed")
}

func TestPostgresSSHTargetRevocationLinearizesWithLeaseRenewal(t *testing.T) {
	fixture := newPostgresSSHTargetRevocationRaceFixture(t, "ssh-target-revoke-renew-race")
	const operationGeneration int64 = 11
	now := time.Now().UTC()
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.execution.TargetID).
		Updates(map[string]any{
			"status": "offline", "ssh_operation_generation": operationGeneration,
			"ssh_operation_kind": "revoke", "ssh_operation_started_at": now,
		}).Error; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var revokeErr, renewErr error
	go func() {
		defer wait.Done()
		<-start
		revokeErr = fixture.services[0].RevokeExecutionTargetWorkers(
			ctx, fixture.principal, fixture.execution.TenantID, fixture.execution.TargetID,
			operationGeneration, "concurrent SSH Target lease renewal fencing",
			"ssh-target-revoke-renew", "127.0.0.1",
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, renewErr = fixture.services[1].Renew(
			ctx, fixture.worker, fixture.execution.ExecutionID,
			RenewLeaseInput{LeaseInput: fixture.leaseInput}, "ssh-target-renew-race-"+uuid.NewString(),
		)
	}()
	close(start)
	wait.Wait()

	if revokeErr != nil {
		t.Fatalf("concurrent SSH Target Worker revocation failed: %v", revokeErr)
	}
	// Renewal may linearize immediately before revocation and succeed, or after
	// revocation and be fenced. Either order is valid; the durable final state
	// below must always have revoked authority and no surviving lease.
	assertWorkerRaceError(t, renewErr, "worker_token_revoked")
	assertPostgresRevokedWorkerRaceState(t, fixture, "recovering")
}

func TestPostgresSSHHeartbeatScopeLinearizesWithOperationFinish(t *testing.T) {
	fixture := newPostgresSSHTargetRevocationRaceFixture(t, "ssh-heartbeat-finish-race")
	if fixture.worker.SSHBootstrapGeneration == nil {
		t.Fatal("SSH Worker did not persist its bootstrap generation")
	}
	generation := *fixture.worker.SSHBootstrapGeneration
	startedAt := time.Now().UTC()
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.execution.TargetID).
		Updates(map[string]any{
			"status": "offline", "ssh_operation_generation": generation,
			"ssh_operation_kind": "install", "ssh_operation_started_at": startedAt,
			"ssh_expected_instance_uid": uuid.MustParse(fixture.worker.InstanceUID),
		}).Error; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	heartbeatScope := fixture.db.Begin()
	if heartbeatScope.Error != nil {
		t.Fatal(heartbeatScope.Error)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = heartbeatScope.Rollback().Error
		}
	})
	if _, _, err := fixture.services[0].targets.ResolveWorkerBootstrapTargetInTransaction(
		ctx, heartbeatScope, fixture.execution.TargetID, "ssh", fixture.worker.InstanceUID, &generation,
	); err != nil {
		t.Fatal(err)
	}

	finishDone := make(chan error, 1)
	go func() {
		finishDone <- fixture.db.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where(
				"id = ? AND status = ? AND ssh_operation_generation = ? AND ssh_operation_kind = ? AND ssh_expected_instance_uid = ?",
				fixture.execution.TargetID, "offline", generation, "install", fixture.worker.InstanceUID,
			).
			Updates(map[string]any{
				"status": "active", "ssh_operation_kind": nil, "ssh_operation_started_at": nil,
				"ssh_expected_instance_uid": nil,
			}).Error
	}()
	select {
	case err := <-finishDone:
		t.Fatalf("operation finish bypassed heartbeat Target lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := heartbeatScope.Commit().Error; err != nil {
		t.Fatal(err)
	}
	committed = true
	select {
	case err := <-finishDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("operation finish remained blocked after heartbeat scope committed")
	}
	if _, err := fixture.services[0].Heartbeat(ctx, fixture.worker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, SSHBootstrapGeneration: &generation,
	}); err != nil {
		t.Fatalf("active SSH Worker heartbeat after operation finish: %v", err)
	}
}

func TestPostgresActiveSSHWorkerBootstrapIdentityIsImmutable(t *testing.T) {
	fixture := newPostgresSSHTargetRevocationRaceFixture(t, "ssh-active-bootstrap-immutable")
	if fixture.worker.SSHBootstrapGeneration == nil {
		t.Fatal("SSH Worker did not persist its bootstrap generation")
	}
	wrongGeneration := *fixture.worker.SSHBootstrapGeneration + 1
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.WorkerInstance{}).Where("id = ?", fixture.worker.ID).
			Update("ssh_bootstrap_generation", wrongGeneration).Error
	}); err == nil {
		t.Fatal("active SSH Worker bootstrap generation changed without an offline operation fence")
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.WorkerInstance{}).Where("id = ?", fixture.worker.ID).
			Update("instance_uid", uuid.NewString()).Error
	}); err == nil {
		t.Fatal("active SSH Worker instance UID changed without an offline operation fence")
	}
	var current persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", fixture.worker.ID).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.InstanceUID != fixture.worker.InstanceUID ||
		!equalOptionalInt64(current.SSHBootstrapGeneration, fixture.worker.SSHBootstrapGeneration) {
		t.Fatalf("rejected active SSH identity mutation changed durable state: %#v", current)
	}
}

func TestPostgresAtomicSSHRevokeCommitsAuthorityBeforeDecryptFailure(t *testing.T) {
	fixture := newPostgresSSHTargetRevocationRaceFixture(t, "ssh-atomic-revoke-decrypt-failure")
	provisioner := executiontargets.NewSSHProvisioner(
		fixture.services[0].targets,
		executiontargets.SSHProvisioningConfig{},
	)
	provisioner.SetWorkerAuthorityRevoker(fixture.services[0].RevokeExecutionTargetWorkersInTransaction)
	_, err := provisioner.Revoke(
		context.Background(), fixture.principal, fixture.execution.TenantID, fixture.execution.TargetID,
		"ssh-atomic-revoke-decrypt-failure", "127.0.0.1",
	)
	assertWorkerRevocationProblem(t, err, 409, "ssh_configuration_missing")

	_, err = fixture.services[0].Renew(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		RenewLeaseInput{LeaseInput: fixture.leaseInput}, "ssh-atomic-renew-after-revoke",
	)
	assertWorkerRevocationProblem(t, err, 401, "worker_token_revoked")
	_, err = fixture.services[0].Start(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		fixture.leaseInput, "ssh-atomic-start-after-revoke",
	)
	assertWorkerRevocationProblem(t, err, 401, "worker_token_revoked")
	_, err = fixture.services[0].Complete(
		context.Background(), fixture.worker, fixture.execution.ExecutionID,
		CompleteExecutionInput{LeaseInput: fixture.leaseInput}, "ssh-atomic-complete-after-revoke",
	)
	assertWorkerRevocationProblem(t, err, 401, "worker_token_revoked")
	assertPostgresRevokedWorkerRaceState(t, fixture, "recovering")
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.execution.TargetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "offline" || target.SSHOperationKind != nil || target.SSHExpectedInstanceUID != nil {
		t.Fatalf("decrypt failure did not leave committed revoke authority offline: %#v", target)
	}
}

type postgresWorkerRevocationRaceFixture struct {
	db         *gorm.DB
	services   [2]*Service
	execution  executionFixture
	worker     persistence.WorkerInstance
	leaseInput LeaseInput
	principal  identity.Principal
}

func newPostgresWorkerRevocationRaceFixture(t *testing.T, podName string) postgresWorkerRevocationRaceFixture {
	t.Helper()
	db := integrationDB(t)
	execution := seedExecutionFixture(t, db)
	services := [2]*Service{integrationService(t, db), integrationService(t, db)}
	worker := registerManifestTestWorker(t, services[0], execution.TargetID, execution.TargetKind, podName)
	cleanupWorkers(t, db, worker.ID)
	claim, err := services[0].Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind, ExecutionID: &execution.ExecutionID,
	}, "worker-revocation-race-claim-"+uuid.NewString())
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim Worker revocation race Execution: %#v, %v", claim, err)
	}
	return postgresWorkerRevocationRaceFixture{
		db: db, services: services, execution: execution, worker: worker,
		leaseInput: LeaseInput{
			TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
		principal: identity.Principal{UserID: execution.UserID, ActiveTenantID: &execution.TenantID},
	}
}

func newPostgresSSHTargetRevocationRaceFixture(t *testing.T, podName string) postgresWorkerRevocationRaceFixture {
	t.Helper()
	db := integrationDB(t)
	execution := seedExecutionFixtureWithoutCleanup(t, db)
	t.Cleanup(func() {
		if err := cleanupImmutableExecutionFacts(db, execution.TenantID); err != nil {
			t.Errorf("cleanup SSH Target immutable execution facts: %v", err)
			return
		}
		if err := cleanupFixture(db, execution.TenantID); err != nil {
			t.Errorf("cleanup SSH Target revocation race fixture: %v", err)
		}
	})
	var originalTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", execution.TargetID).Take(&originalTarget).Error; err != nil {
		t.Fatal(err)
	}
	sshTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &execution.TenantID, OrganizationID: originalTarget.OrganizationID,
		Kind: "ssh", Name: "ssh-revocation-race-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: originalTarget.Capabilities,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&sshTarget).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
			Update("execution_target_id", sshTarget.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", execution.TenantID, execution.ExecutionID).
			Updates(map[string]any{"execution_target_id": sshTarget.ID, "target_kind": "ssh"}).Error
	}); err != nil {
		t.Fatalf("seed SSH Target scope: %v", err)
	}
	execution.TargetID = sshTarget.ID
	execution.TargetKind = "ssh"
	services := [2]*Service{integrationService(t, db), integrationService(t, db)}
	const bootstrapGeneration int64 = 1
	bootstrapInstanceUID := uuid.New()
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", sshTarget.ID).
		Updates(map[string]any{
			"status": "offline", "ssh_operation_generation": bootstrapGeneration,
			"ssh_operation_kind": "install", "ssh_operation_started_at": time.Now().UTC(),
			"ssh_expected_instance_uid": bootstrapInstanceUID,
		}).Error; err != nil {
		t.Fatalf("start SSH bootstrap operation: %v", err)
	}
	worker := registerSSHBootstrapManifestTestWorker(
		t, services[0], execution.TargetID, bootstrapInstanceUID, bootstrapGeneration, podName,
	)
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", sshTarget.ID).
		Updates(map[string]any{
			"status": "active", "ssh_operation_kind": nil, "ssh_operation_started_at": nil,
			"ssh_expected_instance_uid": nil,
		}).Error; err != nil {
		t.Fatalf("activate SSH bootstrap Target: %v", err)
	}
	cleanupWorkers(t, db, worker.ID)
	claim, err := services[0].Claim(context.Background(), worker, ClaimExecutionInput{
		ExecutionTargetID: execution.TargetID, TargetKind: execution.TargetKind, ExecutionID: &execution.ExecutionID,
	}, "ssh-target-revocation-race-claim-"+uuid.NewString())
	if err != nil || claim.Value.Lease == nil {
		t.Fatalf("claim SSH Target revocation race Execution: %#v, %v", claim, err)
	}
	return postgresWorkerRevocationRaceFixture{
		db: db, services: services, execution: execution, worker: worker,
		leaseInput: LeaseInput{
			TenantID: execution.TenantID, Generation: claim.Value.Lease.Generation,
			LeaseToken: claim.Value.Lease.LeaseToken,
		},
		principal: identity.Principal{UserID: execution.UserID, ActiveTenantID: &execution.TenantID},
	}
}

func registerSSHBootstrapManifestTestWorker(
	t *testing.T,
	service *Service,
	targetID, instanceUID uuid.UUID,
	generation int64,
	podName string,
) persistence.WorkerInstance {
	t.Helper()
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	fullPodName := podName + "-" + uuid.NewString()
	signWorkerManifestTestContainment(t, capabilities, workerManifestRegistrationContext{
		ExecutionTargetID: targetID, TargetKind: platform.TargetSSH, InstanceUID: instanceUID.String(),
		ClusterID: "test-cluster", Namespace: "default", PodName: fullPodName,
	})
	registered, err := service.Register(context.Background(), RegisterWorkerInput{
		ExecutionTargetID: targetID, TargetKind: "ssh", InstanceUID: instanceUID.String(),
		SSHBootstrapGeneration: &generation,
		ClusterID:              "test-cluster", Namespace: "default", PodName: fullPodName,
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion, Capabilities: capabilities,
		LeaseSupported: true, FencingSupported: true,
	})
	if err != nil {
		t.Fatalf("register exact SSH bootstrap Worker: %v", err)
	}
	worker, err := service.Authenticate(context.Background(), registered.Token)
	if err != nil {
		t.Fatalf("authenticate exact SSH bootstrap Worker: %v", err)
	}
	return worker
}

func cleanupImmutableExecutionFacts(db *gorm.DB, tenantID uuid.UUID) error {
	return db.Transaction(func(tx *gorm.DB) error {
		tables := []struct {
			table   string
			trigger string
		}{
			{table: "execution_recovery_bundles", trigger: "trg_execution_recovery_bundles_immutable"},
			{table: "worker_claim_facts", trigger: "trg_worker_claim_facts_immutable"},
			{table: "execution_generation_facts", trigger: "trg_execution_generation_facts_update"},
			{table: "worker_incarnation_facts", trigger: "trg_worker_incarnation_facts_update"},
		}
		for _, item := range tables {
			if err := tx.Exec("ALTER TABLE " + item.table + " DISABLE TRIGGER " + item.trigger).Error; err != nil {
				return err
			}
			if err := tx.Exec("DELETE FROM "+item.table+" WHERE tenant_id = ?", tenantID).Error; err != nil {
				return err
			}
			if err := tx.Exec("ALTER TABLE " + item.table + " ENABLE TRIGGER " + item.trigger).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func assertWorkerRaceError(t *testing.T, err error, allowedCodes ...string) {
	t.Helper()
	if err == nil {
		return
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		t.Fatalf("concurrent Worker request returned non-problem error: %v", err)
	}
	for _, code := range allowedCodes {
		if apiError.Code == code {
			return
		}
	}
	t.Fatalf("concurrent Worker request problem code = %q, want one of %v", apiError.Code, allowedCodes)
}

func assertPostgresRevokedWorkerRaceState(
	t *testing.T,
	fixture postgresWorkerRevocationRaceFixture,
	allowedStatuses ...string,
) {
	t.Helper()
	var worker persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", fixture.worker.ID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.AdministrativeStatus != "revoked" || worker.RevokedAt == nil || worker.RevokedBy == nil {
		t.Fatalf("Worker race did not persist revocation: %#v", worker)
	}
	var leaseCount int64
	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID).
		Count(&leaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("Worker revocation race retained %d execution Leases", leaseCount)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.execution.TenantID, fixture.execution.ExecutionID,
	).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	allowed := false
	for _, status := range allowedStatuses {
		if execution.Status == status {
			allowed = true
			break
		}
	}
	if !allowed {
		t.Fatalf("Worker revocation race left Execution status %q, want one of %v", execution.Status, allowedStatuses)
	}
	if execution.Status == "recovering" && execution.WorkerID != nil {
		t.Fatalf("recovering Execution retained revoked Worker ownership: %#v", execution)
	}
}

package database

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresSSHBootstrapAuthorityUpgradesSchema71StateAndFencesIdentity(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000071_ssh_target_operation_fencing.sql")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	userID := uuid.New()
	tenantID := uuid.New()
	user := persistence.User{
		ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "SSH migration owner",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Select(
		"id", "email", "display_name", "status", "email_verified_at", "created_at", "updated_at",
	).Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	tenant := persistence.Tenant{
		ID: tenantID, Slug: "ssh-migration-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
		Name: "SSH migration tenant", Status: "active", PlanCode: "free", Region: "default",
		Settings: map[string]any{}, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Select(
			"id", "slug", "name", "status", "plan_code", "region", "settings", "created_by", "created_at", "updated_at",
		).Create(&tenant).Error; err != nil {
			return err
		}
		return tx.Select(
			"tenant_id", "user_id", "role", "status", "joined_at", "created_at", "updated_at",
		).Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}

	activeTarget := createSchema71SSHTarget(t, db, tenantID, "active", "active", 5, nil, nil, now)
	installKind := "install"
	installTarget := createSchema71SSHTarget(t, db, tenantID, "install", "offline", 7, &installKind, &now, now)
	upgradeKind := "upgrade"
	upgradeTarget := createSchema71SSHTarget(t, db, tenantID, "upgrade", "offline", 8, &upgradeKind, &now, now)
	revokeTarget := createSchema71SSHTarget(t, db, tenantID, "revoke", "active", 9, nil, nil, now)

	activeWorker := createSchema71SSHWorker(t, db, activeTarget.ID, "active", now)
	revokeWorker := createSchema71SSHWorker(t, db, revokeTarget.ID, "revoke", now)
	revokeKind := "revoke"
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", revokeTarget.ID).Updates(map[string]any{
		"status": "offline", "ssh_operation_kind": revokeKind, "ssh_operation_started_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	assertSSHMigrationTargetState(t, db, activeTarget.ID, "active", 5, nil, false)
	assertSSHMigrationTargetState(t, db, installTarget.ID, "offline", 7, nil, false)
	assertSSHMigrationTargetState(t, db, upgradeTarget.ID, "offline", 8, nil, false)
	assertSSHMigrationTargetState(t, db, revokeTarget.ID, "offline", 9, &revokeKind, false)

	for _, workerID := range []uuid.UUID{activeWorker.ID, revokeWorker.ID} {
		var worker persistence.WorkerInstance
		if err := db.Where("id = ?", workerID).Take(&worker).Error; err != nil {
			t.Fatal(err)
		}
		if worker.SSHBootstrapGeneration != nil {
			t.Fatalf("schema-71 legacy Worker %s bootstrap generation = %v, want nil", workerID, *worker.SSHBootstrapGeneration)
		}
	}

	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", activeWorker.ID).
		Update("ssh_bootstrap_generation", int64(5)).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("active legacy SSH Worker generation mutation = %v, want check violation", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", activeWorker.ID).
		Update("instance_uid", uuid.NewString()).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("active legacy SSH Worker UID mutation = %v, want check violation", err)
	}

	expectedInstanceUID := uuid.New()
	bootstrapGeneration := int64(10)
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", installTarget.ID).Updates(map[string]any{
		"status": "offline", "ssh_operation_generation": bootstrapGeneration,
		"ssh_operation_kind": installKind, "ssh_operation_started_at": now,
		"ssh_expected_instance_uid": expectedInstanceUID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	wrongGeneration := bootstrapGeneration + 1
	wrongGenerationWorker := schema73SSHWorker(
		installTarget.ID, "wrong-generation", expectedInstanceUID.String(), &wrongGeneration, now,
	)
	if err := db.Create(&wrongGenerationWorker).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("wrong-generation SSH bootstrap Worker = %v, want check violation", err)
	}
	wrongUIDWorker := schema73SSHWorker(
		installTarget.ID, "wrong-uid", uuid.NewString(), &bootstrapGeneration, now,
	)
	if err := db.Create(&wrongUIDWorker).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("wrong-UID SSH bootstrap Worker = %v, want check violation", err)
	}
	exactWorker := schema73SSHWorker(installTarget.ID, "exact", expectedInstanceUID.String(), &bootstrapGeneration, now)
	if err := db.Create(&exactWorker).Error; err != nil {
		t.Fatalf("exact schema-73 SSH bootstrap Worker: %v", err)
	}

	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", exactWorker.ID).
		Update("ssh_bootstrap_generation", wrongGeneration).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("generation-only SSH bootstrap mutation = %v, want check violation", err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", exactWorker.ID).
		Update("instance_uid", uuid.NewString()).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("UID-only SSH bootstrap mutation = %v, want check violation", err)
	}

	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", installTarget.ID).Updates(map[string]any{
		"status": "active", "ssh_operation_kind": nil, "ssh_operation_started_at": nil,
		"ssh_expected_instance_uid": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", exactWorker.ID).
		Update("instance_uid", uuid.NewString()).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
		t.Fatalf("active exact SSH Worker UID mutation = %v, want check violation", err)
	}

	revocationReason := "schema-73 migration revocation"
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", revokeWorker.ID).Updates(map[string]any{
		"administrative_status": "revoked", "revoked_at": now, "revoked_by": userID,
		"revocation_reason": revocationReason,
	}).Error; err != nil {
		t.Fatalf("schema-73 trigger blocked offline revoke authority withdrawal: %v", err)
	}

	for _, version := range []int64{71, 72, 73} {
		var count int64
		if err := db.Table("control_plane_schema_migrations").Where("version = ?", version).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("migration %d applied records = %d, want 1", version, count)
		}
	}
}

func createSchema71SSHTarget(
	t *testing.T,
	db *gorm.DB,
	tenantID uuid.UUID,
	name, status string,
	generation int64,
	operationKind *string,
	operationStartedAt *time.Time,
	now time.Time,
) persistence.ExecutionTarget {
	t.Helper()
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantID, Kind: "ssh", Name: "ssh-migration-" + name, Status: status,
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, SSHOperationGeneration: generation,
		SSHOperationKind: operationKind, SSHOperationStartedAt: operationStartedAt, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Select(
		"id", "tenant_id", "organization_id", "kind", "name", "status", "configuration_encrypted", "capabilities",
		"ssh_operation_generation", "ssh_operation_kind", "ssh_operation_started_at", "created_at", "updated_at",
	).Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func createSchema71SSHWorker(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	identity string,
	now time.Time,
) persistence.WorkerInstance {
	t.Helper()
	worker := schema73SSHWorker(targetID, identity, uuid.NewString(), nil, now)
	if err := db.Select(
		"id", "incarnation", "instance_uid", "execution_target_id", "target_kind", "worker_mode",
		"registration_trust_mode", "cluster_id", "namespace", "pod_name", "version", "protocol_version",
		"capabilities", "compatibility_status", "worker_release_status", "lease_supported", "fencing_supported",
		"auth_token_hash", "status", "administrative_status", "registered_at", "last_heartbeat_at",
	).Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	return worker
}

func schema73SSHWorker(
	targetID uuid.UUID,
	identity, instanceUID string,
	bootstrapGeneration *int64,
	now time.Time,
) persistence.WorkerInstance {
	return persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: instanceUID, SSHBootstrapGeneration: bootstrapGeneration,
		ExecutionTargetID: targetID, TargetKind: "ssh", WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: "migration-cluster", Namespace: "default",
		PodName: "ssh-migration-" + identity, Version: "migration", ProtocolVersion: 2,
		Capabilities: map[string]any{}, CompatibilityStatus: "unknown", WorkerReleaseStatus: "unmanaged",
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("migration-token-" + identity),
		Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}
}

func assertSSHMigrationTargetState(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	expectedStatus string,
	expectedGeneration int64,
	expectedOperationKind *string,
	expectInstanceUID bool,
) {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != expectedStatus || target.SSHOperationGeneration != expectedGeneration ||
		!equalOptionalMigrationString(target.SSHOperationKind, expectedOperationKind) ||
		(target.SSHOperationStartedAt != nil) != (expectedOperationKind != nil) ||
		(target.SSHExpectedInstanceUID != nil) != expectInstanceUID {
		t.Fatalf("SSH migration Target state = %#v", target)
	}
}

func equalOptionalMigrationString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

package database

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteSemanticCredentialAccessEnforcesWatermarkAndLeaseAccessBundle(t *testing.T) {
	fixture := openStage4SQLiteFixture(t)

	for _, name := range []string{
		"idx_worker_leases_provider_credential_access_expiry",
		"idx_worker_leases_provider_credential_refresh_deadline",
		"trg_worker_leases_provider_credential_access_insert",
		"trg_worker_leases_provider_credential_access_update",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite semantic credential access object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	invalidSession := persistence.AgentSession{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
		CreatedBy: fixture.userID, Title: "Invalid activity watermark", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: fixture.targetID, ProviderResumeCursorState: "absent",
		LastEventSequence: 1, MeaningfulActivitySequence: 2,
		ResourceState: "idle", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800,
		WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&invalidSession).Error,
		"Session Resource Lifecycle fields are invalid",
	)

	sessionID := fixture.createSession(t, fixture.tenantID, fixture.organizationID, fixture.projectID, "Semantic access", now)
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.MeaningfulActivitySequence != 0 {
		t.Fatalf("new SQLite Session meaningful activity watermark = %d, want 0", session.MeaningfulActivitySequence)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).
		Updates(map[string]any{"last_event_sequence": 5, "meaningful_activity_sequence": 3}).Error; err != nil {
		t.Fatalf("update valid Session meaningful activity watermark: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, sessionID).
			Update("meaningful_activity_sequence", 6).Error,
		"Session Resource Lifecycle fields are invalid",
	)

	turnID := fixture.createTurn(t, fixture.tenantID, sessionID, now)
	workerID, workerInstanceUID := fixture.createWorker(t, "sqlite-semantic-access", now)
	executionID := fixture.createExecution(t, sessionID, turnID, workerID, 1, "waiting-for-approval", now)
	lease := persistence.WorkerLease{
		ExecutionID: executionID, TenantID: fixture.tenantID, WorkerID: workerID,
		WorkerIncarnation: 1, WorkerInstanceUID: workerInstanceUID, Generation: 1,
		LeaseTokenHash: []byte("semantic-lease-token"), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := fixture.db.Create(&lease).Error; err != nil {
		t.Fatalf("create valid Worker Lease: %v", err)
	}

	credential := sqliteWorkspaceCredential(
		fixture.tenantID, fixture.organizationID, fixture.userID, "provider", "codex", "api_key", now,
	)
	if err := fixture.db.Create(&credential).Error; err != nil {
		t.Fatalf("create Provider Credential: %v", err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, executionID).
		Updates(map[string]any{
			"provider_credential_id_snapshot":      credential.ID,
			"provider_credential_version_snapshot": credential.Version,
			"provider":                             "codex",
		}).Error; err != nil {
		t.Fatalf("bind Execution Provider Credential snapshot: %v", err)
	}

	grantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: grantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
		CredentialID: credential.ID, CredentialVersion: credential.Version, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create valid Execution Provider Credential Grant: %v", err)
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, executionID).
		Update("generation", 2).Error; err != nil {
		t.Fatalf("bump Execution generation to seed mismatched grant: %v", err)
	}
	mismatchedGrantID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionProviderCredentialGrant{
		ID: mismatchedGrantID, TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 2,
		CredentialID: credential.ID, CredentialVersion: credential.Version, CreatedAt: now.Add(time.Second),
	}).Error; err != nil {
		t.Fatalf("create mismatched Execution Provider Credential Grant: %v", err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, executionID).
		Update("generation", 1).Error; err != nil {
		t.Fatalf("restore Execution generation: %v", err)
	}

	issuedAt := now.Add(2 * time.Second)
	activityAt := now.Add(3 * time.Second)
	renewedAt := now.Add(4 * time.Second)
	expiresAt := now.Add(10 * time.Minute)
	refreshDeadlineAt := now.Add(8 * time.Minute)
	hardExpiresAt := now.Add(30 * time.Minute)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_grant_id":            mismatchedGrantID,
				"provider_credential_access_serial":       1,
				"provider_credential_activity_sequence":   3,
				"provider_credential_activity_at":         activityAt,
				"provider_credential_access_issued_at":    issuedAt,
				"provider_credential_access_renewed_at":   renewedAt,
				"provider_credential_access_expires_at":   expiresAt,
				"provider_credential_refresh_deadline_at": refreshDeadlineAt,
				"provider_credential_hard_expires_at":     hardExpiresAt,
			}).Error,
		"frozen Execution Provider Credential Grant",
	)

	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
		Updates(map[string]any{
			"provider_credential_grant_id":            grantID,
			"provider_credential_access_serial":       1,
			"provider_credential_activity_sequence":   3,
			"provider_credential_activity_at":         activityAt,
			"provider_credential_access_issued_at":    issuedAt,
			"provider_credential_access_renewed_at":   renewedAt,
			"provider_credential_access_expires_at":   expiresAt,
			"provider_credential_refresh_deadline_at": refreshDeadlineAt,
			"provider_credential_hard_expires_at":     hardExpiresAt,
		}).Error; err != nil {
		t.Fatalf("attach valid Worker Lease Provider Credential access: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Update("provider_credential_access_issued_at", issuedAt.Add(time.Minute)).Error,
		"issued-at is immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Update("provider_credential_grant_id", mismatchedGrantID).Error,
		"Grant is immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":     4,
				"provider_credential_activity_sequence": 4,
				"provider_credential_activity_at":       activityAt.Add(time.Minute),
			}).Error,
		"serial must advance exactly once per state update",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":       2,
				"provider_credential_refresh_deadline_at": activityAt.Add(-time.Second),
			}).Error,
		"access fields are invalid",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":     2,
				"provider_credential_activity_sequence": 4,
				"provider_credential_activity_at":       activityAt.Add(-time.Second),
			}).Error,
		"activity_at cannot go backwards",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":   2,
				"provider_credential_hard_expires_at": hardExpiresAt.Add(time.Minute),
			}).Error,
		"hard expiry cannot be extended",
	)

	secondRenewedAt := renewedAt.Add(time.Minute)
	secondExpiresAt := expiresAt.Add(time.Minute)
	secondRefreshDeadlineAt := refreshDeadlineAt.Add(time.Minute)
	secondActivityAt := activityAt.Add(time.Minute)
	if err := fixture.db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
		Updates(map[string]any{
			"provider_credential_access_serial":       2,
			"provider_credential_activity_sequence":   4,
			"provider_credential_activity_at":         secondActivityAt,
			"provider_credential_access_renewed_at":   secondRenewedAt,
			"provider_credential_access_expires_at":   secondExpiresAt,
			"provider_credential_refresh_deadline_at": secondRefreshDeadlineAt,
		}).Error; err != nil {
		t.Fatalf("renew valid Worker Lease Provider Credential access: %v", err)
	}

	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":     3,
				"provider_credential_activity_sequence": 4,
				"provider_credential_activity_at":       secondActivityAt.Add(time.Minute),
			}).Error,
		"activity_at must stay frozen without a newer activity sequence",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).
			Updates(map[string]any{
				"provider_credential_access_serial":       3,
				"provider_credential_access_renewed_at":   renewedAt,
				"provider_credential_access_expires_at":   expiresAt,
				"provider_credential_refresh_deadline_at": refreshDeadlineAt,
			}).Error,
		"renewal windows cannot move backwards",
	)

	if err := fixture.db.Delete(&persistence.WorkerLease{},
		"tenant_id = ? AND execution_id = ?", fixture.tenantID, executionID).Error; err != nil {
		t.Fatalf("delete Worker Lease with semantic credential access fields: %v", err)
	}
}

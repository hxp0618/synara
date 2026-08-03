package legalholds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSessionLegalHoldGatesRetentionArtifactDeletionAndTenantDeletion(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "legal-hold-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	now := time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	projectID, sessionID, artifactID := uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Held project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Held session", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
			CreatedAt: old, UpdatedAt: old,
		},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	objectStore, err := artifacts.NewLocalStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	payload := "held retention artifact"
	objectKey := domain.TenantID.String() + "/held.txt"
	if _, err := objectStore.Put(ctx, objectKey, strings.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(payload))
	digestText := hex.EncodeToString(digest[:])
	size := int64(len(payload))
	contentType := "text/plain"
	if err := store.DB().Create(&persistence.Artifact{
		ID: artifactID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, Kind: "generated_file", Status: "ready",
		Bucket: objectStore.Bucket(), ObjectKey: objectKey, ContentType: &contentType,
		SizeBytes: &size, SHA256: &digestText, CreatedByType: "user", CreatedByID: domain.UserID,
		ReadyAt: &old, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}

	targets := executiontargets.NewService(store.DB(), platformConfig, nil)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targets)
	artifactService := artifacts.NewService(store.DB(), objectStore, config.Config{}, nil, sessionService)
	service := NewService(store.DB())
	service.now = func() time.Time { return now }
	otherTenant, err := tenancy.NewService(store.DB()).CreateTenant(ctx, principal, tenancy.CreateTenantInput{
		Slug: "legal-hold-other-" + uuid.NewString()[:8], Name: "Legal Hold Other", PlanCode: "free", Status: "active",
	}, "legal-hold-other-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var otherOrganization persistence.Organization
	if err := store.DB().Where("tenant_id = ? AND kind = ?", otherTenant.ID, "root").Take(&otherOrganization).Error; err != nil {
		t.Fatal(err)
	}
	otherTargetID, otherProjectID, otherSessionID := uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.ExecutionTarget{
			ID: otherTargetID, TenantID: &otherTenant.ID, OrganizationID: &otherOrganization.ID,
			Kind: "local", Name: "Legal Hold Other", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Project{
			ID: otherProjectID, TenantID: otherTenant.ID, OrganizationID: otherOrganization.ID,
			Name: "Other held project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		},
		&persistence.AgentSession{
			ID: otherSessionID, TenantID: otherTenant.ID, OrganizationID: otherOrganization.ID,
			ProjectID: otherProjectID, CreatedBy: domain.UserID, Title: "Other held session", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: otherTargetID,
		},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed cross-Tenant Legal Hold %T: %v", model, err)
		}
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		ScopeType: "session", ScopeID: otherSessionID, Name: "Cross-Tenant scope",
		MatterReference: "MAT-CROSS-TENANT", Reason: "A Legal Hold must not bind a Session owned by another Tenant.",
	}, "legal-hold-cross-tenant", "127.0.0.1"); problemCode(err) != "legal_hold_create_rejected" {
		t.Fatalf("cross-Tenant Legal Hold scope err = %v", err)
	}
	hold, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		ScopeType: "session", ScopeID: sessionID, Name: "Incident preservation",
		MatterReference: "MAT-2026-001", Reason: "Preserve the affected Session during the open investigation.",
	}, "legal-hold-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if hold.Status != "active" || hold.Version != 1 || hold.ScopeID != sessionID {
		t.Fatalf("unexpected active Legal Hold: %#v", hold)
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		ScopeType: "session", ScopeID: sessionID, Name: "Duplicate matter",
		MatterReference: "mat-2026-001", Reason: "A duplicate active matter must be rejected by the database.",
	}, "legal-hold-duplicate", "127.0.0.1"); problemCode(err) != "legal_hold_create_rejected" {
		t.Fatalf("duplicate active Legal Hold err = %v", err)
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		ScopeType: "session", ScopeID: uuid.New(), Name: "Foreign scope",
		MatterReference: "MAT-2026-002", Reason: "An unknown Session scope must be rejected by the database.",
	}, "legal-hold-invalid-scope", "127.0.0.1"); problemCode(err) != "legal_hold_create_rejected" {
		t.Fatalf("invalid Legal Hold scope err = %v", err)
	}
	if err := store.DB().Model(&persistence.LegalHold{}).Where("id = ?", hold.ID).
		Update("reason", "Direct mutation must fail.").Error; err == nil {
		t.Fatal("database allowed direct Legal Hold mutation")
	}

	archived, err := sessionService.ArchiveByRetention(ctx, domain.TenantID, now.Add(-24*time.Hour), now, nil, 10)
	if err != nil || archived != 0 {
		t.Fatalf("held Session retention = %d, %v, want 0", archived, err)
	}
	deleted, err := artifactService.DeleteByRetention(ctx, domain.TenantID, now.Add(-24*time.Hour), now, 10)
	if err != nil || deleted != 0 {
		t.Fatalf("held Artifact retention = %d, %v, want 0", deleted, err)
	}
	deleteErr := tenancy.NewService(store.DB()).RequestTenantDeletion(ctx, principal, domain.TenantID, tenancy.DeleteTenantInput{
		ExpectedVersion: 1, Reason: "Deletion must remain blocked while the legal matter is open.",
	}, "tenant-delete-held", "127.0.0.1")
	if problemCode(deleteErr) != "tenant_legal_hold_active" {
		t.Fatalf("held Tenant deletion err = %v", deleteErr)
	}

	if _, err := service.Release(ctx, principal, domain.TenantID, hold.ID, ReleaseInput{
		ExpectedVersion: 2, Reason: "A stale release version must fail without changing the hold.",
	}, "legal-hold-stale-release", "127.0.0.1"); problemCode(err) != "legal_hold_version_conflict" {
		t.Fatalf("stale Legal Hold release err = %v", err)
	}
	released, err := service.Release(ctx, principal, domain.TenantID, hold.ID, ReleaseInput{
		ExpectedVersion: 1, Reason: "Counsel confirmed the matter is closed and preservation may end.",
	}, "legal-hold-release", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "released" || released.Version != 2 || released.ReleasedAt == nil {
		t.Fatalf("unexpected released Legal Hold: %#v", released)
	}

	archived, err = sessionService.ArchiveByRetention(ctx, domain.TenantID, now.Add(-24*time.Hour), now, nil, 10)
	if err != nil || archived != 1 {
		t.Fatalf("released Session retention = %d, %v, want 1", archived, err)
	}
	deleted, err = artifactService.DeleteByRetention(ctx, domain.TenantID, now.Add(-24*time.Hour), now, 10)
	if err != nil || deleted != 1 {
		t.Fatalf("released Artifact retention = %d, %v, want 1", deleted, err)
	}
	var artifact persistence.Artifact
	if err := store.DB().Where("id = ?", artifactID).Take(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if artifact.Status != "deleted" || artifact.DeletedAt == nil {
		t.Fatalf("released Artifact was not deleted: %#v", artifact)
	}
	items, err := service.List(ctx, principal, domain.TenantID)
	if err != nil || len(items) != 1 || items[0].Status != "released" {
		t.Fatalf("Legal Hold history = %#v, %v", items, err)
	}
	if err := tenancy.NewService(store.DB()).RequestTenantDeletion(ctx, principal, domain.TenantID, tenancy.DeleteTenantInput{
		ExpectedVersion: 1, Reason: "Deletion can proceed after the legal hold is formally released.",
	}, "tenant-delete-released", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var auditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action IN ?", domain.TenantID, []string{"legal_hold.created", "legal_hold.released"}).
		Count(&auditCount).Error; err != nil || auditCount != 2 {
		t.Fatalf("Legal Hold audits = %d, %v, want 2", auditCount, err)
	}
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
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
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRetentionArchivesInactiveSessionsAndDeletesArtifactsIdempotently(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "retention-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), platformConfig, nil)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targetService)
	objectStore, err := artifacts.NewLocalStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	artifactService := artifacts.NewService(store.DB(), objectStore, config.Config{
		ArtifactPresignTTL: 15 * time.Minute, ArtifactMaxUploadBytes: 1 << 20,
	}, nil, sessionService)
	service := NewService(store.DB(), sessionService, artifactService, time.Hour, slog.Default())
	now := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return now }
	expiredSessionID := uuid.New()
	if err := store.DB().Create(&persistence.LoginSession{
		ID: expiredSessionID, UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
		RefreshTokenHash: []byte("expired-login-session"), CreatedAt: now.Add(-62 * 24 * time.Hour),
		ExpiresAt: now.Add(-31 * 24 * time.Hour), LastSeenAt: now.Add(-31 * 24 * time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.WorkerRequestReceipt{
		WorkerID: uuid.New(), RequestID: "expired-receipt", Operation: "claim", RequestHash: "hash",
		StatusCode: 200, Response: map[string]any{}, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	projectID, sessionID, artifactID := uuid.New(), uuid.New(), uuid.New()
	old := now.Add(-48 * time.Hour)
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Retention project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Old session", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		CreatedAt: old, UpdatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	payload := "retention artifact"
	digest := sha256.Sum256([]byte(payload))
	objectKey := domain.TenantID.String() + "/retention.txt"
	if _, err := objectStore.Put(ctx, objectKey, strings.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	size := int64(len(payload))
	contentType := "text/plain"
	sha := hex.EncodeToString(digest[:])
	if err := store.DB().Create(&persistence.Artifact{
		ID: artifactID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, Kind: "generated_file", Status: "ready",
		Bucket: objectStore.Bucket(), ObjectKey: objectKey, ContentType: &contentType,
		SizeBytes: &size, SHA256: &sha, CreatedByType: "user", CreatedByID: domain.UserID,
		ReadyAt: &old, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	days := 1
	if _, err := service.Update(ctx, principal, domain.TenantID, UpdateInput{
		SessionArchiveAfterDays: &days, ArtifactDeleteAfterDays: &days,
	}, "retention-update", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var expiredLoginSessions, expiredReceipts int64
	if err := store.DB().Model(&persistence.LoginSession{}).Where("id = ?", expiredSessionID).Count(&expiredLoginSessions).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.WorkerRequestReceipt{}).Where("request_id = ?", "expired-receipt").Count(&expiredReceipts).Error; err != nil {
		t.Fatal(err)
	}
	if expiredLoginSessions != 0 || expiredReceipts != 0 {
		t.Fatalf("retention left expired ephemeral records: login=%d receipts=%d", expiredLoginSessions, expiredReceipts)
	}
	var archived persistence.AgentSession
	if err := store.DB().Where("id = ?", sessionID).Take(&archived).Error; err != nil {
		t.Fatal(err)
	}
	if archived.Status != "archived" || archived.ArchivedAt == nil {
		t.Fatalf("retention did not archive the Session: %#v", archived)
	}
	var deleted persistence.Artifact
	if err := store.DB().Where("id = ?", artifactID).Take(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	if deleted.Status != "deleted" || deleted.DeletedAt == nil {
		t.Fatalf("retention did not delete the Artifact: %#v", deleted)
	}
	if _, err := objectStore.Stat(ctx, objectKey); err == nil {
		t.Fatal("retention left the Artifact payload in object storage")
	}
	if err := service.RunOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var appliedAudits int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ?", domain.TenantID, "retention_policy.applied").
		Count(&appliedAudits).Error; err != nil {
		t.Fatal(err)
	}
	if appliedAudits != 1 {
		t.Fatalf("idempotent retention produced %d summary audits", appliedAudits)
	}
}

func TestRetentionPolicyRejectsUnauthorizedAndInvalidUpdates(t *testing.T) {
	fixture := newPolicyFixture(t)
	invalid := 0
	_, err := fixture.service.Update(context.Background(), fixture.owner, fixture.tenantID,
		UpdateInput{SessionArchiveAfterDays: &invalid}, "invalid", "127.0.0.1")
	assertRetentionProblemCode(t, err, "invalid_session_retention")
	_, err = fixture.service.Update(context.Background(), fixture.member, fixture.tenantID,
		UpdateInput{}, "forbidden", "127.0.0.1")
	assertRetentionProblemCode(t, err, "tenant_forbidden")
}

type policyFixture struct {
	service  *Service
	tenantID uuid.UUID
	owner    identity.Principal
	member   identity.Principal
}

func newPolicyFixture(t *testing.T) policyFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "retention-policy-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.New()
	now := time.Now().UTC()
	for _, model := range []any{
		&persistence.User{ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Member", Status: "active", EmailVerifiedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	targets := executiontargets.NewService(store.DB(), platformConfig, nil)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targets)
	localStore, _ := artifacts.NewLocalStore(filepath.Join(t.TempDir(), "artifacts"))
	artifactService := artifacts.NewService(store.DB(), localStore, config.Config{}, nil, sessionService)
	return policyFixture{
		service:  NewService(store.DB(), sessionService, artifactService, time.Hour, slog.Default()),
		tenantID: domain.TenantID,
		owner:    identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		member:   identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID},
	}
}

func assertRetentionProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

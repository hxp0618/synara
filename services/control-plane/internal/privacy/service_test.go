package privacy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/synara-ai/synara/services/control-plane/internal/legalholds"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPrivacyRequestListRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	_, err := NewService(nil, nil).List(context.Background(), principal, requestedTenantID)
	if problemCode(err) != "tenant_not_found" {
		t.Fatalf("inactive Tenant Privacy Request list error = %v", err)
	}
}

func TestPrivacyExportAndErasureWorkflow(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "privacy-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	subject := persistence.User{
		ID: uuid.New(), Email: "privacy-subject@example.com", DisplayName: "Privacy Subject",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&subject).Error; err != nil {
		t.Fatal(err)
	}
	joinedAt := now
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: subject.ID, Role: "member", Status: "active", JoinedAt: &joinedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.OrganizationMembership{
		TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, UserID: subject.ID,
		Role: "member", Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	projectID, sessionID, turnID, artifactID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.IdentityConnection{
			ID: uuid.New(), TenantID: domain.TenantID, Kind: "oidc", Name: "Privacy test OIDC",
			Status: "active", Issuer: "https://identity.example.test", ClientID: stringPointer("privacy-client"),
			EncryptedSecret: bytes.Repeat([]byte{0x64}, 32), EncryptedDataKey: bytes.Repeat([]byte{0x65}, 32),
			Configuration: map[string]any{"allowedDomains": []string{"example.test"}},
			CreatedBy:     domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Privacy project", DefaultBranch: "main", Visibility: "organization", CreatedBy: subject.ID,
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: subject.ID, Title: "Personal incident details", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: subject.ID,
			Status: "completed", InputText: "My private personal information", CreatedAt: now, CompletedAt: &now,
		},
		&persistence.SessionEvent{
			TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID,
			SessionID: sessionID, Sequence: 1, EventID: uuid.New(), EventVersion: 1,
			EventType: "turn.created", ActorType: "user", ActorID: &subject.ID,
			Payload: map[string]any{"turnId": turnID, "inputText": "My private personal information"}, OccurredAt: now,
		},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed Privacy workflow %T: %v", model, err)
		}
	}
	objectStore, err := artifacts.NewLocalStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	payload := "private export payload"
	objectKey := domain.TenantID.String() + "/privacy.txt"
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
		SizeBytes: &size, SHA256: &digestText, CreatedByType: "user", CreatedByID: subject.ID,
		ReadyAt: &now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.ProviderCredential{
		ID: uuid.New(), TenantID: domain.TenantID, Scope: "user", ScopeUserID: &subject.ID,
		AutoSelectEnabled: true, Name: "Subject BYOK", Purpose: "provider", Provider: "codex",
		CredentialType: "api_key", EncryptedPayload: bytes.Repeat([]byte{0x61}, 32),
		EncryptedDataKey: bytes.Repeat([]byte{0x62}, 32), KMSProvider: "local", KMSKeyID: "test",
		AADVersion: 3, Version: 1, CreatedBy: subject.ID, UpdatedBy: subject.ID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.LoginSession{
		ID: uuid.New(), UserID: subject.ID, ActiveTenantID: &domain.TenantID,
		AuthMethod: "local", RefreshTokenHash: bytes.Repeat([]byte{0x63}, 32),
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	targets := executiontargets.NewService(store.DB(), platformConfig, nil)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targets)
	artifactService := artifacts.NewService(store.DB(), objectStore, config.Config{}, nil, sessionService)
	service := NewService(store.DB(), artifactService)
	service.now = func() time.Time { return now }
	adminPrincipal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	subjectPrincipal := identity.Principal{UserID: subject.ID, ActiveTenantID: &domain.TenantID}

	exportRequest, err := service.Create(ctx, subjectPrincipal, domain.TenantID, CreateInput{
		RequestType: "access_export", Reason: "I request a copy of my Tenant-scoped personal data.",
	}, "privacy-export-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	exportRequest = transitionPrivacy(t, service, ctx, adminPrincipal, exportRequest, "verified", "Identity was verified against the enterprise directory.")
	exportRequest = transitionPrivacy(t, service, ctx, adminPrincipal, exportRequest, "approved", "The verified access export request is approved for generation.")
	exportResult, err := service.ExecuteExport(ctx, subjectPrincipal, domain.TenantID, exportRequest.ID,
		exportRequest.Version, "privacy-export-execute", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if exportResult.Request.Status != "completed" || exportResult.Request.Version != 5 ||
		exportResult.Request.ResultDigestSHA256 == nil || len(*exportResult.Request.ResultDigestSHA256) != 64 {
		t.Fatalf("unexpected completed export Request: %#v", exportResult.Request)
	}
	if exportResult.Bundle.Subject.Email != subject.Email || len(exportResult.Bundle.Sessions) != 1 ||
		len(exportResult.Bundle.Turns) != 1 || exportResult.Bundle.Turns[0].InputText != "My private personal information" ||
		len(exportResult.Bundle.Artifacts) != 1 || len(exportResult.Bundle.Credentials) != 1 {
		t.Fatalf("unexpected Privacy export bundle: %#v", exportResult.Bundle)
	}
	if _, err := service.ExecuteTenantExport(
		ctx, subjectPrincipal, domain.TenantID, "tenant-export-forbidden", "127.0.0.1",
	); problemCode(err) != "tenant_data_export_forbidden" {
		t.Fatalf("member Tenant export err = %v", err)
	}
	tenantExport, err := service.ExecuteTenantExport(
		ctx, adminPrincipal, domain.TenantID, "tenant-export-execute", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	encodedTenantExport, err := json.Marshal(tenantExport.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	tenantExportDigest := sha256.Sum256(encodedTenantExport)
	if tenantExport.Receipt.SchemaVersion != "synara-tenant-export-v2" ||
		tenantExport.Receipt.DigestSHA256 != hex.EncodeToString(tenantExportDigest[:]) ||
		tenantExport.Receipt.ByteCount != int64(len(encodedTenantExport)) ||
		len(tenantExport.Bundle.Users) != 2 || len(tenantExport.Bundle.Sessions) != 1 ||
		len(tenantExport.Bundle.Turns) != 1 || tenantExport.Bundle.Turns[0].InputText != "My private personal information" ||
		len(tenantExport.Bundle.Artifacts) != 1 || len(tenantExport.Bundle.Credentials) != 1 ||
		len(tenantExport.Bundle.IdentityConnections) != 1 ||
		tenantExport.Bundle.IdentityConnections[0].Configuration["allowedDomains"] == nil {
		t.Fatalf("unexpected Tenant export result: %#v", tenantExport)
	}
	encodedText := string(encodedTenantExport)
	for _, forbidden := range []string{"encryptedPayload", "encryptedDataKey", "encryptedSecret", "refreshTokenHash"} {
		if strings.Contains(encodedText, forbidden) {
			t.Fatalf("Tenant export included secret field %q", forbidden)
		}
	}
	if err := store.DB().Model(&persistence.TenantDataExport{}).Where("id = ?", tenantExport.Receipt.ID).
		Update("byte_count", tenantExport.Receipt.ByteCount+1).Error; err == nil {
		t.Fatal("database allowed Tenant data export receipt mutation")
	}
	if err := store.DB().Delete(&persistence.TenantDataExport{}, "id = ?", tenantExport.Receipt.ID).Error; err == nil {
		t.Fatal("database allowed Tenant data export receipt deletion")
	}

	erasureRequest, err := service.Create(ctx, subjectPrincipal, domain.TenantID, CreateInput{
		RequestType: "erasure", Reason: "I request deletion of my Tenant-scoped personal data.",
	}, "privacy-erasure-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	erasureRequest = transitionPrivacy(t, service, ctx, adminPrincipal, erasureRequest, "verified", "Identity was verified before evaluating the erasure request.")
	erasureRequest = transitionPrivacy(t, service, ctx, adminPrincipal, erasureRequest, "approved", "The verified erasure request passed the administrator review.")
	holdService := legalholds.NewService(store.DB())
	hold, err := holdService.Create(ctx, adminPrincipal, domain.TenantID, legalholds.CreateInput{
		ScopeType: "user", ScopeID: subject.ID, Name: "Privacy litigation preservation",
		MatterReference: "MAT-PRIVACY-001", Reason: "Preserve the subject records until counsel closes the legal matter.",
	}, "privacy-hold-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteErasure(ctx, adminPrincipal, domain.TenantID, erasureRequest.ID,
		erasureRequest.Version, "privacy-erasure-held", "127.0.0.1"); problemCode(err) != "privacy_erasure_legal_hold_active" {
		t.Fatalf("held Privacy erasure err = %v", err)
	}
	if _, err := holdService.Release(ctx, adminPrincipal, domain.TenantID, hold.ID, legalholds.ReleaseInput{
		ExpectedVersion: hold.Version, Reason: "Counsel closed the matter and authorized release of preservation.",
	}, "privacy-hold-release", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	erasureResult, err := service.ExecuteErasure(ctx, adminPrincipal, domain.TenantID, erasureRequest.ID,
		erasureRequest.Version, "privacy-erasure-execute", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if erasureResult.Request.Status != "completed" || erasureResult.Summary.ArtifactsDeleted != 1 ||
		erasureResult.Summary.TurnsRedacted != 1 || erasureResult.Summary.EventsRedacted != 1 ||
		erasureResult.Summary.CredentialsRevoked != 1 || !erasureResult.Summary.GlobalUserPseudonymized {
		t.Fatalf("unexpected Privacy erasure result: %#v", erasureResult)
	}
	var persistedTurn persistence.AgentTurn
	if err := store.DB().Where("id = ?", turnID).Take(&persistedTurn).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persistedTurn.InputText, "redacted by Privacy Request") {
		t.Fatalf("Turn input was not redacted: %#v", persistedTurn)
	}
	var persistedEvent persistence.SessionEvent
	if err := store.DB().Where("tenant_id = ? AND session_id = ? AND sequence = ?", domain.TenantID, sessionID, 1).
		Take(&persistedEvent).Error; err != nil {
		t.Fatal(err)
	}
	if persistedEvent.Payload["redacted"] != true {
		t.Fatalf("Session Event payload was not redacted: %#v", persistedEvent.Payload)
	}
	var persistedArtifact persistence.Artifact
	if err := store.DB().Where("id = ?", artifactID).Take(&persistedArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if persistedArtifact.Status != "deleted" || persistedArtifact.DeletedAt == nil {
		t.Fatalf("Privacy Artifact was not deleted: %#v", persistedArtifact)
	}
	var membership persistence.TenantMembership
	if err := store.DB().Where("tenant_id = ? AND user_id = ?", domain.TenantID, subject.ID).Take(&membership).Error; err != nil {
		t.Fatal(err)
	}
	if membership.Status != "suspended" {
		t.Fatalf("Privacy subject membership = %q, want suspended", membership.Status)
	}
	var persistedUser persistence.User
	if err := store.DB().Where("id = ?", subject.ID).Take(&persistedUser).Error; err != nil {
		t.Fatal(err)
	}
	if persistedUser.DeletedAt == nil || persistedUser.Status != "suspended" ||
		!strings.HasSuffix(persistedUser.Email, "@redacted.invalid") {
		t.Fatalf("Privacy subject was not pseudonymized: %#v", persistedUser)
	}
	var events []persistence.PrivacyRequestEvent
	if err := store.DB().Where("privacy_request_id = ?", erasureRequest.ID).Order("version").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[0].ToStatus != "requested" || events[4].ToStatus != "completed" {
		t.Fatalf("Privacy Request history = %#v", events)
	}
	if err := store.DB().Model(&persistence.PrivacyRequestEvent{}).Where("id = ?", events[0].ID).
		Update("reason", "Direct history mutation must fail.").Error; err == nil {
		t.Fatal("database allowed Privacy Request history mutation")
	}
}

func TestPrivacyRequestRejectsCrossTenantIDSubstitutionWithoutMutation(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "privacy-cross-tenant-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	tenantService := tenancy.NewService(store.DB())
	firstPrincipal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	otherTenant, err := tenantService.CreateTenant(ctx, firstPrincipal, tenancy.CreateTenantInput{
		Slug: "privacy-other-" + uuid.NewString()[:8], Name: "Privacy Other", PlanCode: "free", Status: "active",
	}, "privacy-other-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &otherTenant.ID}
	service := NewService(store.DB(), nil)
	otherRequest, err := service.Create(ctx, otherPrincipal, otherTenant.ID, CreateInput{
		RequestType: "access_export", Reason: "This request belongs only to the other Tenant context.",
	}, "privacy-other-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, firstPrincipal, domain.TenantID, otherRequest.ID); problemCode(err) != "privacy_request_not_found" {
		t.Fatalf("cross-Tenant Privacy Request get err = %v", err)
	}
	if _, err := service.Transition(ctx, firstPrincipal, domain.TenantID, otherRequest.ID, TransitionInput{
		ExpectedVersion: otherRequest.Version, ToStatus: "denied", Reason: "Cross-Tenant mutation must be rejected before state change.",
	}, "privacy-cross-tenant-transition", "127.0.0.1"); problemCode(err) != "privacy_request_not_found" {
		t.Fatalf("cross-Tenant Privacy Request transition err = %v", err)
	}
	var persisted persistence.PrivacyRequest
	if err := store.DB().Where("tenant_id = ? AND id = ?", otherTenant.ID, otherRequest.ID).Take(&persisted).Error; err != nil || persisted.Status != "requested" {
		t.Fatalf("cross-Tenant Privacy Request mutated: %#v, %v", persisted, err)
	}
}

func stringPointer(value string) *string { return &value }

func transitionPrivacy(
	t *testing.T,
	service *Service,
	ctx context.Context,
	principal identity.Principal,
	request Request,
	toStatus, reason string,
) Request {
	t.Helper()
	updated, err := service.Transition(ctx, principal, request.TenantID, request.ID, TransitionInput{
		ExpectedVersion: request.Version, ToStatus: toStatus, Reason: reason,
	}, "privacy-transition-"+toStatus, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

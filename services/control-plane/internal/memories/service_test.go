package memories

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestAgentMemoryPublishRejectsInactiveTenantBeforeStorageAccess(t *testing.T) {
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	_, err := NewService(nil).Publish(
		context.Background(),
		identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID},
		requestedTenantID,
		PublishInput{},
		"inactive-memory",
		"127.0.0.1",
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "tenant_not_found" {
		t.Fatalf("inactive Tenant Agent Memory publish error = %v", err)
	}
}

func TestAgentMemorySessionReadRejectsCrossTenantSessionSubstitution(t *testing.T) {
	fixture := setupMemoryFixture(t, "memory-session-isolation")
	otherTenant, err := tenancy.NewService(fixture.db).CreateTenant(
		context.Background(), fixture.principal,
		tenancy.CreateTenantInput{
			Slug: "memory-other-" + uuid.NewString()[:8], Name: "Memory Other",
			PlanCode: "free", Status: "active",
		},
		"memory-other-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal := fixture.principal
	otherPrincipal.ActiveTenantID = &otherTenant.ID
	_, err = fixture.service.ListEffectiveForSession(
		context.Background(), otherPrincipal, fixture.sessionID,
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "session_not_found" {
		t.Fatalf("cross-Tenant Session Memory read error = %v", err)
	}
}

func TestPublishAndResolveEffectiveImmutableMemoryRevisions(t *testing.T) {
	ctx := context.Background()
	fixture := setupMemoryFixture(t, "memory")

	userArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "user", "text/markdown", int64(len("user")))
	projectArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "project", "text/markdown", int64(len("project")))
	sessionArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "session", "text/markdown", int64(len("session")))
	preferenceArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "preference", "application/json", int64(len("preference")))

	userPublication := publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "user", ScopeID: fixture.domain.UserID, MemoryKey: "instructions",
		ArtifactID: userArtifact.ID, SHA256: *userArtifact.SHA256, ExpectedVersion: 0,
	})
	publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "project", ScopeID: fixture.projectID, MemoryKey: "instructions",
		ArtifactID: projectArtifact.ID, SHA256: *projectArtifact.SHA256, ExpectedVersion: 0,
	})
	sessionPublication := publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "session", ScopeID: fixture.sessionID, MemoryKey: "instructions",
		ArtifactID: sessionArtifact.ID, SHA256: *sessionArtifact.SHA256, ExpectedVersion: 0,
	})
	publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "user", ScopeID: fixture.domain.UserID, MemoryKey: "preferences",
		ArtifactID: preferenceArtifact.ID, SHA256: *preferenceArtifact.SHA256, ExpectedVersion: 0,
	})

	references, err := fixture.service.ListEffectiveForSession(ctx, fixture.principal, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 {
		t.Fatalf("effective references = %#v, want session override plus independent user key", references)
	}
	if references[0].MemoryKey != "instructions" || references[0].Scope != "session" ||
		references[0].RevisionID != sessionPublication.RevisionID || references[0].ArtifactID != sessionArtifact.ID ||
		references[0].MediaType != "text/markdown" || references[0].SizeBytes != int64(len("session")) {
		t.Fatalf("session-scoped Memory did not override broader scopes: %#v", references[0])
	}
	if references[1].MemoryKey != "preferences" || references[1].Scope != "user" ||
		references[1].MediaType != "application/json" || references[1].SizeBytes != int64(len("preference")) {
		t.Fatalf("independent user Memory was not retained: %#v", references[1])
	}

	if err := fixture.db.Model(&persistence.AgentMemoryRevision{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, userPublication.RevisionID).
		Update("sha256", strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("immutable Agent Memory Revision accepted an update")
	}
	if err := fixture.db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, userArtifact.ID).
		Update("status", "deleting").Error; err == nil {
		t.Fatal("Memory-pinned Artifact accepted a destructive state transition")
	}
	if err := fixture.db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, userArtifact.ID).
		Update("content_type", "text/plain").Error; err == nil {
		t.Fatal("Memory-pinned Artifact accepted a content type mutation")
	}
	if err := fixture.db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, userArtifact.ID).
		Update("size_bytes", int64(len("user"))+1).Error; err == nil {
		t.Fatal("Memory-pinned Artifact accepted a byte size mutation")
	}
	if err := fixture.db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, userArtifact.ID).
		Update("object_key", "memory/redirected").Error; err == nil {
		t.Fatal("Memory-pinned Artifact accepted an object key mutation")
	}
}

func TestPublishAndResolveRevalidateFrozenRevisionArtifactMetadata(t *testing.T) {
	for name, updates := range map[string]map[string]any{
		"media type": {"content_type": "text/plain"},
		"byte size":  {"size_bytes": int64(15)},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := setupMemoryFixture(t, "memory-metadata-drift")
			artifact := createReadyMemoryArtifact(
				t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID,
				"metadata-drift", "text/markdown; charset=utf-8", 14,
			)
			publication := publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
				ScopeType: "session", ScopeID: fixture.sessionID, MemoryKey: "instructions",
				ArtifactID: artifact.ID, SHA256: *artifact.SHA256, ExpectedVersion: 0,
			})
			if publication.MediaType != "text/markdown" || publication.SizeBytes != 14 {
				t.Fatalf("publication did not freeze normalized metadata: %#v", publication)
			}

			// Simulate metadata corruption beneath the service layer to prove the
			// publication and resolution paths do not rely only on the database trigger.
			if err := fixture.db.Exec("DROP TRIGGER trg_artifacts_agent_memory_protected").Error; err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.Model(&persistence.Artifact{}).
				Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, artifact.ID).
				Updates(updates).Error; err != nil {
				t.Fatal(err)
			}

			if _, err := fixture.service.Publish(ctx, fixture.principal, fixture.domain.TenantID, PublishInput{
				ScopeType: "session", ScopeID: fixture.sessionID, MemoryKey: "instructions",
				ArtifactID: artifact.ID, SHA256: *artifact.SHA256, ExpectedVersion: 1,
			}, uuid.NewString(), "127.0.0.1"); err == nil {
				t.Fatal("idempotent publication accepted Artifact metadata drift")
			} else {
				assertProblemCode(t, err, "memory_revision_metadata_mismatch")
			}
			if _, err := fixture.service.ResolveRecoveryMemoryReferences(
				ctx, fixture.db, fixture.domain.TenantID, fixture.sessionID,
			); err == nil {
				t.Fatal("Recovery resolution accepted Artifact metadata drift")
			} else {
				assertProblemCode(t, err, "memory_revision_metadata_mismatch")
			}
		})
	}
}

func TestResolveExecutionRecoveryMemoryReferencesUsesExecutionRequesterInSharedSession(t *testing.T) {
	ctx := context.Background()
	fixture := setupMemoryFixture(t, "memory-shared")

	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.domain.TenantID, fixture.sessionID).
		Update("visibility", "organization").Error; err != nil {
		t.Fatal(err)
	}

	collaboratorID := addTenantOrganizationMember(t, fixture.db, fixture.domain.TenantID, fixture.domain.OrganizationID, "collaborator")
	collaborator := identity.Principal{UserID: collaboratorID, ActiveTenantID: &fixture.domain.TenantID}

	ownerPrivateArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "owner-private", "text/plain", int64(len("owner-private")))
	collaboratorPrivateArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, collaboratorID, "collaborator-private", "text/plain", int64(len("collaborator-private")))
	projectArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "project-shared", "text/markdown", int64(len("project-shared")))
	sessionArtifact := createReadyMemoryArtifact(t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID, "session-shared", "application/json", int64(len("session-shared")))

	publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "user", ScopeID: fixture.domain.UserID, MemoryKey: "private-notes",
		ArtifactID: ownerPrivateArtifact.ID, SHA256: *ownerPrivateArtifact.SHA256, ExpectedVersion: 0,
	})
	collaboratorPublication := publishMemory(t, fixture.service, collaborator, fixture.domain.TenantID, PublishInput{
		ScopeType: "user", ScopeID: collaboratorID, MemoryKey: "private-notes",
		ArtifactID: collaboratorPrivateArtifact.ID, SHA256: *collaboratorPrivateArtifact.SHA256, ExpectedVersion: 0,
	})
	projectPublication := publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "project", ScopeID: fixture.projectID, MemoryKey: "project-guidance",
		ArtifactID: projectArtifact.ID, SHA256: *projectArtifact.SHA256, ExpectedVersion: 0,
	})
	sessionPublication := publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "session", ScopeID: fixture.sessionID, MemoryKey: "session-guidance",
		ArtifactID: sessionArtifact.ID, SHA256: *sessionArtifact.SHA256, ExpectedVersion: 0,
	})

	executionID := createQueuedExecution(t, fixture.db, fixture.domain, fixture.sessionID, collaboratorID)

	references, err := fixture.service.ResolveExecutionRecoveryMemoryReferences(ctx, fixture.db, fixture.domain.TenantID, executionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 3 {
		t.Fatalf("execution references = %#v, want collaborator user + project + session", references)
	}
	byKey := referencesByKey(references)
	if byKey["private-notes"].Scope != "user" ||
		byKey["private-notes"].ArtifactID != collaboratorPrivateArtifact.ID ||
		byKey["private-notes"].RevisionID != collaboratorPublication.RevisionID {
		t.Fatalf("execution used the wrong user-scoped Memory: %#v", byKey["private-notes"])
	}
	if byKey["project-guidance"].Scope != "project" ||
		byKey["project-guidance"].RevisionID != projectPublication.RevisionID {
		t.Fatalf("project Memory was not retained: %#v", byKey["project-guidance"])
	}
	if byKey["session-guidance"].Scope != "session" ||
		byKey["session-guidance"].RevisionID != sessionPublication.RevisionID {
		t.Fatalf("session Memory was not retained: %#v", byKey["session-guidance"])
	}
	for _, reference := range references {
		if reference.ArtifactID == ownerPrivateArtifact.ID {
			t.Fatalf("shared session leaked owner private Memory: %#v", references)
		}
	}

	noActorReferences, err := fixture.service.ResolveRecoveryMemoryReferences(ctx, fixture.db, fixture.domain.TenantID, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(noActorReferences) != 2 {
		t.Fatalf("session-only references = %#v, want project + session without user scope", noActorReferences)
	}
	for _, reference := range noActorReferences {
		if reference.Scope == "user" {
			t.Fatalf("actorless resolution should fail closed on user scope: %#v", noActorReferences)
		}
	}
}

func TestPublishRejectsUnsupportedAndOversizeMemoryArtifacts(t *testing.T) {
	fixture := setupMemoryFixture(t, "memory-metadata")

	unsupported := createReadyMemoryArtifact(
		t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID,
		"unsupported", "application/octet-stream", 32,
	)
	if _, err := fixture.service.Publish(context.Background(), fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "user", ScopeID: fixture.domain.UserID, MemoryKey: "unsupported",
		ArtifactID: unsupported.ID, SHA256: *unsupported.SHA256, ExpectedVersion: 0,
	}, uuid.NewString(), "127.0.0.1"); err == nil {
		t.Fatal("unsupported Memory Artifact content type was accepted")
	} else {
		assertProblemCode(t, err, "memory_artifact_content_type_unsupported")
	}

	oversize := createReadyMemoryArtifact(
		t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID,
		"oversize", "text/plain", executions.MaximumRecoveryMemoryArtifactBytes+1,
	)
	if _, err := fixture.service.Publish(context.Background(), fixture.principal, fixture.domain.TenantID, PublishInput{
		ScopeType: "user", ScopeID: fixture.domain.UserID, MemoryKey: "oversize",
		ArtifactID: oversize.ID, SHA256: *oversize.SHA256, ExpectedVersion: 0,
	}, uuid.NewString(), "127.0.0.1"); err == nil {
		t.Fatal("oversize Memory Artifact was accepted")
	} else {
		assertProblemCode(t, err, "memory_artifact_too_large")
	}
}

func TestResolveRejectsEffectiveMemoryAboveAggregateSizeLimit(t *testing.T) {
	ctx := context.Background()
	fixture := setupMemoryFixture(t, "memory-aggregate")

	for index := 0; index < 5; index++ {
		label := fmt.Sprintf("aggregate-%02d", index)
		artifact := createReadyMemoryArtifact(
			t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID,
			label, "text/plain", executions.MaximumRecoveryMemoryArtifactBytes,
		)
		publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
			ScopeType: "project", ScopeID: fixture.projectID, MemoryKey: label,
			ArtifactID: artifact.ID, SHA256: *artifact.SHA256, ExpectedVersion: 0,
		})
	}

	if _, err := fixture.service.ResolveRecoveryMemoryReferences(ctx, fixture.db, fixture.domain.TenantID, fixture.sessionID); err == nil {
		t.Fatal("effective Memory above the aggregate bundle limit was accepted")
	} else {
		assertProblemCode(t, err, "memory_total_size_exceeded")
	}
}

func TestResolveAllowsRawHeadsAboveWinnerLimitWhenEffectiveKeysStayWithinLimit(t *testing.T) {
	ctx := context.Background()
	fixture := setupMemoryFixture(t, "memory-raw-heads")

	for index := 0; index < 33; index++ {
		key := fmt.Sprintf("shared-%02d", index)
		projectArtifact := createReadyMemoryArtifact(
			t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID,
			key+"-project", "text/markdown", int64(len(key)+8),
		)
		sessionArtifact := createReadyMemoryArtifact(
			t, fixture.db, fixture.domain, fixture.projectID, fixture.sessionID, fixture.domain.UserID,
			key+"-session", "text/markdown", int64(len(key)+8),
		)
		publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
			ScopeType: "project", ScopeID: fixture.projectID, MemoryKey: key,
			ArtifactID: projectArtifact.ID, SHA256: *projectArtifact.SHA256, ExpectedVersion: 0,
		})
		publishMemory(t, fixture.service, fixture.principal, fixture.domain.TenantID, PublishInput{
			ScopeType: "session", ScopeID: fixture.sessionID, MemoryKey: key,
			ArtifactID: sessionArtifact.ID, SHA256: *sessionArtifact.SHA256, ExpectedVersion: 0,
		})
	}

	references, err := fixture.service.ResolveRecoveryMemoryReferences(ctx, fixture.db, fixture.domain.TenantID, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 33 {
		t.Fatalf("effective references = %d, want 33 winners from 66 raw heads", len(references))
	}
	for _, reference := range references {
		if reference.Scope != "session" {
			t.Fatalf("same-key lower-scope winner was not preserved: %#v", reference)
		}
	}
}

func publishMemory(
	t *testing.T,
	service *Service,
	principal identity.Principal,
	tenantID uuid.UUID,
	input PublishInput,
) Publication {
	t.Helper()
	publication, err := service.Publish(context.Background(), principal, tenantID, input, uuid.NewString(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if publication.RevisionNumber != input.ExpectedVersion+1 || !publication.Enabled {
		t.Fatalf("unexpected publication: %#v", publication)
	}
	return publication
}

type memoryFixture struct {
	db        *gorm.DB
	service   *Service
	domain    bootstrap.Result
	projectID uuid.UUID
	sessionID uuid.UUID
	principal identity.Principal
}

func setupMemoryFixture(t *testing.T, slug string) memoryFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), slug+".sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, slug+"-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	projectID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Memory project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Memory session", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		ResourceState: "idle", MeaningfulActivityAt: now, ResourceIdleSince: &now,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return memoryFixture{
		db:        store.DB(),
		service:   NewService(store.DB()),
		domain:    domain,
		projectID: projectID,
		sessionID: sessionID,
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
	}
}

func createQueuedExecution(
	t *testing.T,
	db *gorm.DB,
	domain bootstrap.Result,
	sessionID, userID uuid.UUID,
) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	turnID := uuid.New()
	if err := db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: userID,
		Status: "queued", InputText: "shared execution", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	executionID := uuid.New()
	provider := "codex"
	if err := db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		Provider: &provider, Generation: 0, RequestedBy: userID, QueuedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return executionID
}

func addTenantOrganizationMember(
	t *testing.T,
	db *gorm.DB,
	tenantID, organizationID uuid.UUID,
	label string,
) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	email := label + "@example.com"
	if err := db.Create(&persistence.User{
		ID: userID, Email: email, DisplayName: strings.ReplaceAll(label, "-", " "),
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: tenantID, UserID: userID, Role: "member", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.OrganizationMembership{
		TenantID: tenantID, OrganizationID: organizationID, UserID: userID,
		Role: "member", Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return userID
}

func createReadyMemoryArtifact(
	t *testing.T,
	db *gorm.DB,
	domain bootstrap.Result,
	projectID, sessionID, userID uuid.UUID,
	label, contentType string,
	size int64,
) persistence.Artifact {
	t.Helper()
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte(label))
	sha := hex.EncodeToString(digest[:])
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, Kind: "memory", Status: "ready",
		Bucket: "memory-test", ObjectKey: "memory/" + uuid.NewString(),
		ContentType: &contentType, SizeBytes: &size, SHA256: &sha,
		CreatedByType: "user", CreatedByID: userID, ReadyAt: &now, CreatedAt: now,
	}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	return artifact
}

func referencesByKey(references []executions.RecoveryMemoryReference) map[string]executions.RecoveryMemoryReference {
	result := make(map[string]executions.RecoveryMemoryReference, len(references))
	for _, reference := range references {
		result[reference.MemoryKey] = reference
	}
	return result
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem %q, got %v", code, err)
	}
}

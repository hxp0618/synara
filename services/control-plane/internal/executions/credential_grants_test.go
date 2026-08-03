package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercommercial"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWorkloadCredentialGrantDescriptorDoesNotExposeLegacyOrVaultIdentity(t *testing.T) {
	grantID := uuid.New()
	encoded, err := json.Marshal(Workload{CredentialGrants: []CredentialGrantDescriptor{{
		GrantID: grantID, BindingKind: "git_fetch", Purpose: "git", Provider: "git",
		CredentialType: "ssh_key", Selector: "ssh://git@git.example.com/team/repository.git",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("gitCredentialId"), []byte("credentialId"), []byte("credentialVersion"),
		[]byte("encryptedPayload"), []byte("wrapped"), []byte("privateKey"),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("Workload Credential Grant exposed forbidden identity or plaintext: %s", encoded)
		}
	}
	if !bytes.Contains(encoded, []byte(grantID.String())) || !bytes.Contains(encoded, []byte(`"credentialGrants"`)) {
		t.Fatalf("Workload omitted the opaque Credential Grant descriptor: %s", encoded)
	}
}

func TestProviderExecutionCredentialBindingsExcludePublishAuthority(t *testing.T) {
	for _, kind := range []string{"git_fetch", "package_read"} {
		if !providerExecutionCredentialBindingKindAllowed(kind) {
			t.Fatalf("controlled read Binding %q was rejected", kind)
		}
	}
	for _, kind := range []string{
		"git_push",
		"registry_pull",
		"registry_push",
		"package_publish",
		"worker_image_pull",
	} {
		if providerExecutionCredentialBindingKindAllowed(kind) {
			t.Fatalf("unbrokered Credential Binding %q entered the Provider workload", kind)
		}
	}
}

func TestExecutionCredentialGrantsSnapshotAndReplayByGeneration(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	fixture := seedExecutionFixture(t, db)

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	var project persistence.Project
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, session.ProjectID).
		Take(&project).Error; err != nil {
		t.Fatal(err)
	}
	first := snapshotCredentialGrantsForGeneration(t, ctx, db, fixture, 1)
	if len(first) != 1 {
		t.Fatalf("expected one Credential Grant descriptor, got %#v", first)
	}
	if first[0].BindingKind != "git_fetch" || first[0].Purpose != "git" ||
		first[0].CredentialType != "https_token" || first[0].Selector != *project.RepositoryURL {
		t.Fatalf("unexpected Credential Grant descriptor: %#v", first[0])
	}

	replayed := snapshotCredentialGrantsForGeneration(t, ctx, db, fixture, 1)
	if len(replayed) != 1 || replayed[0].GrantID != first[0].GrantID {
		t.Fatalf("same generation did not reuse its immutable Grant: first=%#v replay=%#v", first, replayed)
	}

	second := snapshotCredentialGrantsForGeneration(t, ctx, db, fixture, 2)
	if len(second) != 1 || second[0].GrantID == first[0].GrantID {
		t.Fatalf("replacement generation did not receive a new Grant: first=%#v second=%#v", first, second)
	}
	var count int64
	if err := db.Model(&persistence.ExecutionCredentialGrant{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected one immutable Grant per generation, got %d", count)
	}
}

func TestExecutionProviderCredentialGrantSnapshotAndReplayByGeneration(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	fixture := seedExecutionFixture(t, db)

	first := snapshotProviderCredentialGrantForGeneration(t, ctx, db, fixture, 1)
	if first == nil {
		t.Fatal("expected an Execution Provider Credential Grant")
	}
	replayed := snapshotProviderCredentialGrantForGeneration(t, ctx, db, fixture, 1)
	if replayed == nil || *replayed != *first {
		t.Fatalf("same generation did not reuse its immutable Provider Credential Grant: first=%v replay=%v", first, replayed)
	}
	second := snapshotProviderCredentialGrantForGeneration(t, ctx, db, fixture, 2)
	if second == nil || *second == *first {
		t.Fatalf("replacement generation did not receive a new Provider Credential Grant: first=%v second=%v", first, second)
	}
	var count int64
	if err := db.Model(&persistence.ExecutionProviderCredentialGrant{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected one immutable Provider Credential Grant per generation, got %d", count)
	}
}

func TestClaimPersistsCredentialGrantDescriptorsAndReplaysSameGrant(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerManifestTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "credential-grant-claim",
	)
	cleanupWorkers(t, db, worker.ID)
	input := ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}

	first, err := service.Claim(ctx, worker, input, "credential-grant-claim-replay")
	if err != nil {
		t.Fatal(err)
	}
	if first.Value.Workload == nil || len(first.Value.Workload.CredentialGrants) != 1 {
		t.Fatalf("Claim omitted Credential Grant descriptors: %#v", first.Value.Workload)
	}
	grantID := first.Value.Workload.CredentialGrants[0].GrantID
	replayed, err := service.Claim(ctx, worker, input, "credential-grant-claim-replay")
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Value.Workload == nil ||
		len(replayed.Value.Workload.CredentialGrants) != 1 ||
		replayed.Value.Workload.CredentialGrants[0].GrantID != grantID {
		t.Fatalf("Claim receipt replay changed the Credential Grant: %#v", replayed)
	}
}

func TestClaimPersistsProviderCredentialGrantAndReplaysSameGrant(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(
		ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerManifestTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "provider-credential-grant-claim",
	)
	cleanupWorkers(t, db, worker.ID)
	input := ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}

	first, err := service.Claim(ctx, worker, input, "provider-credential-grant-claim-replay")
	if err != nil {
		t.Fatal(err)
	}
	if first.Value.Workload == nil || first.Value.Workload.ProviderCredentialGrantID == nil {
		t.Fatalf("Claim omitted the Provider Credential Grant: %#v", first.Value.Workload)
	}
	grantID := *first.Value.Workload.ProviderCredentialGrantID
	replayed, err := service.Claim(ctx, worker, input, "provider-credential-grant-claim-replay")
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Value.Workload == nil ||
		replayed.Value.Workload.ProviderCredentialGrantID == nil ||
		*replayed.Value.Workload.ProviderCredentialGrantID != grantID {
		t.Fatalf("Claim receipt replay changed the Provider Credential Grant: %#v", replayed)
	}
}

func TestEnterpriseClaimRequiresActiveProviderCommercialAuthorization(t *testing.T) {
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
	db := store.DB()
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	service.providerCommercialAuthorizationRequired = true
	service.providerCommercialOperatorTenantID = fixture.TenantID
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "commercial-authorization-claim")
	cleanupWorkers(t, db, worker.ID)
	claimInput := ClaimExecutionInput{ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID}

	_, err = service.Claim(ctx, worker, claimInput, "commercial-authorization-missing")
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "provider_commercial_authorization_required" {
		t.Fatalf("claim without commercial authorization = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, 4)
	for index := range 4 {
		user := persistence.User{ID: uuid.New(), Email: "claim-commercial-" + string(rune('a'+index)) + "@example.test", DisplayName: "Claim Commercial " + string(rune('A'+index)), Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.TenantMembership{TenantID: fixture.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	for index, role := range []string{"legal", "privacy", "security", "product"} {
		if err := db.Create(&persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: fixture.TenantID, UserID: approvers[index], AuthorityKey: "provider_commercial." + role,
			Status: "active", Version: 1, ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: fixture.UserID,
			Reason:            "Owner assigned the exact Provider commercial claim approval function.",
			EvidenceReference: "https://evidence.example.test/claim/authority/" + role, CreatedAt: now, UpdatedAt: now,
			EvidenceSHA256: stage6authority.DigestPointer("claim-authority-" + role),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	governance := providercommercial.NewService(db, fixture.TenantID)
	authorization, err := governance.Create(ctx, fixture.UserID, providercommercial.CreateInput{
		AuthorizationKey: "claim-openai-2026", Provider: "codex", ProviderProduct: "OpenAI API",
		AccountType: "Enterprise API organization", ContractingEntity: "OpenAI contracting entity",
		CredentialMode: "customer_byok", AllowedCredentialScopes: []string{"organization"}, AllowedRegions: []string{"default"},
		DataUsePolicy: "no_training", RetentionPolicy: "Approved enterprise API retention policy applies.",
		TermsEffectiveAt: now.Add(-24 * time.Hour), TermsReference: "https://evidence.example.test/claim/terms",
		TermsSHA256:        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AgreementReference: "https://evidence.example.test/claim/agreement", DPAReference: "https://evidence.example.test/claim/dpa",
		AgreementSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", DPASHA256: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ProhibitedUseSummary:        "Consumer login sharing and safety-control bypass are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/claim/termination", ReviewExpiresAt: now.Add(90 * 24 * time.Hour),
		TerminationRunbookSHA256: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}, "claim-authorization-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	authorization, err = governance.Transition(ctx, fixture.UserID, authorization.ID, providercommercial.TransitionInput{ExpectedVersion: authorization.Version, TargetState: "ready_for_review", Reason: "Submit exact hosted use for separated review."}, "claim-authorization-ready", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range []string{"legal", "privacy", "security", "product"} {
		authorization, err = governance.RecordApproval(ctx, approvers[index], authorization.ID, providercommercial.ApprovalInput{
			Role: role, Decision: "approved", Reason: "Approved the exact Provider commercial use and data boundary.",
			EvidenceReference: "https://evidence.example.test/claim/approval/" + role,
			EvidenceSHA256:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		}, "claim-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = governance.Transition(ctx, fixture.UserID, authorization.ID, providercommercial.TransitionInput{ExpectedVersion: authorization.Version, TargetState: "active", Reason: "Activate after all separated approvals committed."}, "claim-authorization-active", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, worker, claimInput, "commercial-authorization-active")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Value.Workload == nil || claimed.Value.Workload.ProviderCredentialGrantID == nil {
		t.Fatalf("authorized hosted claim omitted Provider credential grant: %#v", claimed.Value)
	}
}

func TestExecutionCredentialGrantsOmitDisabledBindings(t *testing.T) {
	ctx := context.Background()
	profile, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(
		ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	fixture := seedExecutionFixture(t, db)
	now := time.Now().UTC()
	if err := db.Model(&persistence.CredentialBinding{}).
		Where(
			"tenant_id = ? AND credential_id = ? AND binding_kind = ? AND disabled_at IS NULL",
			fixture.TenantID, fixture.GitCredentialID, "git_fetch",
		).
		Updates(map[string]any{"disabled_at": now, "disabled_by": fixture.UserID}).Error; err != nil {
		t.Fatal(err)
	}

	descriptors := snapshotCredentialGrantsForGeneration(t, ctx, db, fixture, 1)
	if len(descriptors) != 0 {
		t.Fatalf("disabled Binding produced an Execution Grant: %#v", descriptors)
	}
}

func snapshotCredentialGrantsForGeneration(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	fixture executionFixture,
	generation int64,
) []CredentialGrantDescriptor {
	t.Helper()
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Update("generation", generation).Error; err != nil {
		t.Fatal(err)
	}
	var descriptors []CredentialGrantDescriptor
	err := db.Transaction(func(tx *gorm.DB) error {
		var execution persistence.AgentExecution
		if err := tx.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Take(&execution).Error; err != nil {
			return err
		}
		var err error
		descriptors, err = bindExecutionCredentialGrants(ctx, tx, execution, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return descriptors
}

func snapshotProviderCredentialGrantForGeneration(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	fixture executionFixture,
	generation int64,
) *uuid.UUID {
	t.Helper()
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{
			"generation":                           generation,
			"provider_credential_id_snapshot":      fixture.ProviderCredentialID,
			"provider_credential_version_snapshot": 1,
		}).Error; err != nil {
		t.Fatal(err)
	}
	var grantID *uuid.UUID
	err := db.Transaction(func(tx *gorm.DB) error {
		var execution persistence.AgentExecution
		if err := tx.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Take(&execution).Error; err != nil {
			return err
		}
		var err error
		grantID, err = bindExecutionProviderCredentialGrant(ctx, tx, execution, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return grantID
}

package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
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

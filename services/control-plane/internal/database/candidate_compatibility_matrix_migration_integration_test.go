package database

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6candidate"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestCandidateCompatibilityMatrixMigrationRequiresV5ForActivePostgresCandidates(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	databaseURL = postgresisolation.URL(t, databaseURL)
	ctx := context.Background()
	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(ctx, db, migrationsThrough(t, "000162_operations_support_terminal_audit_evidence.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "candidate-v5-migration-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	legacy := candidateModelForCompatibilityMigration(t, domain.TenantID, domain.UserID, "legacy-v4", "stage6/legacy-v4", true, now)
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err == nil {
		t.Fatalf("Migration 000163 should reject an active v4 Candidate, got %v", err)
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", legacy.ID).Updates(map[string]any{
		"state": "ready_for_review", "version": 2, "updated_at": now.Add(time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", legacy.ID).Updates(map[string]any{
		"state": "rejected", "version": 3, "rejected_at": now.Add(2 * time.Second), "updated_at": now.Add(2 * time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	newLegacy := candidateModelForCompatibilityMigration(t, domain.TenantID, domain.UserID, "new-v4", "stage6/new-v4", true, now)
	if err := db.Create(&newLegacy).Error; err == nil {
		t.Fatal("Migration 000163 allowed a new v4 Candidate")
	}
	current := candidateModelForCompatibilityMigration(t, domain.TenantID, domain.UserID, "new-v5", "stage6/new-v5", false, now)
	if err := db.Create(&current).Error; err != nil {
		t.Fatalf("Migration 000163 rejected an exact v5 Candidate: %v", err)
	}
}

func candidateModelForCompatibilityMigration(
	t *testing.T,
	operatorTenantID, creatorID uuid.UUID,
	candidateID, environmentID string,
	legacy bool,
	now time.Time,
) persistence.Stage6ReleaseCandidate {
	t.Helper()
	var encoded []byte
	var digest string
	var err error
	if legacy {
		encodedBytes, encodedDigest, receiptErr := stage6candidate.LegacyV4Receipt(candidateID, environmentID)
		encoded, digest, err = encodedBytes, encodedDigest, receiptErr
	} else {
		encodedBytes, encodedDigest, receiptErr := stage6candidate.Receipt(candidateID, environmentID)
		encoded, digest, err = encodedBytes, encodedDigest, receiptErr
	}
	if err != nil {
		t.Fatal(err)
	}
	lockfile, _ := hex.DecodeString(strings.Repeat("b", 64))
	bundleDigest, _ := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	finalAssets, _ := hex.DecodeString(strings.Repeat("d", 64))
	schema := "synara.stage6-candidate-evidence-bundle-validation.v5"
	if legacy {
		schema = "synara.stage6-candidate-evidence-bundle-validation.v4"
	}
	return persistence.Stage6ReleaseCandidate{
		ID: uuid.New(), OperatorTenantID: operatorTenantID, CandidateID: candidateID,
		SourceCommit: strings.Repeat("a", 40), LockfileSHA256: lockfile,
		EvidenceBundleSHA256: bundleDigest, EvidenceBundleReceipt: encoded, EvidenceBundleSchema: schema,
		EvidenceBundleAssessment: "evidence-consistent-not-ga-approved", EvidenceBundleValidatedAt: "2026-07-31T12:00:00Z",
		EvidenceBundleReceiptSizeBytes: int64(len(encoded)), DesktopArtifactSetSHA256: "sha256:" + strings.Repeat("e", 64),
		EvidenceReceiptBound: true, FinalAssetSetSHA256: finalAssets, EnvironmentID: environmentID,
		ImpactDomains: []string{"code_change"}, State: "draft", Version: 1, CreatedBy: creatorID,
		ResidualRisks: []map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
}

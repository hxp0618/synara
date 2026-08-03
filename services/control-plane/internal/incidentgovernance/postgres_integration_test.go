package incidentgovernance

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestIncidentGovernancePostgresSerializesPublicEvidenceAndRetainsHistory(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	databaseURL = postgresisolation.URL(t, databaseURL)
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "incident-governance-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	communicationsID := createIncidentOperator(t, db, domain.TenantID, "incident-pg-comms@example.test", "Incident PG Comms", "admin", now)
	securityID := createIncidentOperator(t, db, domain.TenantID, "incident-pg-security@example.test", "Incident PG Security", "security_admin", now)
	service := NewService(db, domain.TenantID, "https://status.example.test")
	service.now = func() time.Time { return now }
	incident, err := service.Create(ctx, domain.UserID, CreateInput{
		IncidentKey: "INC-PG-" + uuid.NewString(), Severity: "SEV-1",
		Title:                 "PostgreSQL incident governance concurrency",
		InternalImpactSummary: "Customers may observe delayed execution starts during the PostgreSQL governance test.",
		BroadInternalImpact:   true, SecurityPrivacyImpact: true,
		AffectedComponents: []string{"execution-scheduling"}, AffectedRegions: []string{"region-one"},
		CommunicationsLeadUserID: communicationsID, SecurityPrivacyLeadUserID: &securityID,
		StartedAt: now.Add(-2 * time.Minute), ImpactConfirmedAt: now.Add(-time.Minute),
	}, "pg-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	incident, err = service.BindStatusBoard(ctx, domain.UserID, incident.ID, BindStatusBoardInput{
		ExpectedVersion: incident.Version, InternalStatusBoardIncidentReference: "incident-postgres-concurrency",
		Reason: "Bind the exact independent internal Status Board incident before recording employee-visible evidence.",
	}, "pg-bind", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsByUpdate := make(chan error, 2)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, updateErr := service.AddInternalUpdate(ctx, communicationsID, incident.ID, AddUpdateInput{
				ExpectedVersion: incident.Version, Kind: "initial",
				Summary:           "PostgreSQL serialized one exact initial public incident evidence record.",
				PublishedAt:       now.Add(time.Duration(index+1) * time.Second),
				EvidenceReference: "https://status.example.test/incidents/postgres/updates/initial-" + string(rune('a'+index)),
			}, "pg-update", "127.0.0.1")
			errorsByUpdate <- updateErr
		}(index)
	}
	wait.Wait()
	close(errorsByUpdate)
	successes := 0
	conflicts := 0
	for updateErr := range errorsByUpdate {
		if updateErr == nil {
			successes++
		} else if problemCode(updateErr) == "incident_internal_update_conflict" || problemCode(updateErr) == "incident_version_conflict" {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent internal update error: %v", updateErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent updates successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	incident, err = service.get(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(incident.InternalUpdates) != 1 {
		t.Fatalf("PostgreSQL internal update count = %d, want 1", len(incident.InternalUpdates))
	}
	if err := db.Model(&persistence.Stage6IncidentUpdate{}).
		Where("incident_id = ?", incident.ID).Update("summary", "forged mutable evidence").Error; err == nil {
		t.Fatal("PostgreSQL internal incident evidence was mutable")
	}
	if err := db.Delete(&persistence.Stage6Incident{}, "id = ?", incident.ID).Error; err == nil {
		t.Fatal("PostgreSQL incident operational history was deletable")
	}
}

func TestIncidentResolutionApprovalDigestMigrationSupersedesLegacyPostgresDecision(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, incidentGovernanceMigrationsThrough(t, "000148_stage6_billing_exercise_approval_evidence_digests.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "incident-resolution-digest-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	communicationsID := createIncidentOperator(t, db, domain.TenantID, "incident-upgrade-comms-"+uuid.NewString()+"@example.test", "Incident upgrade communications", "admin", now)
	securityID := createIncidentOperator(t, db, domain.TenantID, "incident-upgrade-security-"+uuid.NewString()+"@example.test", "Incident upgrade security", "security_admin", now)
	service := NewService(db, domain.TenantID, "https://status.example.test")
	service.now = func() time.Time { return now }
	incident, err := service.Create(ctx, domain.UserID, CreateInput{
		IncidentKey: "INC-UPGRADE-" + uuid.NewString(), Severity: "SEV-2",
		Title:                 "Incident resolution approval digest upgrade",
		InternalImpactSummary: "Security and Privacy resolution evidence is being upgraded to exact byte binding.",
		BroadInternalImpact:   false, SecurityPrivacyImpact: true,
		AffectedComponents: []string{"control-plane-api"}, AffectedRegions: []string{"region-one"},
		CommunicationsLeadUserID: communicationsID, SecurityPrivacyLeadUserID: &securityID,
		StartedAt: now.Add(-2 * time.Minute), ImpactConfirmedAt: now.Add(-time.Minute),
	}, "upgrade-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"identified", "monitoring"} {
		incident, err = service.Transition(ctx, domain.UserID, incident.ID, TransitionInput{
			ExpectedVersion: incident.Version, TargetState: target,
			Reason: "Advance the upgrade fixture through the governed incident lifecycle.",
		}, "upgrade-"+target, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	legacy := persistence.Stage6IncidentResolutionApproval{
		ID: uuid.New(), IncidentID: incident.ID, OperatorTenantID: domain.TenantID,
		Decision: "approved", Reason: "Legacy Security and Privacy approval referenced mutable evidence without an exact digest.",
		EvidenceReference: "https://evidence.example.test/incidents/legacy-resolution", ApproverUserID: securityID, CreatedAt: now,
	}
	if err := db.Omit("EvidenceSHA256", "SupersededAt", "SupersededReason").Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&legacy, "id = ?", legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.SupersededAt == nil || legacy.SupersededReason == nil || !strings.Contains(*legacy.SupersededReason, "Migration 000149") {
		t.Fatalf("legacy Incident resolution approval was not superseded: %#v", legacy)
	}
	incident, err = service.get(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if incident.ResolutionApproval != nil || len(incident.ResolutionApprovals) != 1 || incident.ResolutionApprovals[0].SupersededAt == nil {
		t.Fatalf("Incident resolution approval projection after Migration 000149 = %#v", incident)
	}
	_, err = service.Transition(ctx, domain.UserID, incident.ID, TransitionInput{
		ExpectedVersion: incident.Version, TargetState: "resolved",
		Reason: "A URL-only legacy decision must no longer authorize incident resolution.",
	}, "legacy-resolve", "127.0.0.1")
	if problemCode(err) != "incident_security_approval_required" {
		t.Fatalf("legacy resolution transition error = %v", err)
	}
	incident, err = service.RecordResolutionApproval(ctx, securityID, incident.ID, ResolutionApprovalInput{
		Decision: "approved", Reason: "Replacement Security and Privacy authority reviewed exact byte-bound resolution evidence.",
		EvidenceReference: "https://evidence.example.test/incidents/replacement-resolution",
		EvidenceSHA256:    "sha256:" + strings.Repeat("2", 64),
	}, "replacement-resolution", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if incident.ResolutionApproval == nil || len(incident.ResolutionApprovals) != 2 {
		t.Fatalf("replacement Incident resolution approval = %#v", incident)
	}
	incident, err = service.Transition(ctx, domain.UserID, incident.ID, TransitionInput{
		ExpectedVersion: incident.Version, TargetState: "resolved",
		Reason: "Resolve only after exact byte-bound Security and Privacy approval.",
	}, "replacement-resolve", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if incident.State != "resolved" || incident.Version != 4 {
		t.Fatalf("resolved Incident after replacement approval = %#v", incident)
	}
}

func incidentGovernanceMigrationsThrough(t *testing.T, tail string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	result := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() > tail {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return result
}

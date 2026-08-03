package compliancegovernance

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestComplianceGovernancePostgresStartGateAndAppendOnlyEvidence(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "compliance-governance-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	operators := make([]uuid.UUID, 0, 5)
	for index := range 5 {
		user := persistence.User{ID: uuid.New(), Email: "compliance-pg-" + string(rune('a'+index)) + "@example.test", DisplayName: "Compliance PG " + string(rune('A'+index)), Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		operators = append(operators, user.ID)
	}
	seedComplianceAuthority(t, db, domain.TenantID, domain.UserID, operators[0], "compliance.evidence.security", now)
	for index, role := range requiredDecisionRoles {
		seedComplianceAuthority(t, db, domain.TenantID, domain.UserID, operators[index], "compliance."+role, now)
	}
	service := NewService(db, domain.TenantID)
	program, err := service.CreateProgram(ctx, domain.UserID, CreateProgramInput{
		ProgramKey: "soc2-pg", Framework: "soc2_type2", ScopeVersion: "2026.1",
		ScopeSummary:           "Synara SaaS production services and supporting production operations boundary.",
		ExecutiveSponsorUserID: operators[4], AuditorOrganization: "Independent Auditor LLP",
		AuditorEngagementReference: "https://evidence.example.test/pg/auditor", ObservationStart: now.Add(time.Hour),
		ObservationEnd: now.AddDate(0, 6, 0), EvidenceRepositoryReference: "https://evidence.example.test/pg/repository",
		EvidenceAccessPolicyReference: "https://evidence.example.test/pg/access", EvidenceRetentionDays: 365,
		VendorRegisterReference: "https://evidence.example.test/pg/vendors", RiskRegisterReference: "https://evidence.example.test/pg/risks",
	}, "pg-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var manifestControl Control
	for index, family := range requiredControlFamilies {
		program, err = service.CreateControl(ctx, domain.UserID, program.ID, CreateControlInput{
			ControlID: "PG." + string(rune('A'+index)), Family: family, Title: "PostgreSQL " + family,
			Description: "PostgreSQL integration control for the bounded " + family + " evidence family.",
			OwnerUserID: operators[index%len(operators)], Cadence: "quarterly",
			EvidenceRequirement: "Retain the exact hashed source and independent review for the selected sample.",
		}, "pg-control-"+family, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if family == "change_release" {
			for _, control := range program.Controls {
				if control.Family == family {
					manifestControl = control
				}
			}
		}
	}
	program, err = service.SubmitEvidence(ctx, domain.UserID, program.ID, SubmitEvidenceInput{
		ControlRecordID: manifestControl.ID, EvidenceID: "pg-release-manifest", EvidenceType: "release_manifest",
		PeriodStart: now.Add(-time.Hour), PeriodEnd: now, SourceReference: "https://evidence.example.test/pg/manifest",
		SHA256: strings.Repeat("a", 64), MediaType: "application/json", Classification: "confidential",
		CollectedAt: now, RetentionUntil: now.AddDate(2, 0, 0),
	}, "pg-evidence", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var manifest Evidence
	for _, control := range program.Controls {
		for _, evidence := range control.Evidence {
			if evidence.EvidenceType == "release_manifest" {
				manifest = evidence
			}
		}
	}
	program, err = service.ReviewEvidence(ctx, operators[0], program.ID, manifest.ID, ReviewEvidenceInput{
		Decision: "accepted", ReviewRole: "security", Reason: "PostgreSQL manifest review accepted the exact immutable digest.",
		EvidenceReference: "https://evidence.example.test/pg/manifest-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("b", 64),
	}, "pg-review", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	program, err = service.Transition(ctx, domain.UserID, program.ID, TransitionInput{ExpectedVersion: program.Version, TargetState: "ready_for_review", Reason: "Submit the complete PostgreSQL control inventory for start-gate review."}, "pg-ready", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsByRole := make(chan error, len(requiredDecisionRoles))
	for index, role := range requiredDecisionRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			_, decisionErr := service.RecordDecision(ctx, operators[index], program.ID, DecisionInput{
				DecisionRole: role, Decision: "approved", Reason: "PostgreSQL concurrent start-gate role approved the bounded record.",
				EvidenceReference: "https://evidence.example.test/pg/decision/" + role,
				EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('c'+index)), 64),
			}, "pg-decision-"+role, "127.0.0.1")
			errorsByRole <- decisionErr
		}(index, role)
	}
	wait.Wait()
	close(errorsByRole)
	for decisionErr := range errorsByRole {
		if decisionErr != nil {
			t.Fatal(decisionErr)
		}
	}
	program, err = service.get(ctx, program.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !program.Readiness.EligibleForRecordCompleteReview || len(program.Decisions) != 4 {
		t.Fatalf("PostgreSQL readiness = %#v", program.Readiness)
	}
	program, err = service.Transition(ctx, domain.UserID, program.ID, TransitionInput{ExpectedVersion: program.Version, TargetState: "record_complete", Reason: "Freeze the complete record without claiming external audit activation."}, "pg-complete", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ComplianceEvidence{}).Where("id = ?", manifest.ID).Update("source_reference", "https://evidence.example.test/pg/mutated").Error; err == nil {
		t.Fatal("PostgreSQL compliance evidence was mutable")
	}
	if err := db.Model(&persistence.Stage6ComplianceProgram{}).Where("id = ?", program.ID).Update("scope_version", "mutated").Error; err == nil {
		t.Fatal("PostgreSQL compliance scope was mutable")
	}
	var persisted persistence.Stage6ComplianceProgram
	if err := db.Where("id = ?", program.ID).First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	persisted.ID = uuid.New()
	persisted.ProgramKey = "soc2-pg-query-ref"
	persisted.AuditorEngagementReference = "https://evidence.example.test/pg/auditor?token=secret"
	persisted.State = "draft"
	persisted.Version = 1
	persisted.RecordCompletedAt = nil
	persisted.CreatedAt = now
	persisted.UpdatedAt = now
	if err := db.Create(&persisted).Error; err == nil {
		t.Fatal("PostgreSQL accepted a credential-bearing compliance reference")
	}
}

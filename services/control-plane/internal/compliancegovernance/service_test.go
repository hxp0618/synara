package compliancegovernance

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestComplianceGovernanceRequiresCompleteSeparatedStartGate(t *testing.T) {
	fixture := newComplianceFixture(t)
	program := fixture.createProgram(t)
	controls := fixture.createRequiredControls(t, program)
	now := fixture.now

	program, err := fixture.service.SubmitEvidence(fixture.ctx, fixture.creatorID, program.ID, SubmitEvidenceInput{
		ControlRecordID: controls[1].ID, EvidenceID: "release-manifest-v0.6.3", EvidenceType: "release_manifest",
		PeriodStart: now.Add(-time.Hour), PeriodEnd: now, SourceReference: "https://evidence.example.test/release-manifest.json",
		SHA256: strings.Repeat("a", 64), MediaType: "application/json", Classification: "confidential",
		CollectedAt: now, RetentionUntil: now.AddDate(2, 0, 0),
	}, "submit-manifest", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	evidence := program.Controls[1].Evidence[0]
	_, err = fixture.service.ReviewEvidence(fixture.ctx, fixture.operatorIDs[0], program.ID, evidence.ID, ReviewEvidenceInput{
		Decision: "accepted", ReviewRole: "security", Reason: "A zero digest cannot bind the bytes reviewed.",
		EvidenceReference: "https://evidence.example.test/zero-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "zero-review", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_evidence_review_invalid")
	_, err = fixture.service.ReviewEvidence(fixture.ctx, fixture.creatorID, program.ID, evidence.ID, ReviewEvidenceInput{
		Decision: "accepted", ReviewRole: "security", Reason: "Self review must not be accepted.",
		EvidenceReference: "https://evidence.example.test/self-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("1", 64),
	}, "self-review", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_evidence_self_review_forbidden")

	program, err = fixture.service.ReviewEvidence(fixture.ctx, fixture.operatorIDs[0], program.ID, evidence.ID, ReviewEvidenceInput{
		Decision: "accepted", ReviewRole: "security", Reason: "Reviewed the exact manifest digest and immutable source reference.",
		EvidenceReference: "https://evidence.example.test/manifest-review",
		EvidenceSHA256:    "sha256:" + strings.Repeat("2", 64),
	}, "manifest-review", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if program.Controls[1].Evidence[0].Review == nil || program.Controls[1].Evidence[0].Review.EvidenceSHA256 == nil ||
		*program.Controls[1].Evidence[0].Review.EvidenceSHA256 != "sha256:"+strings.Repeat("2", 64) {
		t.Fatalf("byte-bound compliance review = %#v", program.Controls[1].Evidence[0].Review)
	}
	program = fixture.transition(t, program, "ready_for_review")
	_, err = fixture.service.RecordDecision(fixture.ctx, fixture.operatorIDs[0], program.ID, DecisionInput{
		DecisionRole: "security", Decision: "approved", Reason: "A zero digest cannot bind the decision evidence.",
		EvidenceReference: "https://evidence.example.test/zero-decision",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "zero-decision", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_decision_invalid")

	_, err = fixture.service.RecordDecision(fixture.ctx, fixture.creatorID, program.ID, DecisionInput{
		DecisionRole: "security", Decision: "approved", Reason: "Creator cannot approve the same start gate.",
		EvidenceReference: "https://evidence.example.test/creator-decision",
		EvidenceSHA256:    "sha256:" + strings.Repeat("3", 64),
	}, "creator-decision", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_decision_self_review_forbidden")

	for index, role := range requiredDecisionRoles {
		program, err = fixture.service.RecordDecision(fixture.ctx, fixture.operatorIDs[index], program.ID, DecisionInput{
			DecisionRole: role, Decision: "approved", Reason: "Reviewed the bounded compliance start-gate record for this role.",
			EvidenceReference: "https://evidence.example.test/start-gate/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('4'+index)), 64),
		}, "decision-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !program.Readiness.EligibleForRecordCompleteReview || len(program.Readiness.MissingControlFamilies) != 0 ||
		len(program.Readiness.MissingDecisionRoles) != 0 || !program.Readiness.HasAcceptedReleaseManifest {
		t.Fatalf("readiness = %#v", program.Readiness)
	}
	program = fixture.transition(t, program, "record_complete")
	if program.State != "record_complete" || program.RecordCompletedAt == nil || program.Readiness.Assessment != "record-complete-not-audit-active" {
		t.Fatalf("completed compliance record = %#v", program)
	}

	if err := fixture.db.Model(&persistence.Stage6ComplianceProgram{}).Where("id = ?", program.ID).
		Update("scope_summary", "A malicious replacement scope that should be rejected by the database trigger.").Error; err == nil {
		t.Fatal("compliance program scope was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6ComplianceEvidence{}, "id = ?", evidence.ID).Error; err == nil {
		t.Fatal("compliance evidence was deletable")
	}
	if err := fixture.db.Model(&persistence.Stage6ComplianceEvidenceReview{}).Where("evidence_record_id = ?", evidence.ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("9", 64)).Error; err == nil {
		t.Fatal("compliance evidence review digest was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6ComplianceProgramDecision{}).Where("id = ?", program.Decisions[0].ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("9", 64)).Error; err == nil {
		t.Fatal("compliance program decision digest was mutable")
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where("tenant_id = ? AND action LIKE ?", fixture.operatorTenantID, "compliance.%").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 16 {
		t.Fatalf("compliance Audit count = %d, want 16", auditCount)
	}
}

func TestComplianceGovernanceRejectsIncompleteGateEvidenceDriftAndDuplicateDecisions(t *testing.T) {
	fixture := newComplianceFixture(t)
	_, err := fixture.service.CreateProgram(fixture.ctx, fixture.creatorID, CreateProgramInput{
		ProgramKey: "soc2-query-ref", Framework: "soc2_type2", ScopeVersion: "2026.1",
		ScopeSummary:           "A deliberately invalid program reference must fail before persistence.",
		ExecutiveSponsorUserID: fixture.operatorIDs[4], AuditorOrganization: "Independent Auditor LLP",
		AuditorEngagementReference: "https://evidence.example.test/auditor?token=secret",
		ObservationStart:           fixture.now.AddDate(0, 0, 1), ObservationEnd: fixture.now.AddDate(0, 6, 1),
		EvidenceRepositoryReference:   "https://evidence.example.test/repository",
		EvidenceAccessPolicyReference: "https://evidence.example.test/access-policy", EvidenceRetentionDays: 365,
		VendorRegisterReference: "https://evidence.example.test/vendors", RiskRegisterReference: "https://evidence.example.test/risks",
	}, "invalid-reference", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_program_reference_invalid")
	program := fixture.createProgram(t)
	controls := fixture.createRequiredControls(t, program)
	unauthorized := persistence.User{
		ID: uuid.New(), Email: "compliance-outsider@example.test", DisplayName: "Compliance Outsider",
		Status: "active", EmailVerifiedAt: &fixture.now, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	if err := fixture.db.Create(&unauthorized).Error; err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.SubmitEvidence(fixture.ctx, unauthorized.ID, program.ID, SubmitEvidenceInput{
		ControlRecordID: controls[0].ID, EvidenceID: "outsider-evidence", EvidenceType: "access_review",
		PeriodStart: fixture.now.Add(-time.Hour), PeriodEnd: fixture.now,
		SourceReference: "https://evidence.example.test/outsider", SHA256: strings.Repeat("c", 64),
		MediaType: "application/json", Classification: "restricted", CollectedAt: fixture.now,
		RetentionUntil: fixture.now.AddDate(2, 0, 0),
	}, "outsider-evidence", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_evidence_create_failed")
	program = fixture.transition(t, program, "ready_for_review")

	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, program.ID, TransitionInput{
		ExpectedVersion: program.Version, TargetState: "record_complete", Reason: "Attempt completion before evidence and approvals exist.",
	}, "incomplete", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_start_gate_incomplete")

	_, err = fixture.service.RecordDecision(fixture.ctx, fixture.operatorIDs[0], program.ID, DecisionInput{
		DecisionRole: "security", Decision: "approved", Reason: "First immutable Security decision for this program.",
		EvidenceReference: "https://evidence.example.test/security-one",
		EvidenceSHA256:    "sha256:" + strings.Repeat("7", 64),
	}, "security-one", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.RecordDecision(fixture.ctx, fixture.operatorIDs[1], program.ID, DecisionInput{
		DecisionRole: "security", Decision: "approved", Reason: "Duplicate role must not replace the first decision.",
		EvidenceReference: "https://evidence.example.test/security-two",
		EvidenceSHA256:    "sha256:" + strings.Repeat("8", 64),
	}, "security-two", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_decision_conflict")

	_, err = fixture.service.SubmitEvidence(fixture.ctx, fixture.operatorIDs[1], program.ID, SubmitEvidenceInput{
		ControlRecordID: program.Controls[0].ID, EvidenceID: "too-short-retention", EvidenceType: "access_review",
		PeriodStart: fixture.now.Add(-time.Hour), PeriodEnd: fixture.now,
		SourceReference: "https://evidence.example.test/access-review", SHA256: strings.Repeat("b", 64),
		MediaType: "application/json", Classification: "restricted", CollectedAt: fixture.now,
		RetentionUntil: fixture.now.AddDate(0, 6, 0),
	}, "short-retention", "127.0.0.1")
	assertComplianceProblem(t, err, "compliance_evidence_retention_invalid")
}

type complianceFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	operatorTenantID uuid.UUID
	creatorID        uuid.UUID
	operatorIDs      []uuid.UUID
	now              time.Time
}

func newComplianceFixture(t *testing.T) complianceFixture {
	t.Helper()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "compliance-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	operators := make([]uuid.UUID, 0, 5)
	for index := range 5 {
		user := persistence.User{
			ID: uuid.New(), Email: "compliance-operator-" + string(rune('a'+index)) + "@example.test",
			DisplayName: "Compliance Operator " + string(rune('A'+index)), Status: "active",
			EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membership := persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.DB().Create(&membership).Error; err != nil {
			t.Fatal(err)
		}
		operators = append(operators, user.ID)
	}
	seedComplianceAuthority(t, store.DB(), domain.TenantID, domain.UserID, operators[0], governanceauthority.ComplianceEvidencePrefix+"security", now)
	for index, role := range requiredDecisionRoles {
		seedComplianceAuthority(t, store.DB(), domain.TenantID, domain.UserID, operators[index], "compliance."+role, now)
	}
	seedComplianceAuthority(t, store.DB(), domain.TenantID, domain.UserID, operators[1], governanceauthority.ComplianceSecurity, now)
	return complianceFixture{ctx: ctx, db: store.DB(), service: NewService(store.DB(), domain.TenantID), operatorTenantID: domain.TenantID, creatorID: domain.UserID, operatorIDs: operators, now: now}
}

func seedComplianceAuthority(t *testing.T, db *gorm.DB, tenantID, ownerID, userID uuid.UUID, key string, now time.Time) {
	t.Helper()
	grant := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: tenantID, UserID: userID, AuthorityKey: key,
		Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: ownerID,
		Reason:            "Owner assigned the exact compliance governance review function.",
		EvidenceReference: "https://evidence.example.test/governance-authority/" + strings.ReplaceAll(key, ".", "/"),
		EvidenceSHA256:    stage6authority.DigestPointer("compliance-authority-" + key),
		CreatedAt:         now, UpdatedAt: now,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
}

func (f complianceFixture) createProgram(t *testing.T) Program {
	t.Helper()
	program, err := f.service.CreateProgram(f.ctx, f.creatorID, CreateProgramInput{
		ProgramKey: "soc2-2026", Framework: "soc2_type2", ScopeVersion: "2026.1",
		ScopeSummary:           "Synara SaaS Control Plane, Web, remote execution, data services and production operations.",
		ExecutiveSponsorUserID: f.operatorIDs[4], AuditorOrganization: "Independent Auditor LLP",
		AuditorEngagementReference: "https://evidence.example.test/contracts/auditor",
		ObservationStart:           f.now.AddDate(0, 0, 1), ObservationEnd: f.now.AddDate(0, 6, 1),
		EvidenceRepositoryReference:   "https://evidence.example.test/repository/soc2-2026",
		EvidenceAccessPolicyReference: "https://evidence.example.test/policies/evidence-access",
		EvidenceRetentionDays:         365, VendorRegisterReference: "https://evidence.example.test/registers/vendors",
		RiskRegisterReference: "https://evidence.example.test/registers/risks",
	}, "create-program", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func (f complianceFixture) createRequiredControls(t *testing.T, program Program) []Control {
	t.Helper()
	controls := make([]Control, 0, len(requiredControlFamilies))
	for index, family := range requiredControlFamilies {
		updated, err := f.service.CreateControl(f.ctx, f.creatorID, program.ID, CreateControlInput{
			ControlID: "CTRL." + string(rune('A'+index)), Family: family,
			Title: "Control for " + family, Description: "This control defines an authoritative operating boundary for " + family + ".",
			OwnerUserID: f.operatorIDs[index%len(f.operatorIDs)], Cadence: "quarterly",
			EvidenceRequirement: "Retain a hashed, access-controlled and independently reviewed evidence sample.",
		}, "create-control-"+family, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		program = updated
	}
	for _, control := range program.Controls {
		controls = append(controls, control)
	}
	return controls
}

func (f complianceFixture) transition(t *testing.T, program Program, target string) Program {
	t.Helper()
	updated, err := f.service.Transition(f.ctx, f.creatorID, program.ID, TransitionInput{
		ExpectedVersion: program.Version, TargetState: target,
		Reason: "Advance after completing the bounded compliance record requirements.",
	}, "transition-"+target, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func assertComplianceProblem(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}

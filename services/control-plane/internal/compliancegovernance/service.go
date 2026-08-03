package compliancegovernance

import (
	"context"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var (
	programKeyPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)
	controlIDPattern      = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{1,79}$`)
	evidenceIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,119}$`)
	evidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

var requiredControlFamilies = []string{
	"logical_access", "change_release", "operations", "data_governance",
	"resilience", "vendor_provider", "security_testing",
}
var allowedCadences = []string{"continuous", "daily", "monthly", "quarterly", "annual", "per_release", "per_incident"}
var allowedEvidenceTypes = []string{
	"release_manifest", "access_review", "release", "incident", "recovery",
	"vendor_review", "security_test", "data_governance", "other",
}
var requiredDecisionRoles = []string{"security", "operations", "legal_privacy", "executive"}

type Program struct {
	ID                            uuid.UUID  `json:"id"`
	ProgramKey                    string     `json:"programKey"`
	Framework                     string     `json:"framework"`
	ScopeVersion                  string     `json:"scopeVersion"`
	ScopeSummary                  string     `json:"scopeSummary"`
	ExecutiveSponsorUserID        uuid.UUID  `json:"executiveSponsorUserId"`
	AuditorOrganization           string     `json:"auditorOrganization"`
	AuditorEngagementReference    string     `json:"auditorEngagementReference"`
	ObservationStart              time.Time  `json:"observationStart"`
	ObservationEnd                time.Time  `json:"observationEnd"`
	EvidenceRepositoryReference   string     `json:"evidenceRepositoryReference"`
	EvidenceAccessPolicyReference string     `json:"evidenceAccessPolicyReference"`
	EvidenceRetentionDays         int        `json:"evidenceRetentionDays"`
	VendorRegisterReference       string     `json:"vendorRegisterReference"`
	RiskRegisterReference         string     `json:"riskRegisterReference"`
	State                         string     `json:"state"`
	Version                       int64      `json:"version"`
	CreatedBy                     uuid.UUID  `json:"createdBy"`
	RecordCompletedAt             *time.Time `json:"recordCompletedAt"`
	Controls                      []Control  `json:"controls"`
	Decisions                     []Decision `json:"decisions"`
	Readiness                     Readiness  `json:"readiness"`
	CreatedAt                     time.Time  `json:"createdAt"`
	UpdatedAt                     time.Time  `json:"updatedAt"`
}

type Control struct {
	ID                  uuid.UUID  `json:"id"`
	ControlID           string     `json:"controlId"`
	Family              string     `json:"family"`
	Title               string     `json:"title"`
	Description         string     `json:"description"`
	OwnerUserID         uuid.UUID  `json:"ownerUserId"`
	Cadence             string     `json:"cadence"`
	EvidenceRequirement string     `json:"evidenceRequirement"`
	CreatedBy           uuid.UUID  `json:"createdBy"`
	Evidence            []Evidence `json:"evidence"`
	CreatedAt           time.Time  `json:"createdAt"`
}

type Evidence struct {
	ID              uuid.UUID `json:"id"`
	EvidenceID      string    `json:"evidenceId"`
	EvidenceType    string    `json:"evidenceType"`
	PeriodStart     time.Time `json:"periodStart"`
	PeriodEnd       time.Time `json:"periodEnd"`
	SourceReference string    `json:"sourceReference"`
	SHA256          string    `json:"sha256"`
	MediaType       string    `json:"mediaType"`
	Classification  string    `json:"classification"`
	CollectedAt     time.Time `json:"collectedAt"`
	RetentionUntil  time.Time `json:"retentionUntil"`
	SubmittedBy     uuid.UUID `json:"submittedBy"`
	Review          *Review   `json:"review"`
	CreatedAt       time.Time `json:"createdAt"`
}

type Review struct {
	ID                uuid.UUID `json:"id"`
	Decision          string    `json:"decision"`
	ReviewRole        string    `json:"reviewRole"`
	ReviewerUserID    uuid.UUID `json:"reviewerUserId"`
	Reason            string    `json:"reason"`
	EvidenceReference string    `json:"evidenceReference"`
	EvidenceSHA256    *string   `json:"evidenceSha256"`
	CreatedAt         time.Time `json:"createdAt"`
}

type Decision struct {
	ID                uuid.UUID  `json:"id"`
	DecisionRole      string     `json:"decisionRole"`
	Decision          string     `json:"decision"`
	DeciderUserID     uuid.UUID  `json:"deciderUserId"`
	Reason            string     `json:"reason"`
	EvidenceReference string     `json:"evidenceReference"`
	EvidenceSHA256    *string    `json:"evidenceSha256"`
	SupersededAt      *time.Time `json:"supersededAt"`
	SupersededReason  *string    `json:"supersededReason"`
	CreatedAt         time.Time  `json:"createdAt"`
}

type Readiness struct {
	Assessment                      string   `json:"assessment"`
	MissingControlFamilies          []string `json:"missingControlFamilies"`
	MissingDecisionRoles            []string `json:"missingDecisionRoles"`
	HasAcceptedReleaseManifest      bool     `json:"hasAcceptedReleaseManifest"`
	EligibleForRecordCompleteReview bool     `json:"eligibleForRecordCompleteReview"`
}

type CreateProgramInput struct {
	ProgramKey                    string    `json:"programKey"`
	Framework                     string    `json:"framework"`
	ScopeVersion                  string    `json:"scopeVersion"`
	ScopeSummary                  string    `json:"scopeSummary"`
	ExecutiveSponsorUserID        uuid.UUID `json:"executiveSponsorUserId"`
	AuditorOrganization           string    `json:"auditorOrganization"`
	AuditorEngagementReference    string    `json:"auditorEngagementReference"`
	ObservationStart              time.Time `json:"observationStart"`
	ObservationEnd                time.Time `json:"observationEnd"`
	EvidenceRepositoryReference   string    `json:"evidenceRepositoryReference"`
	EvidenceAccessPolicyReference string    `json:"evidenceAccessPolicyReference"`
	EvidenceRetentionDays         int       `json:"evidenceRetentionDays"`
	VendorRegisterReference       string    `json:"vendorRegisterReference"`
	RiskRegisterReference         string    `json:"riskRegisterReference"`
}

type CreateControlInput struct {
	ControlID           string    `json:"controlId"`
	Family              string    `json:"family"`
	Title               string    `json:"title"`
	Description         string    `json:"description"`
	OwnerUserID         uuid.UUID `json:"ownerUserId"`
	Cadence             string    `json:"cadence"`
	EvidenceRequirement string    `json:"evidenceRequirement"`
}

type SubmitEvidenceInput struct {
	ControlRecordID uuid.UUID `json:"controlRecordId"`
	EvidenceID      string    `json:"evidenceId"`
	EvidenceType    string    `json:"evidenceType"`
	PeriodStart     time.Time `json:"periodStart"`
	PeriodEnd       time.Time `json:"periodEnd"`
	SourceReference string    `json:"sourceReference"`
	SHA256          string    `json:"sha256"`
	MediaType       string    `json:"mediaType"`
	Classification  string    `json:"classification"`
	CollectedAt     time.Time `json:"collectedAt"`
	RetentionUntil  time.Time `json:"retentionUntil"`
}

type ReviewEvidenceInput struct {
	Decision          string `json:"decision"`
	ReviewRole        string `json:"reviewRole"`
	Reason            string `json:"reason"`
	EvidenceReference string `json:"evidenceReference"`
	EvidenceSHA256    string `json:"evidenceSha256"`
}

type DecisionInput struct {
	DecisionRole      string `json:"decisionRole"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	EvidenceReference string `json:"evidenceReference"`
	EvidenceSHA256    string `json:"evidenceSha256"`
}

type TransitionInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	TargetState     string `json:"targetState"`
	Reason          string `json:"reason"`
}

type Service struct {
	db               *gorm.DB
	operatorTenantID uuid.UUID
	now              func() time.Time
	authority        *governanceauthority.Service
}

func NewService(db *gorm.DB, operatorTenantID uuid.UUID) *Service {
	return &Service{db: db, operatorTenantID: operatorTenantID, now: func() time.Time { return time.Now().UTC() }, authority: governanceauthority.NewService(db, operatorTenantID)}
}

func (s *Service) List(ctx context.Context) ([]Program, error) {
	var models []persistence.Stage6ComplianceProgram
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(50).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "compliance_programs_load_failed", "Compliance programs could not be loaded.", err)
	}
	items := make([]Program, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) CreateProgram(ctx context.Context, actorID uuid.UUID, input CreateProgramInput, requestID, ipAddress string) (Program, error) {
	model, err := s.normalizeProgram(actorID, input)
	if err != nil {
		return Program{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "compliance_program_exists", "This compliance program already exists.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_program_create_failed", "Compliance program could not be created.", err)
		}
		return s.recordAudit(ctx, tx, actorID, "compliance.program_created", "stage6_compliance_program", model.ID, requestID, ipAddress, map[string]any{
			"programKey": model.ProgramKey, "framework": model.Framework, "scopeVersion": model.ScopeVersion,
		})
	})
	if err != nil {
		return Program{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) CreateControl(ctx context.Context, actorID, programID uuid.UUID, input CreateControlInput, requestID, ipAddress string) (Program, error) {
	model, err := s.normalizeControl(actorID, programID, input)
	if err != nil {
		return Program{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := s.lockProgram(ctx, tx, programID, false); err != nil {
			return err
		}
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "compliance_control_exists", "This compliance control already exists.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_control_create_failed", "Compliance control could not be created.", err)
		}
		return s.recordAudit(ctx, tx, actorID, "compliance.control_created", "stage6_compliance_control", model.ID, requestID, ipAddress, map[string]any{
			"programId": programID, "controlId": model.ControlID, "family": model.Family, "ownerUserId": model.OwnerUserID,
		})
	})
	if err != nil {
		return Program{}, err
	}
	return s.get(ctx, programID)
}

func (s *Service) SubmitEvidence(ctx context.Context, actorID, programID uuid.UUID, input SubmitEvidenceInput, requestID, ipAddress string) (Program, error) {
	model, err := s.normalizeEvidence(actorID, programID, input)
	if err != nil {
		return Program{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		program, err := s.lockProgram(ctx, tx, programID, false)
		if err != nil {
			return err
		}
		minimumRetention := model.CollectedAt.AddDate(0, 0, program.EvidenceRetentionDays)
		if model.RetentionUntil.Before(minimumRetention) {
			return problem.New(400, "compliance_evidence_retention_invalid", "Evidence retention is shorter than the program policy.")
		}
		var control persistence.Stage6ComplianceControl
		if err := tx.Where("id = ? AND program_id = ? AND operator_tenant_id = ?", model.ControlRecordID, programID, s.operatorTenantID).First(&control).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "compliance_control_not_found", "Compliance control was not found.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_control_load_failed", "Compliance control could not be loaded.", err)
		}
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "compliance_evidence_exists", "This compliance evidence ID already exists.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_evidence_create_failed", "Compliance evidence could not be submitted.", err)
		}
		return s.recordAudit(ctx, tx, actorID, "compliance.evidence_submitted", "stage6_compliance_evidence", model.ID, requestID, ipAddress, map[string]any{
			"programId": programID, "controlId": control.ControlID, "evidenceId": model.EvidenceID,
			"evidenceType": model.EvidenceType, "sha256": hex.EncodeToString(model.SHA256), "sourceReference": model.SourceReference,
		})
	})
	if err != nil {
		return Program{}, err
	}
	return s.get(ctx, programID)
}

func (s *Service) ReviewEvidence(ctx context.Context, actorID, programID, evidenceRecordID uuid.UUID, input ReviewEvidenceInput, requestID, ipAddress string) (Program, error) {
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.ReviewRole = strings.ToLower(strings.TrimSpace(input.ReviewRole))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains([]string{"accepted", "rejected"}, input.Decision) ||
		!slices.Contains([]string{"security", "operations", "legal_privacy", "auditor"}, input.ReviewRole) ||
		len(input.Reason) < 10 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validEvidenceSHA256(input.EvidenceSHA256) {
		return Program{}, problem.New(400, "compliance_evidence_review_invalid", "Evidence review requires a valid role, decision, reason, HTTPS reference and non-zero SHA-256.")
	}
	review := persistence.Stage6ComplianceEvidenceReview{
		ID: uuid.New(), EvidenceRecordID: evidenceRecordID, ProgramID: programID, OperatorTenantID: s.operatorTenantID,
		Decision: input.Decision, ReviewRole: input.ReviewRole, ReviewerUserID: actorID,
		Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := s.lockProgram(ctx, tx, programID, false); err != nil {
			return err
		}
		var evidence persistence.Stage6ComplianceEvidence
		if err := tx.Where("id = ? AND program_id = ? AND operator_tenant_id = ?", evidenceRecordID, programID, s.operatorTenantID).First(&evidence).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "compliance_evidence_not_found", "Compliance evidence was not found.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_evidence_load_failed", "Compliance evidence could not be loaded.", err)
		}
		if evidence.SubmittedBy == actorID {
			return problem.New(409, "compliance_evidence_self_review_forbidden", "Evidence submitter cannot review the same evidence.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.ComplianceEvidencePrefix+input.ReviewRole); err != nil {
			return err
		}
		if err := tx.Create(&review).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "compliance_evidence_already_reviewed", "This evidence already has an immutable review.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_evidence_review_failed", "Compliance evidence review could not be recorded.", err)
		}
		return s.recordAudit(ctx, tx, actorID, "compliance.evidence_reviewed", "stage6_compliance_evidence", evidence.ID, requestID, ipAddress, map[string]any{
			"programId": programID, "evidenceId": evidence.EvidenceID, "decision": review.Decision, "reviewRole": review.ReviewRole, "evidenceSha256": review.EvidenceSHA256,
		})
	})
	if err != nil {
		return Program{}, err
	}
	return s.get(ctx, programID)
}

func (s *Service) RecordDecision(ctx context.Context, actorID, programID uuid.UUID, input DecisionInput, requestID, ipAddress string) (Program, error) {
	input.DecisionRole = strings.ToLower(strings.TrimSpace(input.DecisionRole))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(requiredDecisionRoles, input.DecisionRole) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) ||
		len(input.Reason) < 10 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validEvidenceSHA256(input.EvidenceSHA256) {
		return Program{}, problem.New(400, "compliance_decision_invalid", "Compliance decision requires a valid role, decision, reason, HTTPS reference and non-zero SHA-256.")
	}
	decision := persistence.Stage6ComplianceProgramDecision{
		ID: uuid.New(), ProgramID: programID, OperatorTenantID: s.operatorTenantID,
		DecisionRole: input.DecisionRole, Decision: input.Decision, DeciderUserID: actorID,
		Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		program, err := s.lockProgram(ctx, tx, programID, true)
		if err != nil {
			return err
		}
		if program.CreatedBy == actorID {
			return problem.New(409, "compliance_decision_self_review_forbidden", "Program creator cannot approve the same start gate.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, "compliance."+input.DecisionRole); err != nil {
			return err
		}
		if err := tx.Create(&decision).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "compliance_decision_conflict", "This role or operator already recorded a decision.")
		} else if err != nil {
			return problem.Wrap(500, "compliance_decision_create_failed", "Compliance decision could not be recorded.", err)
		}
		return s.recordAudit(ctx, tx, actorID, "compliance.start_gate_decision_recorded", "stage6_compliance_program", program.ID, requestID, ipAddress, map[string]any{
			"programKey": program.ProgramKey, "decisionRole": decision.DecisionRole, "decision": decision.Decision, "evidenceSha256": decision.EvidenceSHA256,
		})
	})
	if err != nil {
		return Program{}, err
	}
	return s.get(ctx, programID)
}

func (s *Service) Transition(ctx context.Context, actorID, programID uuid.UUID, input TransitionInput, requestID, ipAddress string) (Program, error) {
	input.TargetState = strings.ToLower(strings.TrimSpace(input.TargetState))
	input.Reason = strings.TrimSpace(input.Reason)
	if input.ExpectedVersion <= 0 || !slices.Contains([]string{"ready_for_review", "record_complete"}, input.TargetState) || len(input.Reason) < 10 || len(input.Reason) > 1000 {
		return Program{}, problem.New(400, "compliance_transition_invalid", "Compliance transition requires a version, valid target and bounded reason.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		program, err := s.lockProgram(ctx, tx, programID, false)
		if err != nil {
			return err
		}
		if program.Version != input.ExpectedVersion ||
			(program.State == "draft" && input.TargetState != "ready_for_review") ||
			(program.State == "ready_for_review" && input.TargetState != "record_complete") ||
			program.State == "record_complete" {
			return problem.New(409, "compliance_program_version_conflict", "Compliance program state or version changed.")
		}
		if input.TargetState == "record_complete" {
			readiness, err := s.evaluateReadiness(tx, program.ID)
			if err != nil {
				return err
			}
			if !readiness.EligibleForRecordCompleteReview {
				return problem.New(409, "compliance_start_gate_incomplete", "All control families, four approvals and an accepted release manifest are required.")
			}
		}
		now := s.now()
		updates := map[string]any{"state": input.TargetState, "version": program.Version + 1, "updated_at": now}
		if input.TargetState == "record_complete" {
			updates["record_completed_at"] = now
		}
		result := tx.Model(&persistence.Stage6ComplianceProgram{}).
			Where("id = ? AND operator_tenant_id = ? AND version = ? AND state = ?", program.ID, s.operatorTenantID, program.Version, program.State).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "compliance_program_transition_failed", "Compliance program could not be transitioned.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "compliance_program_version_conflict", "Compliance program changed before transition.")
		}
		return s.recordAudit(ctx, tx, actorID, "compliance.program_transitioned", "stage6_compliance_program", program.ID, requestID, ipAddress, map[string]any{
			"programKey": program.ProgramKey, "from": program.State, "to": input.TargetState, "reason": input.Reason,
			"assessment": "record-complete-not-audit-active",
		})
	})
	if err != nil {
		return Program{}, err
	}
	return s.get(ctx, programID)
}

func (s *Service) normalizeProgram(actorID uuid.UUID, input CreateProgramInput) (persistence.Stage6ComplianceProgram, error) {
	input.ProgramKey = strings.ToLower(strings.TrimSpace(input.ProgramKey))
	input.Framework = strings.ToLower(strings.TrimSpace(input.Framework))
	input.ScopeVersion = strings.TrimSpace(input.ScopeVersion)
	input.ScopeSummary = strings.TrimSpace(input.ScopeSummary)
	input.AuditorOrganization = strings.TrimSpace(input.AuditorOrganization)
	refs := []*string{&input.AuditorEngagementReference, &input.EvidenceRepositoryReference, &input.EvidenceAccessPolicyReference, &input.VendorRegisterReference, &input.RiskRegisterReference}
	for _, ref := range refs {
		*ref = strings.TrimSpace(*ref)
	}
	if actorID == uuid.Nil || input.ExecutiveSponsorUserID == uuid.Nil || !programKeyPattern.MatchString(input.ProgramKey) ||
		!slices.Contains([]string{"soc2_type2", "iso27001"}, input.Framework) || len(input.ScopeVersion) < 1 || len(input.ScopeVersion) > 80 ||
		len(input.ScopeSummary) < 20 || len(input.ScopeSummary) > 4000 || len(input.AuditorOrganization) < 2 || len(input.AuditorOrganization) > 300 ||
		input.ObservationStart.IsZero() || !input.ObservationEnd.After(input.ObservationStart) || input.EvidenceRetentionDays < 365 || input.EvidenceRetentionDays > 3650 {
		return persistence.Stage6ComplianceProgram{}, problem.New(400, "compliance_program_invalid", "Compliance program identity, scope or observation window is invalid.")
	}
	for _, ref := range refs {
		if !validHTTPSReference(*ref) {
			return persistence.Stage6ComplianceProgram{}, problem.New(400, "compliance_program_reference_invalid", "Compliance program references must be absolute HTTPS URLs without credentials.")
		}
	}
	now := s.now()
	return persistence.Stage6ComplianceProgram{
		ID: uuid.New(), OperatorTenantID: s.operatorTenantID, ProgramKey: input.ProgramKey, Framework: input.Framework,
		ScopeVersion: input.ScopeVersion, ScopeSummary: input.ScopeSummary, ExecutiveSponsorUserID: input.ExecutiveSponsorUserID,
		AuditorOrganization: input.AuditorOrganization, AuditorEngagementReference: input.AuditorEngagementReference,
		ObservationStart: input.ObservationStart.UTC(), ObservationEnd: input.ObservationEnd.UTC(),
		EvidenceRepositoryReference: input.EvidenceRepositoryReference, EvidenceAccessPolicyReference: input.EvidenceAccessPolicyReference,
		EvidenceRetentionDays: input.EvidenceRetentionDays, VendorRegisterReference: input.VendorRegisterReference,
		RiskRegisterReference: input.RiskRegisterReference, State: "draft", Version: 1, CreatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Service) normalizeControl(actorID, programID uuid.UUID, input CreateControlInput) (persistence.Stage6ComplianceControl, error) {
	input.ControlID = strings.ToUpper(strings.TrimSpace(input.ControlID))
	input.Family = strings.ToLower(strings.TrimSpace(input.Family))
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	input.Cadence = strings.ToLower(strings.TrimSpace(input.Cadence))
	input.EvidenceRequirement = strings.TrimSpace(input.EvidenceRequirement)
	if actorID == uuid.Nil || programID == uuid.Nil || input.OwnerUserID == uuid.Nil || !controlIDPattern.MatchString(input.ControlID) ||
		!slices.Contains(requiredControlFamilies, input.Family) || !slices.Contains(allowedCadences, input.Cadence) ||
		len(input.Title) < 3 || len(input.Title) > 200 || len(input.Description) < 20 || len(input.Description) > 4000 ||
		len(input.EvidenceRequirement) < 20 || len(input.EvidenceRequirement) > 4000 {
		return persistence.Stage6ComplianceControl{}, problem.New(400, "compliance_control_invalid", "Compliance control identity, owner, cadence or evidence requirement is invalid.")
	}
	return persistence.Stage6ComplianceControl{
		ID: uuid.New(), ProgramID: programID, OperatorTenantID: s.operatorTenantID, ControlID: input.ControlID,
		Family: input.Family, Title: input.Title, Description: input.Description, OwnerUserID: input.OwnerUserID,
		Cadence: input.Cadence, EvidenceRequirement: input.EvidenceRequirement, CreatedBy: actorID, CreatedAt: s.now(),
	}, nil
}

func (s *Service) normalizeEvidence(actorID, programID uuid.UUID, input SubmitEvidenceInput) (persistence.Stage6ComplianceEvidence, error) {
	input.EvidenceID = strings.TrimSpace(input.EvidenceID)
	input.EvidenceType = strings.ToLower(strings.TrimSpace(input.EvidenceType))
	input.SourceReference = strings.TrimSpace(input.SourceReference)
	input.MediaType = strings.ToLower(strings.TrimSpace(input.MediaType))
	input.Classification = strings.ToLower(strings.TrimSpace(input.Classification))
	digest, err := parseSHA256(input.SHA256)
	if err != nil {
		return persistence.Stage6ComplianceEvidence{}, err
	}
	if actorID == uuid.Nil || programID == uuid.Nil || input.ControlRecordID == uuid.Nil || !evidenceIDPattern.MatchString(input.EvidenceID) ||
		!slices.Contains(allowedEvidenceTypes, input.EvidenceType) || !validHTTPSReference(input.SourceReference) ||
		len(input.MediaType) < 3 || len(input.MediaType) > 120 || !slices.Contains([]string{"internal", "confidential", "restricted"}, input.Classification) ||
		input.PeriodStart.IsZero() || input.PeriodEnd.Before(input.PeriodStart) || input.CollectedAt.IsZero() || input.RetentionUntil.IsZero() {
		return persistence.Stage6ComplianceEvidence{}, problem.New(400, "compliance_evidence_invalid", "Compliance evidence subject, period, reference or classification is invalid.")
	}
	return persistence.Stage6ComplianceEvidence{
		ID: uuid.New(), ProgramID: programID, ControlRecordID: input.ControlRecordID, OperatorTenantID: s.operatorTenantID,
		EvidenceID: input.EvidenceID, EvidenceType: input.EvidenceType, PeriodStart: input.PeriodStart.UTC(), PeriodEnd: input.PeriodEnd.UTC(),
		SourceReference: input.SourceReference, SHA256: digest, MediaType: input.MediaType, Classification: input.Classification,
		CollectedAt: input.CollectedAt.UTC(), RetentionUntil: input.RetentionUntil.UTC(), SubmittedBy: actorID, CreatedAt: s.now(),
	}, nil
}

func (s *Service) lockProgram(ctx context.Context, tx *gorm.DB, programID uuid.UUID, requireReview bool) (persistence.Stage6ComplianceProgram, error) {
	var program persistence.Stage6ComplianceProgram
	query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND operator_tenant_id = ?", programID, s.operatorTenantID)
	if err := query.First(&program).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return program, problem.New(404, "compliance_program_not_found", "Compliance program was not found.")
	} else if err != nil {
		return program, problem.Wrap(500, "compliance_program_load_failed", "Compliance program could not be loaded.", err)
	}
	if program.State == "record_complete" || (requireReview && program.State != "ready_for_review") {
		return program, problem.New(409, "compliance_program_not_open", "Compliance program is not open for this operation.")
	}
	return program, nil
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Program, error) {
	var model persistence.Stage6ComplianceProgram
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).First(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Program{}, problem.New(404, "compliance_program_not_found", "Compliance program was not found.")
	} else if err != nil {
		return Program{}, problem.Wrap(500, "compliance_program_load_failed", "Compliance program could not be loaded.", err)
	}
	return s.view(ctx, model)
}

func (s *Service) view(ctx context.Context, model persistence.Stage6ComplianceProgram) (Program, error) {
	var controls []persistence.Stage6ComplianceControl
	if err := s.db.WithContext(ctx).Where("program_id = ?", model.ID).Order("control_family, control_id").Find(&controls).Error; err != nil {
		return Program{}, problem.Wrap(500, "compliance_controls_load_failed", "Compliance controls could not be loaded.", err)
	}
	controlViews := make([]Control, 0, len(controls))
	for _, control := range controls {
		var evidenceModels []persistence.Stage6ComplianceEvidence
		if err := s.db.WithContext(ctx).Where("control_record_id = ?", control.ID).Order("period_end DESC, id").Find(&evidenceModels).Error; err != nil {
			return Program{}, problem.Wrap(500, "compliance_evidence_load_failed", "Compliance evidence could not be loaded.", err)
		}
		evidenceViews := make([]Evidence, 0, len(evidenceModels))
		for _, evidence := range evidenceModels {
			var reviewModel persistence.Stage6ComplianceEvidenceReview
			var review *Review
			if err := s.db.WithContext(ctx).Where("evidence_record_id = ? AND superseded_at IS NULL", evidence.ID).First(&reviewModel).Error; err == nil {
				review = &Review{ID: reviewModel.ID, Decision: reviewModel.Decision, ReviewRole: reviewModel.ReviewRole, ReviewerUserID: reviewModel.ReviewerUserID, Reason: reviewModel.Reason, EvidenceReference: reviewModel.EvidenceReference, EvidenceSHA256: reviewModel.EvidenceSHA256, CreatedAt: reviewModel.CreatedAt}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return Program{}, problem.Wrap(500, "compliance_evidence_review_load_failed", "Compliance evidence review could not be loaded.", err)
			}
			evidenceViews = append(evidenceViews, Evidence{ID: evidence.ID, EvidenceID: evidence.EvidenceID, EvidenceType: evidence.EvidenceType, PeriodStart: evidence.PeriodStart, PeriodEnd: evidence.PeriodEnd, SourceReference: evidence.SourceReference, SHA256: hex.EncodeToString(evidence.SHA256), MediaType: evidence.MediaType, Classification: evidence.Classification, CollectedAt: evidence.CollectedAt, RetentionUntil: evidence.RetentionUntil, SubmittedBy: evidence.SubmittedBy, Review: review, CreatedAt: evidence.CreatedAt})
		}
		controlViews = append(controlViews, Control{ID: control.ID, ControlID: control.ControlID, Family: control.Family, Title: control.Title, Description: control.Description, OwnerUserID: control.OwnerUserID, Cadence: control.Cadence, EvidenceRequirement: control.EvidenceRequirement, CreatedBy: control.CreatedBy, Evidence: evidenceViews, CreatedAt: control.CreatedAt})
	}
	var decisionModels []persistence.Stage6ComplianceProgramDecision
	if err := s.db.WithContext(ctx).Where("program_id = ?", model.ID).Order("created_at, id").Find(&decisionModels).Error; err != nil {
		return Program{}, problem.Wrap(500, "compliance_decisions_load_failed", "Compliance decisions could not be loaded.", err)
	}
	decisions := make([]Decision, 0, len(decisionModels))
	for _, decision := range decisionModels {
		decisions = append(decisions, Decision{ID: decision.ID, DecisionRole: decision.DecisionRole, Decision: decision.Decision, DeciderUserID: decision.DeciderUserID, Reason: decision.Reason, EvidenceReference: decision.EvidenceReference, EvidenceSHA256: decision.EvidenceSHA256, SupersededAt: decision.SupersededAt, SupersededReason: decision.SupersededReason, CreatedAt: decision.CreatedAt})
	}
	readiness := evaluateViews(controlViews, decisions)
	if model.State == "record_complete" {
		readiness.Assessment = "record-complete-not-audit-active"
	}
	return Program{
		ID: model.ID, ProgramKey: model.ProgramKey, Framework: model.Framework, ScopeVersion: model.ScopeVersion,
		ScopeSummary: model.ScopeSummary, ExecutiveSponsorUserID: model.ExecutiveSponsorUserID,
		AuditorOrganization: model.AuditorOrganization, AuditorEngagementReference: model.AuditorEngagementReference,
		ObservationStart: model.ObservationStart, ObservationEnd: model.ObservationEnd,
		EvidenceRepositoryReference: model.EvidenceRepositoryReference, EvidenceAccessPolicyReference: model.EvidenceAccessPolicyReference,
		EvidenceRetentionDays: model.EvidenceRetentionDays, VendorRegisterReference: model.VendorRegisterReference,
		RiskRegisterReference: model.RiskRegisterReference, State: model.State, Version: model.Version, CreatedBy: model.CreatedBy,
		RecordCompletedAt: model.RecordCompletedAt, Controls: controlViews, Decisions: decisions, Readiness: readiness,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

func (s *Service) evaluateReadiness(tx *gorm.DB, programID uuid.UUID) (Readiness, error) {
	var controls []persistence.Stage6ComplianceControl
	if err := tx.Where("program_id = ?", programID).Find(&controls).Error; err != nil {
		return Readiness{}, problem.Wrap(500, "compliance_readiness_load_failed", "Compliance readiness could not be evaluated.", err)
	}
	controlViews := make([]Control, 0, len(controls))
	for _, control := range controls {
		var evidenceModels []persistence.Stage6ComplianceEvidence
		if err := tx.Where("control_record_id = ?", control.ID).Find(&evidenceModels).Error; err != nil {
			return Readiness{}, problem.Wrap(500, "compliance_readiness_load_failed", "Compliance readiness could not be evaluated.", err)
		}
		evidenceViews := make([]Evidence, 0, len(evidenceModels))
		for _, evidence := range evidenceModels {
			var reviewModel persistence.Stage6ComplianceEvidenceReview
			var review *Review
			if err := tx.Where("evidence_record_id = ? AND superseded_at IS NULL", evidence.ID).First(&reviewModel).Error; err == nil {
				review = &Review{Decision: reviewModel.Decision, EvidenceSHA256: reviewModel.EvidenceSHA256}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return Readiness{}, problem.Wrap(500, "compliance_readiness_load_failed", "Compliance readiness could not be evaluated.", err)
			}
			evidenceViews = append(evidenceViews, Evidence{EvidenceType: evidence.EvidenceType, Review: review})
		}
		controlViews = append(controlViews, Control{Family: control.Family, Evidence: evidenceViews})
	}
	var decisionModels []persistence.Stage6ComplianceProgramDecision
	if err := tx.Where("program_id = ?", programID).Find(&decisionModels).Error; err != nil {
		return Readiness{}, problem.Wrap(500, "compliance_readiness_load_failed", "Compliance readiness could not be evaluated.", err)
	}
	decisions := make([]Decision, 0, len(decisionModels))
	for _, decision := range decisionModels {
		decisions = append(decisions, Decision{DecisionRole: decision.DecisionRole, Decision: decision.Decision, EvidenceSHA256: decision.EvidenceSHA256, SupersededAt: decision.SupersededAt})
	}
	return evaluateViews(controlViews, decisions), nil
}

func evaluateViews(controls []Control, decisions []Decision) Readiness {
	families := map[string]bool{}
	hasManifest := false
	for _, control := range controls {
		families[control.Family] = true
		for _, evidence := range control.Evidence {
			if evidence.EvidenceType == "release_manifest" && evidence.Review != nil && evidence.Review.Decision == "accepted" && evidence.Review.EvidenceSHA256 != nil && validEvidenceSHA256(*evidence.Review.EvidenceSHA256) {
				hasManifest = true
			}
		}
	}
	missingFamilies := make([]string, 0)
	for _, family := range requiredControlFamilies {
		if !families[family] {
			missingFamilies = append(missingFamilies, family)
		}
	}
	approvedRoles := map[string]bool{}
	for _, decision := range decisions {
		if decision.Decision == "approved" && decision.SupersededAt == nil && decision.EvidenceSHA256 != nil && validEvidenceSHA256(*decision.EvidenceSHA256) {
			approvedRoles[decision.DecisionRole] = true
		}
	}
	missingRoles := make([]string, 0)
	for _, role := range requiredDecisionRoles {
		if !approvedRoles[role] {
			missingRoles = append(missingRoles, role)
		}
	}
	eligible := len(missingFamilies) == 0 && len(missingRoles) == 0 && hasManifest
	return Readiness{
		Assessment: "record-incomplete-not-audit-active", MissingControlFamilies: missingFamilies,
		MissingDecisionRoles: missingRoles, HasAcceptedReleaseManifest: hasManifest,
		EligibleForRecordCompleteReview: eligible,
	}
}

func (s *Service) recordAudit(ctx context.Context, tx *gorm.DB, actorID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestID, ipAddress string, metadata map[string]any) error {
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: action,
		ResourceType: resourceType, ResourceID: &resourceID, RequestID: requestID, IPAddress: ipAddress, Metadata: metadata,
	})
}

func validHTTPSReference(raw string) bool {
	if len(raw) < 8 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.RawQuery == "" && parsed.Fragment == ""
}

func parseSHA256(raw string) ([]byte, error) {
	raw = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "sha256:")
	digest, err := hex.DecodeString(raw)
	if err != nil || len(digest) != 32 {
		return nil, problem.New(400, "compliance_evidence_sha256_invalid", "Compliance evidence SHA-256 is invalid.")
	}
	return digest, nil
}

func validEvidenceSHA256(value string) bool {
	return evidenceSHA256Pattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}

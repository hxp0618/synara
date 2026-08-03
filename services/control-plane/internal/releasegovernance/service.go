package releasegovernance

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var (
	candidateIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,199}$`)
	hex40Pattern           = regexp.MustCompile(`^[0-9a-f]{40}$`)
	riskIDPattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)
	sha256ReferencePattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

var requiredApprovalRoles = []string{"engineering", "operations", "security", "product"}
var allowedApprovalRoles = append(slices.Clone(requiredApprovalRoles), "privacy_legal")
var allowedImpactDomains = []string{
	"code_change", "data_migration", "data_residency", "internal_cost",
	"desktop_distribution", "personal_data", "provider_commercial", "regulated_customer",
	"retention_legal_hold", "runtime_isolation", "security_incident",
}
var privacyLegalImpactDomains = []string{
	"data_residency", "personal_data", "provider_commercial",
	"regulated_customer", "retention_legal_hold", "security_incident",
}
var stateTransitions = map[string][]string{
	"draft":            {"ready_for_review"},
	"ready_for_review": {"approved"},
	"approved":         {"deploying"},
	"deploying":        {"observing", "rolled_back"},
	"observing":        {"released", "rolled_back"},
}

type ResidualRisk struct {
	ID                string    `json:"id"`
	Summary           string    `json:"summary"`
	Owner             string    `json:"owner"`
	DueAt             time.Time `json:"dueAt"`
	AcceptanceReason  string    `json:"acceptanceReason"`
	EvidenceReference string    `json:"evidenceReference"`
}

type Approval struct {
	ID                uuid.UUID `json:"id"`
	Role              string    `json:"role"`
	Decision          string    `json:"decision"`
	ApproverUserID    uuid.UUID `json:"approverUserId"`
	ApproverEmail     string    `json:"approverEmail"`
	ApproverName      string    `json:"approverName"`
	Reason            string    `json:"reason"`
	EvidenceReference string    `json:"evidenceReference"`
	EvidenceSHA256    *string   `json:"evidenceSha256"`
	CreatedAt         time.Time `json:"createdAt"`
}

type Candidate struct {
	ID                               uuid.UUID                                `json:"id"`
	CandidateID                      string                                   `json:"candidateId"`
	SourceCommit                     string                                   `json:"sourceCommit"`
	LockfileSHA256                   string                                   `json:"lockfileSha256"`
	EvidenceBundleSHA256             string                                   `json:"evidenceBundleSha256"`
	EvidenceBundleSchema             string                                   `json:"evidenceBundleSchema"`
	EvidenceBundleAssessment         string                                   `json:"evidenceBundleAssessment"`
	EvidenceBundleValidatedAt        string                                   `json:"evidenceBundleValidatedAt"`
	EvidenceBundleReceiptSizeBytes   int64                                    `json:"evidenceBundleReceiptSizeBytes"`
	DesktopArtifactSetSHA256         string                                   `json:"desktopArtifactSetSha256"`
	EvidenceReceiptBound             bool                                     `json:"evidenceReceiptBound"`
	FinalAssetSetSHA256              string                                   `json:"finalAssetSetSha256"`
	EnvironmentID                    string                                   `json:"environmentId"`
	ImpactDomains                    []string                                 `json:"impactDomains"`
	ProviderCommercialAuthorizations []ProviderCommercialAuthorizationBinding `json:"providerCommercialAuthorizations"`
	PrivacyLegalRequired             bool                                     `json:"privacyLegalRequired"`
	RequiredApprovalRoles            []string                                 `json:"requiredApprovalRoles"`
	State                            string                                   `json:"state"`
	Version                          int64                                    `json:"version"`
	CreatedBy                        uuid.UUID                                `json:"createdBy"`
	DecisionSummary                  *string                                  `json:"decisionSummary"`
	ResidualRiskDisposition          *string                                  `json:"residualRiskDisposition"`
	ResidualRisks                    []ResidualRisk                           `json:"residualRisks"`
	Approvals                        []Approval                               `json:"approvals"`
	FinalReview                      *FinalReview                             `json:"finalReview"`
	ApprovedAt                       *time.Time                               `json:"approvedAt"`
	ReleasedAt                       *time.Time                               `json:"releasedAt"`
	RejectedAt                       *time.Time                               `json:"rejectedAt"`
	RolledBackAt                     *time.Time                               `json:"rolledBackAt"`
	CreatedAt                        time.Time                                `json:"createdAt"`
	UpdatedAt                        time.Time                                `json:"updatedAt"`
}

type CreateInput struct {
	CandidateID                        string      `json:"candidateId"`
	SourceCommit                       string      `json:"sourceCommit"`
	LockfileSHA256                     string      `json:"lockfileSha256"`
	EvidenceBundleSHA256               string      `json:"evidenceBundleSha256"`
	EvidenceBundleReceiptBase64        string      `json:"evidenceBundleReceiptBase64"`
	FinalAssetSetSHA256                string      `json:"finalAssetSetSha256"`
	EnvironmentID                      string      `json:"environmentId"`
	ImpactDomains                      []string    `json:"impactDomains"`
	ProviderCommercialAuthorizationIDs []uuid.UUID `json:"providerCommercialAuthorizationIds"`
}

type ProviderCommercialAuthorizationBinding struct {
	AuthorizationID      uuid.UUID `json:"authorizationId"`
	Provider             string    `json:"provider"`
	AuthorizationKey     string    `json:"authorizationKey"`
	AuthorizationVersion int64     `json:"authorizationVersion"`
	CurrentVersion       int64     `json:"currentVersion"`
	CurrentState         string    `json:"currentState"`
	ReviewExpiresAt      time.Time `json:"reviewExpiresAt"`
	BindingValid         bool      `json:"bindingValid"`
	BoundAt              time.Time `json:"boundAt"`
}

type ApprovalInput struct {
	Role              string `json:"role"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	EvidenceReference string `json:"evidenceReference"`
	EvidenceSHA256    string `json:"evidenceSha256"`
}

type FinalReview struct {
	ID                                   uuid.UUID      `json:"id"`
	ReceiptSHA256                        string         `json:"receiptSha256"`
	SchemaVersion                        string         `json:"schemaVersion"`
	Assessment                           string         `json:"assessment"`
	ValidatedAt                          time.Time      `json:"validatedAt"`
	ReceiptSizeBytes                     int64          `json:"receiptSizeBytes"`
	ControlInventorySHA256               string         `json:"controlInventorySha256"`
	ControlCount                         int            `json:"controlCount"`
	FinalApprovalCount                   int            `json:"finalApprovalCount"`
	DecisionSummary                      string         `json:"decisionSummary"`
	ResidualRiskDisposition              string         `json:"residualRiskDisposition"`
	ResidualRisks                        []ResidualRisk `json:"residualRisks"`
	AllRequiredControlsPassed            bool           `json:"allRequiredControlsPassed"`
	AllRequiredFinalApprovalsApproved    bool           `json:"allRequiredFinalApprovalsApproved"`
	EligibleForExternalGAAuthorityReview bool           `json:"eligibleForExternalGAAuthorityReview"`
	BoundBy                              uuid.UUID      `json:"boundBy"`
	CreatedAt                            time.Time      `json:"createdAt"`
}

type FinalReviewInput struct {
	ReceiptSHA256 string `json:"receiptSha256"`
	ReceiptBase64 string `json:"receiptBase64"`
}

type TransitionInput struct {
	ExpectedVersion         int64          `json:"expectedVersion"`
	TargetState             string         `json:"targetState"`
	Reason                  string         `json:"reason"`
	DecisionSummary         *string        `json:"decisionSummary"`
	ResidualRiskDisposition *string        `json:"residualRiskDisposition"`
	ResidualRisks           []ResidualRisk `json:"residualRisks"`
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

func (s *Service) List(ctx context.Context) ([]Candidate, error) {
	var models []persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "release_candidates_load_failed", "Release candidates could not be loaded.", err)
	}
	return s.views(ctx, models)
}

func (s *Service) Create(
	ctx context.Context,
	actorID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (Candidate, error) {
	model, err := s.normalizeCreate(actorID, input)
	if err != nil {
		return Candidate{}, err
	}
	authorizationIDs, err := normalizeProviderCommercialAuthorizationIDs(
		input.ProviderCommercialAuthorizationIDs,
		slices.Contains(model.ImpactDomains, "provider_commercial"),
	)
	if err != nil {
		return Candidate{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "release_candidate_exists", "This release candidate already exists.")
		} else if err != nil {
			return problem.Wrap(500, "release_candidate_create_failed", "Release candidate could not be created.", err)
		}
		if err := s.bindProviderCommercialAuthorizations(ctx, tx, model, authorizationIDs); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "release.candidate_created", ResourceType: "stage6_release_candidate", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"candidateId": model.CandidateID, "sourceCommit": model.SourceCommit,
				"environmentId": model.EnvironmentID, "impactDomains": model.ImpactDomains,
				"providerCommercialAuthorizationIds": authorizationIDs,
				"privacyLegalRequired":               model.PrivacyLegalRequired,
				"evidenceBundleSchema":               model.EvidenceBundleSchema,
				"evidenceBundleAssessment":           model.EvidenceBundleAssessment,
				"evidenceBundleValidatedAt":          model.EvidenceBundleValidatedAt,
				"evidenceBundleReceiptSizeBytes":     model.EvidenceBundleReceiptSizeBytes,
				"desktopArtifactSetSha256":           model.DesktopArtifactSetSHA256,
			},
		})
	})
	if err != nil {
		return Candidate{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(
	ctx context.Context,
	actorID, candidateRecordID uuid.UUID,
	input ApprovalInput,
	requestID, ipAddress string,
) (Candidate, error) {
	input.Role = strings.TrimSpace(strings.ToLower(input.Role))
	input.Decision = strings.TrimSpace(strings.ToLower(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(allowedApprovalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) {
		return Candidate{}, problem.New(400, "release_approval_invalid", "Release approval role or decision is invalid.")
	}
	if len(input.Reason) < 10 || len(input.Reason) > 2000 || !validEvidenceReference(input.EvidenceReference) ||
		!validSHA256ReferenceValue(input.EvidenceSHA256) {
		return Candidate{}, problem.New(400, "release_approval_invalid", "Release approval requires a bounded reason, HTTPS evidence reference and non-zero SHA-256.")
	}
	approval := persistence.Stage6ReleaseApproval{
		ID: uuid.New(), CandidateRecordID: candidateRecordID, OperatorTenantID: s.operatorTenantID,
		Role: input.Role, Decision: input.Decision, ApproverUserID: actorID,
		Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256,
		CreatedAt: s.now(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate, err := s.lockCandidate(ctx, tx, candidateRecordID)
		if err != nil {
			return err
		}
		if candidate.State != "ready_for_review" || candidate.CreatedBy == actorID {
			return problem.New(409, "release_candidate_not_reviewable", "Release candidate is not reviewable by this operator.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, "release."+input.Role); err != nil {
			return err
		}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "release_approval_conflict", "This role or operator already recorded a decision for the candidate.")
		} else if err != nil {
			return problem.Wrap(500, "release_approval_create_failed", "Release approval could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "release.approval_recorded", ResourceType: "stage6_release_candidate", ResourceID: &candidate.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"candidateId": candidate.CandidateID, "role": approval.Role, "decision": approval.Decision,
				"evidenceReference": approval.EvidenceReference, "evidenceSha256": input.EvidenceSHA256,
			},
		}); err != nil {
			return err
		}
		if approval.Decision != "rejected" {
			return nil
		}
		now := s.now()
		result := tx.Model(&persistence.Stage6ReleaseCandidate{}).
			Where("id = ? AND version = ? AND state = ?", candidate.ID, candidate.Version, "ready_for_review").
			Updates(map[string]any{"state": "rejected", "version": candidate.Version + 1, "rejected_at": now, "updated_at": now})
		if result.Error != nil {
			return problem.Wrap(500, "release_candidate_reject_failed", "Release candidate could not be rejected.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "release_candidate_version_conflict", "Release candidate changed before rejection.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "release.candidate_rejected", ResourceType: "stage6_release_candidate", ResourceID: &candidate.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"candidateId": candidate.CandidateID, "approvalRole": approval.Role},
		})
	})
	if err != nil {
		return Candidate{}, err
	}
	return s.get(ctx, candidateRecordID)
}

func (s *Service) RecordFinalReview(
	ctx context.Context,
	actorID, candidateRecordID uuid.UUID,
	input FinalReviewInput,
	requestID, ipAddress string,
) (Candidate, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate, err := s.lockCandidate(ctx, tx, candidateRecordID)
		if err != nil {
			return err
		}
		if candidate.State != "observing" {
			return problem.New(409, "release_final_review_not_recordable", "Final review can be bound only while the exact candidate is observing.")
		}
		review, err := bindFinalReviewReceipt(input, candidate, actorID, s.now())
		if err != nil {
			return err
		}
		if err := tx.Create(&review).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "release_final_review_exists", "This candidate already has an immutable final review.")
		} else if err != nil {
			return problem.Wrap(500, "release_final_review_create_failed", "Final review could not be recorded.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "release.final_review_bound", ResourceType: "stage6_release_candidate", ResourceID: &candidate.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"candidateId": candidate.CandidateID, "receiptSha256": formatSHA256(review.ReceiptSHA256),
				"schemaVersion": review.SchemaVersion, "assessment": review.Assessment,
				"controlCount": review.ControlCount, "finalApprovalCount": review.FinalApprovalCount,
				"eligibleForExternalGAAuthorityReview": review.EligibleForExternalGAAuthorityReview,
			},
		})
	})
	if err != nil {
		return Candidate{}, err
	}
	return s.get(ctx, candidateRecordID)
}

func (s *Service) Transition(
	ctx context.Context,
	actorID, candidateRecordID uuid.UUID,
	input TransitionInput,
	requestID, ipAddress string,
) (Candidate, error) {
	input.TargetState = strings.TrimSpace(strings.ToLower(input.TargetState))
	input.Reason = strings.TrimSpace(input.Reason)
	if input.ExpectedVersion <= 0 || len(input.Reason) < 10 || len(input.Reason) > 1000 {
		return Candidate{}, problem.New(400, "release_transition_invalid", "Release transition requires a version and bounded reason.")
	}
	if !slices.Contains([]string{"ready_for_review", "approved", "deploying", "observing", "released", "rolled_back"}, input.TargetState) {
		return Candidate{}, problem.New(400, "release_transition_invalid", "Release transition target is invalid.")
	}
	decisionSummary, disposition, risks, err := s.normalizeDecision(input)
	if err != nil {
		return Candidate{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate, err := s.lockCandidate(ctx, tx, candidateRecordID)
		if err != nil {
			return err
		}
		if candidate.Version != input.ExpectedVersion || !validTransition(candidate.State, input.TargetState) {
			return problem.New(409, "release_candidate_version_conflict", "Release candidate state or version changed.")
		}
		if input.TargetState == "ready_for_review" && !candidate.EvidenceReceiptBound {
			return problem.New(409, "release_candidate_evidence_unbound", "Release candidate review requires an exact eligible evidence receipt.")
		}
		if input.TargetState != "rolled_back" {
			valid, err := s.evaluateProviderCommercialAuthorizationBindings(ctx, tx, candidate, s.now())
			if err != nil {
				return err
			}
			if !valid {
				return problem.New(409, "release_provider_authorization_gate_incomplete", "Release transition requires active exact Provider commercial authorization bindings.")
			}
		}
		if input.TargetState == "approved" {
			if err := s.requireApprovalGates(ctx, tx, candidate); err != nil {
				return err
			}
		}
		if input.TargetState == "released" {
			var finalReview persistence.Stage6ReleaseFinalReview
			if err := tx.Where("candidate_record_id = ? AND operator_tenant_id = ?", candidate.ID, s.operatorTenantID).
				Take(&finalReview).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.New(409, "release_final_review_unbound", "Released state requires an eligible exact-candidate Final Review receipt.")
			} else if err != nil {
				return problem.Wrap(500, "release_final_review_load_failed", "Final review could not be loaded.", err)
			}
			if !finalReview.EligibleForExternalGAAuthorityReview ||
				!finalReviewDecisionMatches(finalReview, *decisionSummary, *disposition, risks) {
				return problem.New(409, "release_final_review_decision_mismatch", "Released decision and residual risks must match the immutable Final Review receipt.")
			}
		}
		now := s.now()
		updates := map[string]any{"state": input.TargetState, "version": candidate.Version + 1, "updated_at": now}
		switch input.TargetState {
		case "approved":
			updates["approved_at"] = now
		case "released":
			encodedRisks, marshalErr := json.Marshal(risksToPersistence(risks))
			if marshalErr != nil {
				return problem.Wrap(500, "release_residual_risks_encode_failed", "Release residual risks could not be encoded.", marshalErr)
			}
			updates["released_at"] = now
			updates["decision_summary"] = *decisionSummary
			updates["residual_risk_disposition"] = *disposition
			updates["residual_risks"] = string(encodedRisks)
		case "rolled_back":
			updates["rolled_back_at"] = now
		}
		result := tx.Model(&persistence.Stage6ReleaseCandidate{}).
			Where("id = ? AND version = ? AND state = ?", candidate.ID, candidate.Version, candidate.State).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "release_candidate_transition_failed", "Release candidate could not be transitioned.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "release_candidate_version_conflict", "Release candidate changed before transition.")
		}
		metadata := map[string]any{"candidateId": candidate.CandidateID, "from": candidate.State, "to": input.TargetState, "reason": input.Reason}
		if input.TargetState == "released" {
			metadata["decisionSummary"] = *decisionSummary
			metadata["residualRiskDisposition"] = *disposition
			metadata["residualRiskIds"] = riskIDs(risks)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "release.candidate_transitioned", ResourceType: "stage6_release_candidate", ResourceID: &candidate.ID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: metadata,
		})
	})
	if err != nil {
		return Candidate{}, err
	}
	return s.get(ctx, candidateRecordID)
}

func (s *Service) normalizeCreate(actorID uuid.UUID, input CreateInput) (persistence.Stage6ReleaseCandidate, error) {
	input.CandidateID = strings.TrimSpace(input.CandidateID)
	input.SourceCommit = strings.TrimSpace(strings.ToLower(input.SourceCommit))
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	if actorID == uuid.Nil || s.operatorTenantID == uuid.Nil || !candidateIDPattern.MatchString(input.CandidateID) ||
		!hex40Pattern.MatchString(input.SourceCommit) || len(input.EnvironmentID) < 3 || len(input.EnvironmentID) > 300 ||
		strings.ContainsAny(input.EnvironmentID, "\r\n\x00") {
		return persistence.Stage6ReleaseCandidate{}, problem.New(400, "release_candidate_invalid", "Release candidate identity is invalid.")
	}
	impactDomains, privacyLegalRequired, err := normalizeImpactDomains(input.ImpactDomains)
	if err != nil {
		return persistence.Stage6ReleaseCandidate{}, err
	}
	lockfile, err := parseSHA256(input.LockfileSHA256)
	if err != nil {
		return persistence.Stage6ReleaseCandidate{}, err
	}
	assets, err := parseSHA256(input.FinalAssetSetSHA256)
	if err != nil {
		return persistence.Stage6ReleaseCandidate{}, err
	}
	now := s.now()
	evidence, err := bindCandidateEvidenceReceipt(input, lockfile, now)
	if err != nil {
		return persistence.Stage6ReleaseCandidate{}, err
	}
	return persistence.Stage6ReleaseCandidate{
		ID: uuid.New(), OperatorTenantID: s.operatorTenantID, CandidateID: input.CandidateID,
		SourceCommit: input.SourceCommit, LockfileSHA256: lockfile, EvidenceBundleSHA256: evidence.SHA256,
		EvidenceBundleReceipt: evidence.Receipt, EvidenceBundleSchema: evidence.Schema,
		EvidenceBundleAssessment: evidence.Assessment, EvidenceBundleValidatedAt: evidence.ValidatedAt,
		EvidenceBundleReceiptSizeBytes: evidence.ReceiptSizeBytes,
		DesktopArtifactSetSHA256:       evidence.DesktopArtifactSetSHA256, EvidenceReceiptBound: true,
		FinalAssetSetSHA256: assets, EnvironmentID: input.EnvironmentID,
		ImpactDomains: impactDomains, PrivacyLegalRequired: privacyLegalRequired,
		State: "draft", Version: 1, CreatedBy: actorID, ResidualRisks: []map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func normalizeProviderCommercialAuthorizationIDs(values []uuid.UUID, required bool) ([]uuid.UUID, error) {
	if required && (len(values) == 0 || len(values) > 16) {
		return nil, problem.New(400, "release_provider_authorizations_invalid", "Provider commercial impact requires one to sixteen exact authorization IDs.")
	}
	if !required && len(values) != 0 {
		return nil, problem.New(400, "release_provider_authorizations_invalid", "Provider authorization IDs require the provider_commercial impact domain.")
	}
	result := slices.Clone(values)
	seen := make(map[uuid.UUID]struct{}, len(result))
	for _, id := range result {
		if id == uuid.Nil {
			return nil, problem.New(400, "release_provider_authorizations_invalid", "Provider authorization IDs must be non-zero and unique.")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, problem.New(400, "release_provider_authorizations_invalid", "Provider authorization IDs must be non-zero and unique.")
		}
		seen[id] = struct{}{}
	}
	slices.SortFunc(result, func(left, right uuid.UUID) int { return strings.Compare(left.String(), right.String()) })
	return result, nil
}

func (s *Service) bindProviderCommercialAuthorizations(
	ctx context.Context,
	tx *gorm.DB,
	candidate persistence.Stage6ReleaseCandidate,
	authorizationIDs []uuid.UUID,
) error {
	if len(authorizationIDs) == 0 {
		return nil
	}
	var authorizations []persistence.ProviderCommercialAuthorization
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id IN ? AND operator_tenant_id = ?", authorizationIDs, s.operatorTenantID).
		Order("provider, authorization_key, id").Find(&authorizations).Error; err != nil {
		return problem.Wrap(500, "release_provider_authorizations_load_failed", "Provider commercial authorizations could not be loaded.", err)
	}
	if len(authorizations) != len(authorizationIDs) {
		return problem.New(409, "release_provider_authorization_gate_incomplete", "Every bound Provider commercial authorization must exist in the Platform Operator Tenant.")
	}
	for _, authorization := range authorizations {
		valid, err := providerCommercialAuthorizationEligible(ctx, tx, authorization, candidate.CreatedAt)
		if err != nil {
			return err
		}
		if !valid {
			return problem.New(409, "release_provider_authorization_gate_incomplete", "Every bound Provider commercial authorization must be active, unexpired and byte-bound with four approved decisions.")
		}
		binding := persistence.Stage6ReleaseProviderAuthorizationBinding{
			CandidateRecordID: candidate.ID, AuthorizationID: authorization.ID,
			OperatorTenantID: s.operatorTenantID, Provider: authorization.Provider,
			AuthorizationKey: authorization.AuthorizationKey, AuthorizationVersion: authorization.Version,
			BoundAt: candidate.CreatedAt,
		}
		if err := tx.WithContext(ctx).Create(&binding).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "release_provider_authorization_binding_conflict", "Provider commercial authorization is already bound to this candidate.")
		} else if err != nil {
			return problem.Wrap(500, "release_provider_authorization_binding_failed", "Provider commercial authorization could not be bound to the candidate.", err)
		}
	}
	return nil
}

func (s *Service) evaluateProviderCommercialAuthorizationBindings(
	ctx context.Context,
	db *gorm.DB,
	candidate persistence.Stage6ReleaseCandidate,
	at time.Time,
) (bool, error) {
	var bindings []persistence.Stage6ReleaseProviderAuthorizationBinding
	if err := db.WithContext(ctx).Where(
		"candidate_record_id = ? AND operator_tenant_id = ?", candidate.ID, candidate.OperatorTenantID,
	).Order("provider, authorization_key, authorization_id").Find(&bindings).Error; err != nil {
		return false, problem.Wrap(500, "release_provider_authorization_bindings_load_failed", "Release Provider authorization bindings could not be verified.", err)
	}
	required := slices.Contains(candidate.ImpactDomains, "provider_commercial")
	if !required {
		return len(bindings) == 0, nil
	}
	if len(bindings) == 0 {
		return false, nil
	}
	for _, binding := range bindings {
		var authorization persistence.ProviderCommercialAuthorization
		if err := db.WithContext(ctx).Where(
			"id = ? AND operator_tenant_id = ?", binding.AuthorizationID, binding.OperatorTenantID,
		).Take(&authorization).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		} else if err != nil {
			return false, problem.Wrap(500, "release_provider_authorizations_load_failed", "Provider commercial authorization could not be verified.", err)
		}
		if authorization.Version != binding.AuthorizationVersion || authorization.Provider != binding.Provider ||
			authorization.AuthorizationKey != binding.AuthorizationKey {
			return false, nil
		}
		valid, err := providerCommercialAuthorizationEligible(ctx, db, authorization, at)
		if err != nil {
			return false, err
		}
		if !valid {
			return false, nil
		}
	}
	return true, nil
}

func providerCommercialAuthorizationEligible(
	ctx context.Context,
	db *gorm.DB,
	authorization persistence.ProviderCommercialAuthorization,
	at time.Time,
) (bool, error) {
	if authorization.State != "active" || !authorization.ReviewExpiresAt.After(at) ||
		!validProviderCommercialDigest(authorization.TermsSHA256) ||
		!validProviderCommercialDigest(authorization.AgreementSHA256) ||
		!validProviderCommercialDigest(authorization.DPASHA256) ||
		!validProviderCommercialDigest(authorization.TerminationRunbookSHA256) {
		return false, nil
	}
	var approvals []persistence.ProviderCommercialAuthorizationApproval
	if err := db.WithContext(ctx).Where(
		"authorization_id = ? AND decision = ? AND approval_role IN ?",
		authorization.ID, "approved", []string{"legal", "privacy", "security", "product"},
	).Find(&approvals).Error; err != nil {
		return false, problem.Wrap(500, "release_provider_authorization_approvals_load_failed", "Provider commercial approval evidence could not be verified.", err)
	}
	if len(approvals) != 4 {
		return false, nil
	}
	roles := make(map[string]struct{}, 4)
	for _, approval := range approvals {
		if !validProviderCommercialDigest(approval.EvidenceSHA256) {
			return false, nil
		}
		roles[approval.ApprovalRole] = struct{}{}
	}
	return len(roles) == 4, nil
}

func validProviderCommercialDigest(value *string) bool {
	return value != nil && validSHA256ReferenceValue(*value)
}

func validSHA256ReferenceValue(value string) bool {
	return sha256ReferencePattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}

func (s *Service) normalizeDecision(input TransitionInput) (*string, *string, []ResidualRisk, error) {
	if input.TargetState != "released" {
		if input.DecisionSummary != nil || input.ResidualRiskDisposition != nil || len(input.ResidualRisks) != 0 {
			return nil, nil, nil, problem.New(400, "release_decision_premature", "Release decision fields are accepted only for released state.")
		}
		return nil, nil, nil, nil
	}
	if input.DecisionSummary == nil || input.ResidualRiskDisposition == nil {
		return nil, nil, nil, problem.New(400, "release_decision_required", "Released state requires a decision summary and residual-risk disposition.")
	}
	summary := strings.TrimSpace(*input.DecisionSummary)
	disposition := strings.TrimSpace(strings.ToLower(*input.ResidualRiskDisposition))
	if len(summary) < 20 || len(summary) > 4000 || !slices.Contains([]string{"none", "accepted"}, disposition) || len(input.ResidualRisks) > 50 {
		return nil, nil, nil, problem.New(400, "release_decision_invalid", "Release decision or residual-risk disposition is invalid.")
	}
	if (disposition == "none" && len(input.ResidualRisks) != 0) || (disposition == "accepted" && len(input.ResidualRisks) == 0) {
		return nil, nil, nil, problem.New(400, "release_risk_disposition_mismatch", "Residual-risk disposition does not match the risk inventory.")
	}
	seen := make(map[string]struct{}, len(input.ResidualRisks))
	now := s.now()
	for index := range input.ResidualRisks {
		risk := &input.ResidualRisks[index]
		risk.ID = strings.TrimSpace(strings.ToLower(risk.ID))
		risk.Summary = strings.TrimSpace(risk.Summary)
		risk.Owner = strings.TrimSpace(risk.Owner)
		risk.AcceptanceReason = strings.TrimSpace(risk.AcceptanceReason)
		risk.EvidenceReference = strings.TrimSpace(risk.EvidenceReference)
		_, duplicate := seen[risk.ID]
		if duplicate || !riskIDPattern.MatchString(risk.ID) || len(risk.Summary) < 10 || len(risk.Summary) > 500 ||
			len(risk.Owner) < 3 || len(risk.Owner) > 200 || len(risk.AcceptanceReason) < 10 || len(risk.AcceptanceReason) > 1000 ||
			!risk.DueAt.After(now) || !validEvidenceReference(risk.EvidenceReference) {
			return nil, nil, nil, problem.New(400, "release_residual_risk_invalid", "Every accepted residual risk requires a unique ID, owner, future due date, reason and HTTPS evidence reference.")
		}
		seen[risk.ID] = struct{}{}
		risk.DueAt = risk.DueAt.UTC()
	}
	return &summary, &disposition, input.ResidualRisks, nil
}

func (s *Service) lockCandidate(ctx context.Context, tx *gorm.DB, id uuid.UUID) (persistence.Stage6ReleaseCandidate, error) {
	var candidate persistence.Stage6ReleaseCandidate
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&candidate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return candidate, problem.New(404, "release_candidate_not_found", "Release candidate not found.")
	}
	if err != nil {
		return candidate, problem.Wrap(500, "release_candidate_load_failed", "Release candidate could not be loaded.", err)
	}
	return candidate, nil
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Candidate, error) {
	var model persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Candidate{}, problem.New(404, "release_candidate_not_found", "Release candidate not found.")
	} else if err != nil {
		return Candidate{}, problem.Wrap(500, "release_candidate_load_failed", "Release candidate could not be loaded.", err)
	}
	views, err := s.views(ctx, []persistence.Stage6ReleaseCandidate{model})
	if err != nil {
		return Candidate{}, err
	}
	return views[0], nil
}

func (s *Service) views(ctx context.Context, models []persistence.Stage6ReleaseCandidate) ([]Candidate, error) {
	ids := make([]uuid.UUID, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	type approvalRow struct {
		persistence.Stage6ReleaseApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	rows := make([]approvalRow, 0)
	finalReviewRows := make([]persistence.Stage6ReleaseFinalReview, 0)
	type providerBindingRow struct {
		persistence.Stage6ReleaseProviderAuthorizationBinding
		CurrentVersion  int64     `gorm:"column:current_version"`
		CurrentState    string    `gorm:"column:current_state"`
		ReviewExpiresAt time.Time `gorm:"column:review_expires_at"`
	}
	providerBindingRows := make([]providerBindingRow, 0)
	if len(ids) > 0 {
		if err := s.db.WithContext(ctx).Table("stage6_release_approvals AS approval").
			Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").
			Joins("JOIN users AS approver ON approver.id = approval.approver_user_id").
			Where("approval.candidate_record_id IN ?", ids).
			Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
			return nil, problem.Wrap(500, "release_approvals_load_failed", "Release approvals could not be loaded.", err)
		}
		if err := s.db.WithContext(ctx).Where("candidate_record_id IN ?", ids).
			Order("created_at, id").Find(&finalReviewRows).Error; err != nil {
			return nil, problem.Wrap(500, "release_final_reviews_load_failed", "Final reviews could not be loaded.", err)
		}
		if err := s.db.WithContext(ctx).Table("stage6_release_provider_authorization_bindings AS binding").
			Select("binding.*, auth_record.version AS current_version, auth_record.state AS current_state, auth_record.review_expires_at").
			Joins("JOIN provider_commercial_authorizations AS auth_record ON auth_record.id = binding.authorization_id").
			Where("binding.candidate_record_id IN ?", ids).
			Order("binding.provider, binding.authorization_key, binding.authorization_id").Scan(&providerBindingRows).Error; err != nil {
			return nil, problem.Wrap(500, "release_provider_authorization_bindings_load_failed", "Release Provider authorization bindings could not be loaded.", err)
		}
	}
	approvals := make(map[uuid.UUID][]Approval)
	for _, row := range rows {
		approvals[row.CandidateRecordID] = append(approvals[row.CandidateRecordID], Approval{
			ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID,
			ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName, Reason: row.Reason,
			EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256, CreatedAt: row.CreatedAt,
		})
	}
	finalReviews := make(map[uuid.UUID]*FinalReview, len(finalReviewRows))
	for _, row := range finalReviewRows {
		reviewRisks, err := risksFromPersistence(row.ResidualRisks)
		if err != nil {
			return nil, problem.Wrap(500, "release_final_review_risks_invalid", "Stored Final Review risks are invalid.", err)
		}
		finalReviews[row.CandidateRecordID] = &FinalReview{
			ID: row.ID, ReceiptSHA256: formatSHA256(row.ReceiptSHA256), SchemaVersion: row.SchemaVersion,
			Assessment: row.Assessment, ValidatedAt: row.ValidatedAt, ReceiptSizeBytes: row.ReceiptSizeBytes,
			ControlInventorySHA256: row.ControlInventorySHA256, ControlCount: row.ControlCount,
			FinalApprovalCount: row.FinalApprovalCount, DecisionSummary: row.DecisionSummary,
			ResidualRiskDisposition: row.ResidualRiskDisposition, ResidualRisks: reviewRisks,
			AllRequiredControlsPassed:            row.AllRequiredControlsPassed,
			AllRequiredFinalApprovalsApproved:    row.AllRequiredFinalApprovalsApproved,
			EligibleForExternalGAAuthorityReview: row.EligibleForExternalGAAuthorityReview,
			BoundBy:                              row.BoundBy, CreatedAt: row.CreatedAt,
		}
	}
	providerBindings := make(map[uuid.UUID][]ProviderCommercialAuthorizationBinding)
	now := s.now()
	for _, row := range providerBindingRows {
		providerBindings[row.CandidateRecordID] = append(providerBindings[row.CandidateRecordID], ProviderCommercialAuthorizationBinding{
			AuthorizationID: row.AuthorizationID, Provider: row.Provider, AuthorizationKey: row.AuthorizationKey,
			AuthorizationVersion: row.AuthorizationVersion, CurrentVersion: row.CurrentVersion,
			CurrentState: row.CurrentState, ReviewExpiresAt: row.ReviewExpiresAt,
			BindingValid: row.CurrentState == "active" && row.CurrentVersion == row.AuthorizationVersion && row.ReviewExpiresAt.After(now),
			BoundAt:      row.BoundAt,
		})
	}
	result := make([]Candidate, 0, len(models))
	for _, model := range models {
		risks, err := risksFromPersistence(model.ResidualRisks)
		if err != nil {
			return nil, problem.Wrap(500, "release_candidate_risks_invalid", "Stored release risks are invalid.", err)
		}
		items := approvals[model.ID]
		if items == nil {
			items = []Approval{}
		}
		commercialBindings := providerBindings[model.ID]
		if commercialBindings == nil {
			commercialBindings = []ProviderCommercialAuthorizationBinding{}
		}
		result = append(result, Candidate{
			ID: model.ID, CandidateID: model.CandidateID, SourceCommit: model.SourceCommit,
			LockfileSHA256: formatSHA256(model.LockfileSHA256), EvidenceBundleSHA256: formatSHA256(model.EvidenceBundleSHA256),
			EvidenceBundleSchema: model.EvidenceBundleSchema, EvidenceBundleAssessment: model.EvidenceBundleAssessment,
			EvidenceBundleValidatedAt:      model.EvidenceBundleValidatedAt,
			EvidenceBundleReceiptSizeBytes: model.EvidenceBundleReceiptSizeBytes,
			DesktopArtifactSetSHA256:       model.DesktopArtifactSetSHA256, EvidenceReceiptBound: model.EvidenceReceiptBound,
			FinalAssetSetSHA256: formatSHA256(model.FinalAssetSetSHA256), EnvironmentID: model.EnvironmentID,
			ImpactDomains: slices.Clone(model.ImpactDomains), ProviderCommercialAuthorizations: commercialBindings,
			PrivacyLegalRequired:  model.PrivacyLegalRequired,
			RequiredApprovalRoles: requiredApprovalRolesFor(model),
			State:                 model.State, Version: model.Version, CreatedBy: model.CreatedBy,
			DecisionSummary: model.DecisionSummary, ResidualRiskDisposition: model.ResidualRiskDisposition,
			ResidualRisks: risks, Approvals: items, FinalReview: finalReviews[model.ID], ApprovedAt: model.ApprovedAt, ReleasedAt: model.ReleasedAt,
			RejectedAt: model.RejectedAt, RolledBackAt: model.RolledBackAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
		})
	}
	return result, nil
}

func finalReviewDecisionMatches(
	review persistence.Stage6ReleaseFinalReview,
	decisionSummary, disposition string,
	risks []ResidualRisk,
) bool {
	if strings.TrimSpace(decisionSummary) != review.DecisionSummary || disposition != review.ResidualRiskDisposition {
		return false
	}
	expected, err := json.Marshal(review.ResidualRisks)
	if err != nil {
		return false
	}
	actual, err := json.Marshal(risksToPersistence(risks))
	return err == nil && bytes.Equal(expected, actual)
}

func normalizeImpactDomains(values []string) ([]string, bool, error) {
	if len(values) < 1 || len(values) > len(allowedImpactDomains) {
		return nil, false, problem.New(400, "release_impact_domains_invalid", "Release candidate requires one or more valid impact domains.")
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	privacyLegalRequired := false
	for _, value := range values {
		domain := strings.TrimSpace(strings.ToLower(value))
		if _, duplicate := seen[domain]; duplicate || !slices.Contains(allowedImpactDomains, domain) {
			return nil, false, problem.New(400, "release_impact_domains_invalid", "Release impact domains must be valid and unique.")
		}
		seen[domain] = struct{}{}
		normalized = append(normalized, domain)
		privacyLegalRequired = privacyLegalRequired || slices.Contains(privacyLegalImpactDomains, domain)
	}
	slices.Sort(normalized)
	return normalized, privacyLegalRequired, nil
}

func requiredApprovalRolesFor(candidate persistence.Stage6ReleaseCandidate) []string {
	roles := slices.Clone(requiredApprovalRoles)
	if candidate.PrivacyLegalRequired {
		roles = append(roles, "privacy_legal")
	}
	return roles
}

func parseSHA256(value string) ([]byte, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return nil, problem.New(400, "release_digest_invalid", "Release digests must be SHA-256 values.")
	}
	digest, err := hex.DecodeString(value)
	if err != nil {
		return nil, problem.New(400, "release_digest_invalid", "Release digests must be SHA-256 values.")
	}
	return digest, nil
}

func formatSHA256(value []byte) string { return "sha256:" + hex.EncodeToString(value) }

func validEvidenceReference(value string) bool {
	if len(value) < 8 || len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func validTransition(from, to string) bool {
	return slices.Contains(stateTransitions[from], to)
}

func risksToPersistence(risks []ResidualRisk) []map[string]any {
	result := make([]map[string]any, 0, len(risks))
	for _, risk := range risks {
		result = append(result, map[string]any{
			"id": risk.ID, "summary": risk.Summary, "owner": risk.Owner, "dueAt": risk.DueAt.UTC().Format(time.RFC3339),
			"acceptanceReason": risk.AcceptanceReason, "evidenceReference": risk.EvidenceReference,
		})
	}
	return result
}

func risksFromPersistence(values []map[string]any) ([]ResidualRisk, error) {
	result := make([]ResidualRisk, 0, len(values))
	for _, value := range values {
		dueAt, err := time.Parse(time.RFC3339, stringValue(value["dueAt"]))
		if err != nil {
			return nil, err
		}
		result = append(result, ResidualRisk{
			ID: stringValue(value["id"]), Summary: stringValue(value["summary"]), Owner: stringValue(value["owner"]), DueAt: dueAt,
			AcceptanceReason: stringValue(value["acceptanceReason"]), EvidenceReference: stringValue(value["evidenceReference"]),
		})
	}
	return result, nil
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func riskIDs(risks []ResidualRisk) []string {
	result := make([]string, 0, len(risks))
	for _, risk := range risks {
		result = append(result, risk.ID)
	}
	return result
}

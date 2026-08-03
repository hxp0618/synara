package penetrationgovernance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	receiptSchema     = "synara.third-party-penetration-evidence-receipt.v1"
	receiptAssessment = "evidence-validated-not-penetration-passed"
	receiptMaxBytes   = 512 * 1024
)

var approvalRoles = []string{"engineering", "product", "security"}
var approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var assetTypes = []string{"control-plane-api", "provider-host", "web", "worker-runtime"}
var scopeAreas = []string{"commandInjection", "containerEscape", "crossTenantAuthorization", "pathTraversal", "ssrf", "supplyChain"}
var requiredMethodologies = []string{"cloud-runtime", "manual-business-logic", "owasp-web-api"}
var allowedMethodologies = append(slices.Clone(requiredMethodologies), "owasp-asvs", "owasp-wstg", "ptes")
var evidenceFields = []string{
	"stage5CompletionEvidence", "targetRevalidationEvidence", "scopeStatementEvidence",
	"assessorIndependenceEvidence", "executionEvidence", "finalReportEvidence",
	"findingRegisterEvidence", "remediationRetestEvidence", "riskAcceptanceEvidence",
}

type Asset struct {
	AssetType      string `json:"assetType"`
	ArtifactSHA256 string `json:"artifactSha256"`
	Tested         bool   `json:"tested"`
}

type Approval struct {
	ID                uuid.UUID  `json:"id"`
	Role              string     `json:"role"`
	Decision          string     `json:"decision"`
	ApproverUserID    uuid.UUID  `json:"approverUserId"`
	ApproverEmail     string     `json:"approverEmail"`
	ApproverName      string     `json:"approverName"`
	Reason            string     `json:"reason"`
	EvidenceReference string     `json:"evidenceReference"`
	EvidenceSHA256    *string    `json:"evidenceSha256"`
	SupersededAt      *time.Time `json:"supersededAt"`
	SupersededReason  *string    `json:"supersededReason"`
	CreatedAt         time.Time  `json:"createdAt"`
}

type Engagement struct {
	ID                                    uuid.UUID  `json:"id"`
	CandidateRecordID                     uuid.UUID  `json:"candidateRecordId"`
	CandidateID                           string     `json:"candidateId"`
	EngagementID                          uuid.UUID  `json:"engagementId"`
	ReceiptSHA256                         string     `json:"receiptSha256"`
	ReceiptSizeBytes                      int64      `json:"receiptSizeBytes"`
	ReleaseCommit                         string     `json:"releaseCommit"`
	EnvironmentClass                      string     `json:"environmentClass"`
	EnvironmentID                         string     `json:"environmentId"`
	DeploymentProfile                     string     `json:"deploymentProfile"`
	StartedAt                             time.Time  `json:"startedAt"`
	CompletedAt                           time.Time  `json:"completedAt"`
	ReportIssuedAt                        time.Time  `json:"reportIssuedAt"`
	ValidatedAt                           time.Time  `json:"validatedAt"`
	ThirdPartyIndependenceDeclared        bool       `json:"thirdPartyIndependenceDeclared"`
	Stage5DependencySatisfied             bool       `json:"stage5DependencySatisfied"`
	AssetCoverageComplete                 bool       `json:"assetCoverageComplete"`
	ScopeCoverageComplete                 bool       `json:"scopeCoverageComplete"`
	MethodologyCoverageComplete           bool       `json:"methodologyCoverageComplete"`
	NoUnacceptedHighOrCriticalFindings    bool       `json:"noUnacceptedHighOrCriticalFindings"`
	EligibleForHumanGateReview            bool       `json:"eligibleForHumanGateReview"`
	CryptographicSignaturesVerified       bool       `json:"cryptographicSignaturesVerified"`
	ExternalAuthorityVerificationRequired bool       `json:"externalAuthorityVerificationRequired"`
	State                                 string     `json:"state"`
	Version                               int64      `json:"version"`
	CreatedBy                             uuid.UUID  `json:"createdBy"`
	Assets                                []Asset    `json:"assets"`
	Approvals                             []Approval `json:"approvals"`
	ApprovedAt                            *time.Time `json:"approvedAt"`
	RejectedAt                            *time.Time `json:"rejectedAt"`
	CreatedAt                             time.Time  `json:"createdAt"`
	UpdatedAt                             time.Time  `json:"updatedAt"`
}

type ImportInput struct {
	CandidateRecordID uuid.UUID `json:"candidateRecordId"`
	ReceiptBase64     string    `json:"receiptBase64"`
	ReceiptSHA256     string    `json:"receiptSha256"`
}

type ApprovalInput struct {
	Role              string `json:"role"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	EvidenceReference string `json:"evidenceReference"`
	EvidenceSHA256    string `json:"evidenceSha256"`
}

type Service struct {
	db               *gorm.DB
	operatorTenantID uuid.UUID
	authority        *governanceauthority.Service
	now              func() time.Time
}

func NewService(db *gorm.DB, operatorTenantID uuid.UUID) *Service {
	return &Service{db: db, operatorTenantID: operatorTenantID, authority: governanceauthority.NewService(db, operatorTenantID), now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) List(ctx context.Context) ([]Engagement, error) {
	var models []persistence.Stage6PenetrationEngagement
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "penetration_engagements_load_failed", "Penetration engagements could not be loaded.", err)
	}
	items := make([]Engagement, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) Import(ctx context.Context, actorID uuid.UUID, input ImportInput, requestID, ipAddress string) (Engagement, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", input.CandidateRecordID, s.operatorTenantID).Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Engagement{}, problem.New(404, "penetration_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Engagement{}, problem.Wrap(500, "penetration_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Engagement{}, problem.New(403, "penetration_engagement_import_forbidden", "The release candidate creator must import its exact Penetration receipt.")
	}
	binding, err := bindReceipt(input, candidate, s.now())
	if err != nil {
		return Engagement{}, err
	}
	model := binding.model
	model.ID = uuid.New()
	model.OperatorTenantID = s.operatorTenantID
	model.CandidateRecordID = candidate.ID
	model.CreatedBy = actorID
	model.CreatedAt = s.now()
	model.UpdatedAt = model.CreatedAt
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "penetration_engagement_exists", "This exact Penetration engagement already exists.")
		} else if err != nil {
			return problem.Wrap(500, "penetration_engagement_import_failed", "The Penetration engagement could not be imported.", err)
		}
		for _, asset := range binding.assets {
			asset.ID = uuid.New()
			asset.PenetrationEngagementID = model.ID
			asset.OperatorTenantID = s.operatorTenantID
			asset.CreatedAt = model.CreatedAt
			if err := tx.Create(&asset).Error; err != nil {
				return problem.Wrap(500, "penetration_assets_import_failed", "Penetration asset projections could not be imported.", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "penetration.engagement_imported", ResourceType: "stage6_penetration_engagement", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"candidateId": candidate.CandidateID, "engagementId": model.EngagementID, "eligibleForHumanGateReview": model.EligibleForHumanGateReview, "receiptSha256": input.ReceiptSHA256}})
	})
	if err != nil {
		return Engagement{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, recordID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Engagement, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(approvalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) || len(input.Reason) < 20 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validApprovalEvidenceSHA256(input.EvidenceSHA256) {
		return Engagement{}, problem.New(400, "penetration_approval_invalid", "Penetration approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6PenetrationEngagement
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", recordID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "penetration_engagement_not_found", "The Penetration engagement was not found.")
		} else if err != nil {
			return problem.Wrap(500, "penetration_engagement_load_failed", "The Penetration engagement could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "penetration_engagement_not_reviewable", "The Penetration engagement is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "penetration_engagement_ineligible", "An ineligible Penetration engagement cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.PenetrationPrefix+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6PenetrationApproval{ID: uuid.New(), PenetrationEngagementID: model.ID, OperatorTenantID: s.operatorTenantID, Role: input.Role, Decision: input.Decision, ApproverUserID: actorID, Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now()}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "penetration_approval_conflict", "This role or operator already recorded a Penetration decision.")
		} else if err != nil {
			return problem.Wrap(500, "penetration_approval_create_failed", "The Penetration decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "penetration.approval_recorded", ResourceType: "stage6_penetration_engagement", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"role": input.Role, "decision": input.Decision, "evidenceReference": input.EvidenceReference, "evidenceSha256": input.EvidenceSHA256}}); err != nil {
			return err
		}
		target := ""
		if input.Decision == "rejected" {
			target = "rejected"
		} else {
			var count int64
			if err := tx.Model(&persistence.Stage6PenetrationApproval{}).Where("penetration_engagement_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", model.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Count(&count).Error; err != nil {
				return problem.Wrap(500, "penetration_approvals_load_failed", "Penetration decisions could not be verified.", err)
			}
			if count == int64(len(approvalRoles)) {
				target = "approved"
			}
		}
		if target == "" {
			return nil
		}
		now := s.now()
		updates := map[string]any{"state": target, "version": model.Version + 1, "updated_at": now}
		if target == "approved" {
			updates["approved_at"] = now
		} else {
			updates["rejected_at"] = now
		}
		result := tx.Model(&persistence.Stage6PenetrationEngagement{}).Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "penetration_engagement_transition_failed", "The Penetration engagement decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "penetration_engagement_version_conflict", "The Penetration engagement changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "penetration.engagement_" + target, ResourceType: "stage6_penetration_engagement", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress})
	})
	if err != nil {
		return Engagement{}, err
	}
	return s.get(ctx, recordID)
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Engagement, error) {
	var model persistence.Stage6PenetrationEngagement
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Engagement{}, problem.New(404, "penetration_engagement_not_found", "The Penetration engagement was not found.")
	} else if err != nil {
		return Engagement{}, problem.Wrap(500, "penetration_engagement_load_failed", "The Penetration engagement could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6PenetrationEngagement) (Engagement, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Engagement{}, problem.Wrap(500, "penetration_candidate_load_failed", "The linked release candidate could not be loaded.", err)
	}
	var assetModels []persistence.Stage6PenetrationAsset
	if err := db.WithContext(ctx).Where("penetration_engagement_id = ?", model.ID).Order("asset_type").Find(&assetModels).Error; err != nil {
		return Engagement{}, problem.Wrap(500, "penetration_assets_load_failed", "Penetration assets could not be loaded.", err)
	}
	assets := make([]Asset, 0, len(assetModels))
	for _, item := range assetModels {
		assets = append(assets, Asset{AssetType: item.AssetType, ArtifactSHA256: formatDigest(item.ArtifactSHA256), Tested: item.Tested})
	}
	type approvalRow struct {
		persistence.Stage6PenetrationApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_penetration_approvals approval").Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").Joins("JOIN users approver ON approver.id = approval.approver_user_id").Where("approval.penetration_engagement_id = ?", model.ID).Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
		return Engagement{}, problem.Wrap(500, "penetration_approvals_load_failed", "Penetration decisions could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, Approval{ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID, ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName, Reason: row.Reason, EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256, SupersededAt: row.SupersededAt, SupersededReason: row.SupersededReason, CreatedAt: row.CreatedAt})
	}
	return Engagement{ID: model.ID, CandidateRecordID: model.CandidateRecordID, CandidateID: candidate.CandidateID, EngagementID: model.EngagementID, ReceiptSHA256: formatDigest(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes, ReleaseCommit: model.ReleaseCommit, EnvironmentClass: model.EnvironmentClass, EnvironmentID: model.EnvironmentID, DeploymentProfile: model.DeploymentProfile, StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, ReportIssuedAt: model.ReportIssuedAt, ValidatedAt: model.ValidatedAt, ThirdPartyIndependenceDeclared: model.ThirdPartyIndependenceDeclared, Stage5DependencySatisfied: model.Stage5DependencySatisfied, AssetCoverageComplete: model.AssetCoverageComplete, ScopeCoverageComplete: model.ScopeCoverageComplete, MethodologyCoverageComplete: model.MethodologyCoverageComplete, NoUnacceptedHighOrCriticalFindings: model.NoUnacceptedHighOrCriticalFindings, EligibleForHumanGateReview: model.EligibleForHumanGateReview, CryptographicSignaturesVerified: model.CryptographicSignaturesVerified, ExternalAuthorityVerificationRequired: model.ExternalAuthorityVerificationRequired, State: model.State, Version: model.Version, CreatedBy: model.CreatedBy, Assets: assets, Approvals: approvals, ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt}, nil
}

type receiptAsset struct {
	AssetType      string `json:"assetType"`
	AssetID        string `json:"assetId"`
	ArtifactDigest string `json:"artifactDigest"`
	Tested         bool   `json:"tested"`
}

type penetrationReceipt struct {
	SchemaVersion                              string                                `json:"schemaVersion"`
	EngagementID                               uuid.UUID                             `json:"engagementId"`
	ReleaseCommit                              string                                `json:"releaseCommit"`
	EnvironmentClass                           string                                `json:"environmentClass"`
	EnvironmentID                              string                                `json:"environmentId"`
	DeploymentProfile                          string                                `json:"deploymentProfile"`
	Engagement                                 map[string]time.Time                  `json:"engagement"`
	Manifest                                   map[string]json.RawMessage            `json:"manifest"`
	Assessor                                   map[string]json.RawMessage            `json:"assessor"`
	ThirdPartyIndependenceDeclared             bool                                  `json:"thirdPartyIndependenceDeclared"`
	Stage5Dependency                           map[string]json.RawMessage            `json:"stage5Dependency"`
	Assets                                     []receiptAsset                        `json:"assets"`
	AssetCoverageComplete                      bool                                  `json:"assetCoverageComplete"`
	ScopeCoverage                              map[string]map[string]json.RawMessage `json:"scopeCoverage"`
	ScopeCoverageComplete                      bool                                  `json:"scopeCoverageComplete"`
	Methodologies                              []string                              `json:"methodologies"`
	MethodologyCoverageComplete                bool                                  `json:"methodologyCoverageComplete"`
	Findings                                   []json.RawMessage                     `json:"findings"`
	FindingCounts                              map[string]map[string]int64           `json:"findingCounts"`
	DeclaredNoUnacceptedHighOrCriticalFindings bool                                  `json:"declaredNoUnacceptedHighOrCriticalFindings"`
	Evidence                                   map[string]map[string]json.RawMessage `json:"evidence"`
	ReleaseEligibleEnvironment                 bool                                  `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview                 bool                                  `json:"eligibleForHumanGateReview"`
	ValidatedAt                                time.Time                             `json:"validatedAt"`
	Assessment                                 string                                `json:"assessment"`
}

type receiptBinding struct {
	model  persistence.Stage6PenetrationEngagement
	assets []persistence.Stage6PenetrationAsset
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (receiptBinding, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return receiptBinding{}, invalidReceipt("Penetration receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return receiptBinding{}, invalidReceipt("Penetration receipt must be bounded UTF-8 JSON.")
	}
	digest := sha256.Sum256(data)
	expectedDigest, err := parseDigest(input.ReceiptSHA256)
	if err != nil || !bytes.Equal(expectedDigest, digest[:]) {
		return receiptBinding{}, invalidReceipt("Penetration receipt SHA-256 does not match its exact bytes.")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || !exactKeys(top, []string{"schemaVersion", "engagementId", "releaseCommit", "environmentClass", "environmentId", "deploymentProfile", "engagement", "manifest", "assessor", "thirdPartyIndependenceDeclared", "stage5Dependency", "assets", "assetCoverageComplete", "scopeCoverage", "scopeCoverageComplete", "methodologies", "methodologyCoverageComplete", "findings", "findingCounts", "declaredNoUnacceptedHighOrCriticalFindings", "evidence", "releaseEligibleEnvironment", "eligibleForHumanGateReview", "validatedAt", "assessment"}) {
		return receiptBinding{}, invalidReceipt("Penetration receipt schema is invalid.")
	}
	var receipt penetrationReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return receiptBinding{}, invalidReceipt("Penetration receipt JSON is invalid.")
	}
	started, startedOK := receipt.Engagement["startedAt"]
	completed, completedOK := receipt.Engagement["completedAt"]
	reportIssued, reportOK := receipt.Engagement["reportIssuedAt"]
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment || receipt.EngagementID == uuid.Nil || !exactKeys(receipt.Engagement, []string{"startedAt", "completedAt", "reportIssuedAt"}) || !startedOK || !completedOK || !reportOK || !completed.After(started) || reportIssued.Before(completed) || receipt.ValidatedAt.Before(reportIssued) || receipt.ValidatedAt.After(reportIssued.Add(30*24*time.Hour)) || receipt.ValidatedAt.After(now.Add(5*time.Minute)) {
		return receiptBinding{}, invalidReceipt("Penetration receipt identity or timestamps are invalid.")
	}
	projection, err := candidateProjection(candidate)
	if err != nil || projection.PenetrationReceiptSHA256 != formatDigest(digest[:]) || projection.SourceCommit != receipt.ReleaseCommit || projection.EnvironmentID != receipt.EnvironmentID || projection.EnvironmentClass != receipt.EnvironmentClass {
		return receiptBinding{}, invalidReceipt("Penetration receipt is not bound to the exact Release candidate.")
	}
	if !slices.Contains([]string{"production", "production-like"}, receipt.EnvironmentClass) || !receipt.ReleaseEligibleEnvironment {
		return receiptBinding{}, invalidReceipt("Penetration receipt environment is not release eligible.")
	}
	if !validEvidenceReference(receipt.Manifest) || !validAssessor(receipt.Assessor) || !receipt.ThirdPartyIndependenceDeclared || !validStage5(receipt.Stage5Dependency, receipt.ReleaseCommit, receipt.DeploymentProfile) {
		return receiptBinding{}, invalidReceipt("Penetration assessor or Stage 5 dependency projection is invalid.")
	}
	assets, err := bindAssets(receipt.Assets, projection.Artifacts)
	if err != nil || !receipt.AssetCoverageComplete {
		return receiptBinding{}, invalidReceipt("Penetration asset coverage is incomplete or not candidate-bound.")
	}
	if !validScope(receipt.ScopeCoverage) || !receipt.ScopeCoverageComplete || !validMethodologies(receipt.Methodologies) || !receipt.MethodologyCoverageComplete {
		return receiptBinding{}, invalidReceipt("Penetration scope or methodology coverage is incomplete.")
	}
	noUnaccepted, countsValid := validateFindings(receipt.Findings, receipt.FindingCounts, started, completed)
	if !countsValid || receipt.DeclaredNoUnacceptedHighOrCriticalFindings != noUnaccepted || !noUnaccepted {
		return receiptBinding{}, invalidReceipt("Penetration finding projection contains an unaccepted High/Critical result or inconsistent counts.")
	}
	if !validEvidenceSet(receipt.Evidence) {
		return receiptBinding{}, invalidReceipt("Penetration evidence references are incomplete.")
	}
	eligible := receipt.ReleaseEligibleEnvironment && receipt.ThirdPartyIndependenceDeclared && receipt.AssetCoverageComplete && receipt.ScopeCoverageComplete && receipt.MethodologyCoverageComplete && noUnaccepted
	if receipt.EligibleForHumanGateReview != eligible || !eligible {
		return receiptBinding{}, invalidReceipt("Penetration eligibility projection is inconsistent.")
	}
	return receiptBinding{model: persistence.Stage6PenetrationEngagement{EngagementID: receipt.EngagementID, Receipt: slices.Clone(data), ReceiptSHA256: slices.Clone(digest[:]), ReceiptSizeBytes: int64(len(data)), Schema: receipt.SchemaVersion, Assessment: receipt.Assessment, ReleaseCommit: receipt.ReleaseCommit, EnvironmentClass: receipt.EnvironmentClass, EnvironmentID: receipt.EnvironmentID, DeploymentProfile: receipt.DeploymentProfile, StartedAt: started.UTC(), CompletedAt: completed.UTC(), ReportIssuedAt: reportIssued.UTC(), ValidatedAt: receipt.ValidatedAt.UTC(), ThirdPartyIndependenceDeclared: true, Stage5DependencySatisfied: true, AssetCoverageComplete: true, ScopeCoverageComplete: true, MethodologyCoverageComplete: true, NoUnacceptedHighOrCriticalFindings: true, EligibleForHumanGateReview: true, CryptographicSignaturesVerified: false, ExternalAuthorityVerificationRequired: true, State: "recorded", Version: 1}, assets: assets}, nil
}

type projectedCandidate struct {
	SourceCommit             string
	EnvironmentClass         string
	EnvironmentID            string
	PenetrationReceiptSHA256 string
	Artifacts                map[string]string
}

func candidateProjection(candidate persistence.Stage6ReleaseCandidate) (projectedCandidate, error) {
	var receipt struct {
		Candidate struct {
			SourceCommit     string `json:"sourceCommit"`
			EnvironmentClass string `json:"environmentClass"`
			EnvironmentID    string `json:"environmentId"`
			Artifacts        struct {
				ControlPlaneImage string `json:"controlPlaneImage"`
				ProviderHostImage string `json:"providerHostImage"`
				WebArtifact       string `json:"webArtifact"`
				WorkerImage       string `json:"workerImage"`
			} `json:"artifacts"`
		} `json:"candidate"`
		Receipts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		return projectedCandidate{}, err
	}
	penetration, ok := receipt.Receipts["penetration"]
	if !ok || penetration.SHA256 == "" || receipt.Candidate.SourceCommit != candidate.SourceCommit || receipt.Candidate.EnvironmentID != candidate.EnvironmentID {
		return projectedCandidate{}, errors.New("candidate penetration receipt reference is missing")
	}
	return projectedCandidate{SourceCommit: receipt.Candidate.SourceCommit, EnvironmentClass: receipt.Candidate.EnvironmentClass, EnvironmentID: receipt.Candidate.EnvironmentID, PenetrationReceiptSHA256: penetration.SHA256, Artifacts: map[string]string{"control-plane-api": receipt.Candidate.Artifacts.ControlPlaneImage, "provider-host": receipt.Candidate.Artifacts.ProviderHostImage, "web": receipt.Candidate.Artifacts.WebArtifact, "worker-runtime": receipt.Candidate.Artifacts.WorkerImage}}, nil
}

func bindAssets(values []receiptAsset, expected map[string]string) ([]persistence.Stage6PenetrationAsset, error) {
	if len(values) != len(assetTypes) {
		return nil, errors.New("invalid assets")
	}
	seen := map[string]struct{}{}
	assets := make([]persistence.Stage6PenetrationAsset, 0, len(values))
	for _, value := range values {
		if !slices.Contains(assetTypes, value.AssetType) || value.AssetID == "" || !value.Tested || expected[value.AssetType] != value.ArtifactDigest {
			return nil, errors.New("invalid asset")
		}
		if _, duplicate := seen[value.AssetType]; duplicate {
			return nil, errors.New("duplicate asset")
		}
		seen[value.AssetType] = struct{}{}
		digest, err := parseDigest(value.ArtifactDigest)
		if err != nil {
			return nil, err
		}
		assets = append(assets, persistence.Stage6PenetrationAsset{AssetType: value.AssetType, ArtifactSHA256: digest, Tested: true})
	}
	slices.SortFunc(assets, func(a, b persistence.Stage6PenetrationAsset) int { return strings.Compare(a.AssetType, b.AssetType) })
	return assets, nil
}

func validAssessor(value map[string]json.RawMessage) bool {
	if !exactKeys(value, []string{"organizationReference", "engagementReference", "thirdParty", "independent", "noConflictDeclared"}) {
		return false
	}
	var item struct {
		OrganizationReference string `json:"organizationReference"`
		EngagementReference   string `json:"engagementReference"`
		ThirdParty            bool   `json:"thirdParty"`
		Independent           bool   `json:"independent"`
		NoConflictDeclared    bool   `json:"noConflictDeclared"`
	}
	return json.Unmarshal(mustJSON(value), &item) == nil && item.OrganizationReference != "" && item.EngagementReference != "" && item.ThirdParty && item.Independent && item.NoConflictDeclared
}

func validStage5(value map[string]json.RawMessage, commit, profile string) bool {
	if !exactKeys(value, []string{"status", "acceptedCommit", "acceptedCommitEqualsReleaseCommit", "scopeProfile", "scopeProfileMatchesDeployment", "targetRevalidated", "declaredSatisfied"}) {
		return false
	}
	var item struct {
		Status                            string `json:"status"`
		AcceptedCommit                    string `json:"acceptedCommit"`
		AcceptedCommitEqualsReleaseCommit bool   `json:"acceptedCommitEqualsReleaseCommit"`
		ScopeProfile                      string `json:"scopeProfile"`
		ScopeProfileMatchesDeployment     bool   `json:"scopeProfileMatchesDeployment"`
		TargetRevalidated                 bool   `json:"targetRevalidated"`
		DeclaredSatisfied                 bool   `json:"declaredSatisfied"`
	}
	return json.Unmarshal(mustJSON(value), &item) == nil && item.Status == "accepted-current-supported-surface" && item.AcceptedCommit == commit && item.ScopeProfile == profile && item.AcceptedCommitEqualsReleaseCommit && item.ScopeProfileMatchesDeployment && item.TargetRevalidated && item.DeclaredSatisfied
}

func validScope(value map[string]map[string]json.RawMessage) bool {
	if !exactKeys(value, scopeAreas) {
		return false
	}
	for _, area := range scopeAreas {
		if !exactKeys(value[area], []string{"tested", "result"}) {
			return false
		}
		var item struct {
			Tested bool   `json:"tested"`
			Result string `json:"result"`
		}
		if json.Unmarshal(mustJSON(value[area]), &item) != nil || !item.Tested || !slices.Contains([]string{"no-finding", "finding-recorded"}, item.Result) {
			return false
		}
	}
	return true
}

func validMethodologies(values []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if !slices.Contains(allowedMethodologies, value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	for _, required := range requiredMethodologies {
		if _, ok := seen[required]; !ok {
			return false
		}
	}
	return true
}

func validateFindings(findings []json.RawMessage, declared map[string]map[string]int64, started, completed time.Time) (bool, bool) {
	severityKeys := []string{"critical", "high", "medium", "low", "informational"}
	statusKeys := []string{"open", "remediation-in-progress", "remediated-verified", "risk-accepted"}
	counts := map[string]map[string]int64{"bySeverity": {}, "byStatus": {}}
	for _, key := range severityKeys {
		counts["bySeverity"][key] = 0
	}
	for _, key := range statusKeys {
		counts["byStatus"][key] = 0
	}
	seen := map[string]struct{}{}
	noUnaccepted := true
	for _, raw := range findings {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || !exactKeys(fields, []string{"findingId", "severity", "status", "affectedAssetTypes", "discoveredAt", "lastReviewedAt", "retestPassed", "riskAcceptance"}) {
			return false, false
		}
		var finding struct {
			FindingID          string          `json:"findingId"`
			Severity           string          `json:"severity"`
			Status             string          `json:"status"`
			AffectedAssetTypes []string        `json:"affectedAssetTypes"`
			DiscoveredAt       time.Time       `json:"discoveredAt"`
			LastReviewedAt     time.Time       `json:"lastReviewedAt"`
			RetestPassed       bool            `json:"retestPassed"`
			RiskAcceptance     json.RawMessage `json:"riskAcceptance"`
		}
		if json.Unmarshal(raw, &finding) != nil {
			return false, false
		}
		if finding.FindingID == "" || !slices.Contains(severityKeys, finding.Severity) || !slices.Contains(statusKeys, finding.Status) {
			return false, false
		}
		if _, duplicate := seen[finding.FindingID]; duplicate {
			return false, false
		}
		seen[finding.FindingID] = struct{}{}
		if len(finding.AffectedAssetTypes) == 0 || !uniqueKnownValues(finding.AffectedAssetTypes, assetTypes) || finding.DiscoveredAt.Before(started) || finding.LastReviewedAt.Before(finding.DiscoveredAt) || finding.LastReviewedAt.After(completed) {
			return false, false
		}
		riskIsNull := len(finding.RiskAcceptance) == 0 || bytes.Equal(finding.RiskAcceptance, []byte("null"))
		switch finding.Status {
		case "remediated-verified":
			if !finding.RetestPassed || !riskIsNull {
				return false, false
			}
		case "risk-accepted":
			if finding.RetestPassed || riskIsNull || !validRiskAcceptance(finding.RiskAcceptance, finding.LastReviewedAt, completed) {
				return false, false
			}
		default:
			if finding.RetestPassed || !riskIsNull {
				return false, false
			}
		}
		counts["bySeverity"][finding.Severity]++
		counts["byStatus"][finding.Status]++
		if finding.Severity == "critical" && finding.Status != "remediated-verified" {
			noUnaccepted = false
		}
		if finding.Severity == "high" && !slices.Contains([]string{"remediated-verified", "risk-accepted"}, finding.Status) {
			noUnaccepted = false
		}
	}
	return noUnaccepted, mapsEqual(counts, declared)
}

func uniqueKnownValues(values, allowed []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if !slices.Contains(allowed, value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validRiskAcceptance(raw json.RawMessage, reviewedAt, completedAt time.Time) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || !exactKeys(fields, []string{"acceptanceId", "securityApprover", "businessApprover", "approvedAt", "expiresAt", "compensatingControlIds"}) {
		return false
	}
	var item struct {
		AcceptanceID           string    `json:"acceptanceId"`
		SecurityApprover       string    `json:"securityApprover"`
		BusinessApprover       string    `json:"businessApprover"`
		ApprovedAt             time.Time `json:"approvedAt"`
		ExpiresAt              time.Time `json:"expiresAt"`
		CompensatingControlIDs []string  `json:"compensatingControlIds"`
	}
	return json.Unmarshal(raw, &item) == nil && item.AcceptanceID != "" && item.SecurityApprover != "" && item.BusinessApprover != "" && item.SecurityApprover != item.BusinessApprover && !item.ApprovedAt.Before(reviewedAt) && !item.ApprovedAt.After(completedAt) && item.ExpiresAt.After(completedAt) && !item.ExpiresAt.After(item.ApprovedAt.Add(30*24*time.Hour)) && len(item.CompensatingControlIDs) > 0 && uniqueKnownIdentifiers(item.CompensatingControlIDs)
}

func uniqueKnownIdentifiers(values []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func mapsEqual(a, b map[string]map[string]int64) bool {
	encodedA, _ := json.Marshal(a)
	encodedB, _ := json.Marshal(b)
	return bytes.Equal(encodedA, encodedB)
}

func validEvidenceSet(value map[string]map[string]json.RawMessage) bool {
	if !exactKeys(value, evidenceFields) {
		return false
	}
	paths := map[string]struct{}{}
	for _, key := range evidenceFields {
		if !validEvidenceReference(value[key]) {
			return false
		}
		var reference struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(mustJSON(value[key]), &reference)
		if _, duplicate := paths[reference.Path]; duplicate {
			return false
		}
		paths[reference.Path] = struct{}{}
	}
	return true
}

func validEvidenceReference(value map[string]json.RawMessage) bool {
	if !exactKeys(value, []string{"path", "sha256"}) {
		return false
	}
	var reference struct{ Path, SHA256 string }
	if json.Unmarshal(mustJSON(value), &reference) != nil {
		return false
	}
	return reference.Path != "" && !strings.Contains(reference.Path, "..") && !strings.HasPrefix(reference.Path, "/") && parseDigestOnly(reference.SHA256)
}

func mustJSON(value any) []byte        { encoded, _ := json.Marshal(value); return encoded }
func formatDigest(value []byte) string { return "sha256:" + hex.EncodeToString(value) }
func parseDigest(raw string) ([]byte, error) {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	if len(value) != 64 {
		return nil, errors.New("invalid digest")
	}
	return hex.DecodeString(value)
}
func parseDigestOnly(raw string) bool { _, err := parseDigest(raw); return err == nil }
func validHTTPSReference(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && len(raw) <= 2048
}

func validApprovalEvidenceSHA256(value string) bool {
	return approvalEvidenceSHA256Pattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}
func invalidReceipt(detail string) error {
	return problem.New(400, "penetration_receipt_invalid", detail)
}
func exactKeys[T any](values map[string]T, expected []string) bool {
	if len(values) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

package internalcostgovernance

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
	receiptSchema     = "synara.stage6-internal-cost-evidence-validation.v1"
	receiptAssessment = "evidence-validated-not-internal-cost-approved"
	receiptMaxBytes   = 2 * 1024 * 1024
	requiredEvidence  = 4
)

var (
	approvalRoles = []string{"operations", "owner"}
	evidenceIDs   = []string{"platform-allocation-export", "provider-cost-export", "reconciliation-report", "usage-export"}
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$`)
	migrationName = regexp.MustCompile(`^[0-9]{6}_[a-z0-9_]+\.sql$`)
	shaPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	currency      = regexp.MustCompile(`^[A-Z]{3}$`)
	forbiddenKeys = []string{"stripe", "payment", "checkout", "portal", "invoice", "tax", "card", "settlement", "subscription"}
)

type Approval struct {
	ID                uuid.UUID  `json:"id"`
	Role              string     `json:"role"`
	Decision          string     `json:"decision"`
	ApproverUserID    uuid.UUID  `json:"approverUserId"`
	ApproverEmail     string     `json:"approverEmail"`
	ApproverName      string     `json:"approverName"`
	Reason            string     `json:"reason"`
	EvidenceReference string     `json:"evidenceReference"`
	EvidenceSHA256    string     `json:"evidenceSha256"`
	SupersededAt      *time.Time `json:"supersededAt"`
	SupersededReason  *string    `json:"supersededReason"`
	CreatedAt         time.Time  `json:"createdAt"`
}

type Review struct {
	ID                                             uuid.UUID        `json:"id"`
	CandidateRecordID                              uuid.UUID        `json:"candidateRecordId"`
	CandidateID                                    string           `json:"candidateId"`
	ReceiptSHA256                                  string           `json:"receiptSha256"`
	ReceiptSizeBytes                               int64            `json:"receiptSizeBytes"`
	ReleaseCommit                                  string           `json:"releaseCommit"`
	EnvironmentClass                               string           `json:"environmentClass"`
	EnvironmentID                                  string           `json:"environmentId"`
	ManifestSHA256                                 string           `json:"manifestSha256"`
	ControlPlaneOrigin                             string           `json:"controlPlaneOrigin"`
	MigrationName                                  string           `json:"migrationName"`
	MigrationSHA256                                string           `json:"migrationSha256"`
	PeriodStart                                    time.Time        `json:"periodStart"`
	PeriodEnd                                      time.Time        `json:"periodEnd"`
	ValidatedAt                                    time.Time        `json:"validatedAt"`
	ExecutionCount                                 int64            `json:"executionCount"`
	InputTokens                                    int64            `json:"inputTokens"`
	OutputTokens                                   int64            `json:"outputTokens"`
	CachedInputTokens                              int64            `json:"cachedInputTokens"`
	CacheCreationInputTokens                       int64            `json:"cacheCreationInputTokens"`
	ProviderCostReportedExecutionCount             int64            `json:"providerCostReportedExecutionCount"`
	ProviderCostUnavailableExecutionCount          int64            `json:"providerCostUnavailableExecutionCount"`
	ActualPlatformAllocationCount                  int64            `json:"actualPlatformAllocationCount"`
	EstimatedPlatformAllocationCount               int64            `json:"estimatedPlatformAllocationCount"`
	ProviderCostByCurrency                         map[string]int64 `json:"providerCostByCurrency"`
	PlatformCostByCurrency                         map[string]int64 `json:"platformCostByCurrency"`
	KnownCostByCurrency                            map[string]int64 `json:"knownCostByCurrency"`
	EvidenceFileCount                              int64            `json:"evidenceFileCount"`
	TokenTotalsReconciled                          bool             `json:"tokenTotalsReconciled"`
	ProviderCoverageComplete                       bool             `json:"providerCoverageComplete"`
	ActualOverridesEstimate                        bool             `json:"actualOverridesEstimate"`
	CurrencySafeAggregation                        bool             `json:"currencySafeAggregation"`
	TenantIsolationValidated                       bool             `json:"tenantIsolationValidated"`
	NoPaymentDataPresent                           bool             `json:"noPaymentDataPresent"`
	EligibleForHumanGateReview                     bool             `json:"eligibleForHumanGateReview"`
	CryptographicSignaturesVerified                bool             `json:"cryptographicSignaturesVerified"`
	ExternalSourceAndAuthorityVerificationRequired bool             `json:"externalSourceAndAuthorityVerificationRequired"`
	State                                          string           `json:"state"`
	Version                                        int64            `json:"version"`
	CreatedBy                                      uuid.UUID        `json:"createdBy"`
	Approvals                                      []Approval       `json:"approvals"`
	ApprovedAt                                     *time.Time       `json:"approvedAt"`
	RejectedAt                                     *time.Time       `json:"rejectedAt"`
	CreatedAt                                      time.Time        `json:"createdAt"`
	UpdatedAt                                      time.Time        `json:"updatedAt"`
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
	return &Service{
		db: db, operatorTenantID: operatorTenantID,
		authority: governanceauthority.NewService(db, operatorTenantID),
		now:       func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(ctx context.Context) ([]Review, error) {
	var models []persistence.Stage6InternalCostReview
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "internal_cost_reviews_load_failed", "Internal cost reviews could not be loaded.", err)
	}
	items := make([]Review, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) Import(ctx context.Context, actorID uuid.UUID, input ImportInput, requestID, ipAddress string) (Review, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", input.CandidateRecordID, s.operatorTenantID).Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Review{}, problem.New(404, "internal_cost_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Review{}, problem.Wrap(500, "internal_cost_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Review{}, problem.New(403, "internal_cost_import_forbidden", "The release candidate creator must import its exact internal cost receipt.")
	}
	model, _, err := bindReceipt(input, candidate, s.now())
	if err != nil {
		return Review{}, err
	}
	model.ID = uuid.New()
	model.OperatorTenantID = s.operatorTenantID
	model.CandidateRecordID = candidate.ID
	model.CreatedBy = actorID
	model.CreatedAt = s.now()
	model.UpdatedAt = model.CreatedAt
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "internal_cost_review_exists", "This exact internal cost review already exists.")
		} else if err != nil {
			return problem.Wrap(500, "internal_cost_review_import_failed", "The internal cost review could not be imported.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "internal_cost.review_imported", ResourceType: "stage6_internal_cost_review", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"candidateId": candidate.CandidateID, "receiptSha256": input.ReceiptSHA256, "executionCount": model.ExecutionCount},
		})
	}); err != nil {
		return Review{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, reviewID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Review, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(approvalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) ||
		len(input.Reason) < 20 || len(input.Reason) > 2000 || !validHTTPS(input.EvidenceReference) ||
		!shaPattern.MatchString(input.EvidenceSHA256) || input.EvidenceSHA256 == "sha256:"+strings.Repeat("0", 64) {
		return Review{}, problem.New(400, "internal_cost_approval_invalid", "Internal cost approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero lowercase SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6InternalCostReview
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", reviewID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "internal_cost_review_not_found", "The internal cost review was not found.")
		} else if err != nil {
			return problem.Wrap(500, "internal_cost_review_load_failed", "The internal cost review could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "internal_cost_review_not_reviewable", "The internal cost review is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "internal_cost_review_ineligible", "An ineligible internal cost review cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.InternalCostPrefix+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6InternalCostReviewApproval{
			ID: uuid.New(), InternalCostID: model.ID, OperatorTenantID: s.operatorTenantID,
			Role: input.Role, Decision: input.Decision, ApproverUserID: actorID,
			Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: input.EvidenceSHA256, CreatedAt: s.now(),
		}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "internal_cost_approval_conflict", "This role or operator already recorded an internal cost decision.")
		} else if err != nil {
			return problem.Wrap(500, "internal_cost_approval_create_failed", "The internal cost decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "internal_cost.review_approval_recorded", ResourceType: "stage6_internal_cost_review", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"role": input.Role, "decision": input.Decision, "evidenceReference": input.EvidenceReference, "evidenceSha256": input.EvidenceSHA256},
		}); err != nil {
			return err
		}
		target := ""
		if input.Decision == "rejected" {
			target = "rejected"
		} else {
			var count int64
			if err := tx.Model(&persistence.Stage6InternalCostReviewApproval{}).
				Where("internal_cost_review_id = ? AND decision = ? AND superseded_at IS NULL", model.ID, "approved").Count(&count).Error; err != nil {
				return problem.Wrap(500, "internal_cost_approvals_load_failed", "Internal cost decisions could not be verified.", err)
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
		result := tx.Model(&persistence.Stage6InternalCostReview{}).
			Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "internal_cost_transition_failed", "The internal cost decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "internal_cost_version_conflict", "The internal cost review changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "internal_cost.review_" + target, ResourceType: "stage6_internal_cost_review", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Review{}, err
	}
	return s.get(ctx, reviewID)
}

type receiptCandidate struct {
	CandidateID         string           `json:"candidateId"`
	SourceCommit        string           `json:"sourceCommit"`
	Environment         string           `json:"environment"`
	EnvironmentID       string           `json:"environmentId"`
	ControlPlaneBaseURL string           `json:"controlPlaneBaseUrl"`
	MigrationTail       receiptMigration `json:"migrationTail"`
}
type receiptMigration struct{ Name, SHA256 string }
type receiptPeriod struct{ Start, End string }
type receiptUsage struct {
	ExecutionCount, InputTokens, OutputTokens, CachedInputTokens, CacheCreationInputTokens int64
	ProviderCostReportedExecutionCount, ProviderCostUnavailableExecutionCount              int64
	ActualPlatformAllocationCount, EstimatedPlatformAllocationCount                        int64
}
type receiptCosts struct {
	ProviderByCurrency map[string]int64 `json:"providerByCurrency"`
	PlatformByCurrency map[string]int64 `json:"platformByCurrency"`
	KnownByCurrency    map[string]int64 `json:"knownByCurrency"`
}
type receiptControls struct {
	TokenTotalsReconciled, ProviderCoverageComplete, ActualOverridesEstimate bool
	CurrencySafeAggregation, TenantIsolationValidated, NoPaymentDataPresent  bool
}
type receiptManifest struct{ Path, SHA256 string }
type receiptEvidence struct{ ID, Path, SHA256 string }
type receiptDocument struct {
	SchemaVersion                                  string            `json:"schemaVersion"`
	Assessment                                     string            `json:"assessment"`
	Candidate                                      receiptCandidate  `json:"candidate"`
	Period                                         receiptPeriod     `json:"period"`
	Usage                                          receiptUsage      `json:"usage"`
	Costs                                          receiptCosts      `json:"costs"`
	Controls                                       receiptControls   `json:"controls"`
	Manifest                                       receiptManifest   `json:"manifest"`
	Evidence                                       []receiptEvidence `json:"evidence"`
	EvidenceFileCount                              int64             `json:"evidenceFileCount"`
	CryptographicSignaturesVerified                bool              `json:"cryptographicSignaturesVerified"`
	ExternalSourceAndAuthorityVerificationRequired bool              `json:"externalSourceAndAuthorityVerificationRequired"`
	EligibleForHumanGateReview                     bool              `json:"eligibleForHumanGateReview"`
	ValidatedAt                                    string            `json:"validatedAt"`
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (persistence.Stage6InternalCostReview, receiptDocument, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt must be bounded UTF-8 JSON.")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || !exactKeys(raw, []string{
		"assessment", "candidate", "controls", "costs", "cryptographicSignaturesVerified", "eligibleForHumanGateReview",
		"evidence", "evidenceFileCount", "externalSourceAndAuthorityVerificationRequired", "manifest", "period", "schemaVersion", "usage", "validatedAt",
	}) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt schema is invalid.")
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil || containsPaymentKey(generic, "") {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt contains forbidden payment semantics.")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var receipt receiptDocument
	if err := decoder.Decode(&receipt); err != nil {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt schema is invalid.")
	}
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment || !receipt.EligibleForHumanGateReview ||
		receipt.CryptographicSignaturesVerified || !receipt.ExternalSourceAndAuthorityVerificationRequired || receipt.EvidenceFileCount != requiredEvidence {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt is not eligible for review.")
	}
	if receipt.Candidate.CandidateID != candidate.CandidateID || receipt.Candidate.SourceCommit != candidate.SourceCommit ||
		receipt.Candidate.EnvironmentID != candidate.EnvironmentID || !commitPattern.MatchString(receipt.Candidate.SourceCommit) ||
		!idPattern.MatchString(receipt.Candidate.CandidateID) || !idPattern.MatchString(receipt.Candidate.EnvironmentID) ||
		(receipt.Candidate.Environment != "production" && receipt.Candidate.Environment != "production-like") || !validHTTPS(receipt.Candidate.ControlPlaneBaseURL) ||
		!migrationName.MatchString(receipt.Candidate.MigrationTail.Name) || !validSHA(receipt.Candidate.MigrationTail.SHA256) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost candidate identity is invalid or mismatched.")
	}
	periodStart, errStart := time.Parse(time.RFC3339, receipt.Period.Start)
	periodEnd, errEnd := time.Parse(time.RFC3339, receipt.Period.End)
	validatedAt, errValidated := time.Parse(time.RFC3339, receipt.ValidatedAt)
	if errStart != nil || errEnd != nil || errValidated != nil || !strings.HasSuffix(receipt.Period.Start, "Z") || !strings.HasSuffix(receipt.Period.End, "Z") ||
		!strings.HasSuffix(receipt.ValidatedAt, "Z") || !periodStart.Before(periodEnd) || periodEnd.Sub(periodStart) > 93*24*time.Hour || validatedAt.After(now.Add(5*time.Minute)) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost period or validation timestamp is invalid.")
	}
	usage := receipt.Usage
	if usage.ExecutionCount < 1 || min64(usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.CacheCreationInputTokens,
		usage.ProviderCostReportedExecutionCount, usage.ProviderCostUnavailableExecutionCount, usage.ActualPlatformAllocationCount, usage.EstimatedPlatformAllocationCount) < 0 ||
		usage.ProviderCostReportedExecutionCount+usage.ProviderCostUnavailableExecutionCount != usage.ExecutionCount ||
		usage.ActualPlatformAllocationCount+usage.EstimatedPlatformAllocationCount != usage.ExecutionCount {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost usage coverage is incomplete or double-counted.")
	}
	if !receipt.Controls.TokenTotalsReconciled || !receipt.Controls.ProviderCoverageComplete || !receipt.Controls.ActualOverridesEstimate ||
		!receipt.Controls.CurrencySafeAggregation || !receipt.Controls.TenantIsolationValidated || !receipt.Controls.NoPaymentDataPresent ||
		!validCostArithmetic(receipt.Costs) || !validEvidence(receipt.Evidence) || !validSHA(receipt.Manifest.SHA256) || strings.TrimSpace(receipt.Manifest.Path) == "" {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost projections or evidence are incomplete.")
	}
	digest := sha256.Sum256(data)
	expectedDigest, err := parseSHA(input.ReceiptSHA256)
	if err != nil || allZero(expectedDigest) || !bytes.Equal(expectedDigest, digest[:]) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Internal cost receipt SHA-256 does not match its bytes.")
	}
	manifestDigest, _ := parseSHA(receipt.Manifest.SHA256)
	migrationDigest, _ := parseSHA(receipt.Candidate.MigrationTail.SHA256)
	if !candidateBindsReceipt(candidate, input.ReceiptSHA256, receipt.Candidate) {
		return persistence.Stage6InternalCostReview{}, receiptDocument{}, invalidReceipt("Release candidate does not bind this exact internal cost receipt.")
	}
	return persistence.Stage6InternalCostReview{
		Receipt: data, ReceiptSHA256: digest[:], ReceiptSizeBytes: int64(len(data)), Schema: receipt.SchemaVersion, Assessment: receipt.Assessment,
		ReleaseCommit: receipt.Candidate.SourceCommit, EnvironmentClass: receipt.Candidate.Environment, EnvironmentID: receipt.Candidate.EnvironmentID,
		ManifestSHA256: manifestDigest, ControlPlaneOrigin: receipt.Candidate.ControlPlaneBaseURL, MigrationName: receipt.Candidate.MigrationTail.Name, MigrationSHA256: migrationDigest,
		PeriodStart: periodStart, PeriodEnd: periodEnd, ValidatedAt: validatedAt, ExecutionCount: usage.ExecutionCount,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CachedInputTokens: usage.CachedInputTokens, CacheCreationInputTokens: usage.CacheCreationInputTokens,
		ProviderCostReportedExecutionCount: usage.ProviderCostReportedExecutionCount, ProviderCostUnavailableExecutionCount: usage.ProviderCostUnavailableExecutionCount,
		ActualPlatformAllocationCount: usage.ActualPlatformAllocationCount, EstimatedPlatformAllocationCount: usage.EstimatedPlatformAllocationCount,
		EvidenceFileCount: receipt.EvidenceFileCount, TokenTotalsReconciled: true, ProviderCoverageComplete: true, ActualOverridesEstimate: true,
		CurrencySafeAggregation: true, TenantIsolationValidated: true, NoPaymentDataPresent: true, EligibleForHumanGateReview: true,
		CryptographicSignaturesVerified: false, ExternalSourceAndAuthorityVerificationRequired: true, State: "recorded", Version: 1,
	}, receipt, nil
}

func candidateBindsReceipt(candidate persistence.Stage6ReleaseCandidate, digest string, identity receiptCandidate) bool {
	var envelope struct {
		Candidate struct {
			EnvironmentClass string           `json:"environmentClass"`
			MigrationTail    receiptMigration `json:"migrationTail"`
		} `json:"candidate"`
		Receipts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"receipts"`
	}
	if json.Unmarshal(candidate.EvidenceBundleReceipt, &envelope) != nil {
		return false
	}
	projection, ok := envelope.Receipts["internalCost"]
	return ok && projection.SHA256 == digest && envelope.Candidate.EnvironmentClass == identity.Environment &&
		envelope.Candidate.MigrationTail == identity.MigrationTail
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Review, error) {
	var model persistence.Stage6InternalCostReview
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; err != nil {
		return Review{}, problem.Wrap(500, "internal_cost_review_load_failed", "The internal cost review could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

type approvalRow struct {
	persistence.Stage6InternalCostReviewApproval
	ApproverEmail string `gorm:"column:approver_email"`
	ApproverName  string `gorm:"column:approver_name"`
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6InternalCostReview) (Review, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Review{}, problem.Wrap(500, "internal_cost_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_internal_cost_review_approvals AS approval").
		Select("approval.*, users.email AS approver_email, users.display_name AS approver_name").
		Joins("JOIN users ON users.id = approval.approver_user_id").Where("approval.internal_cost_review_id = ?", model.ID).
		Order("approval.created_at, approval.id").Find(&rows).Error; err != nil {
		return Review{}, problem.Wrap(500, "internal_cost_approvals_load_failed", "Internal cost decisions could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, Approval{
			ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID,
			ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName, Reason: row.Reason,
			EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256,
			SupersededAt: row.SupersededAt, SupersededReason: row.SupersededReason, CreatedAt: row.CreatedAt,
		})
	}
	var receipt receiptDocument
	if json.Unmarshal(model.Receipt, &receipt) != nil {
		return Review{}, problem.New(500, "internal_cost_receipt_corrupt", "The stored internal cost receipt is invalid.")
	}
	return Review{
		ID: model.ID, CandidateRecordID: model.CandidateRecordID, CandidateID: candidate.CandidateID,
		ReceiptSHA256: "sha256:" + hex.EncodeToString(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes,
		ReleaseCommit: model.ReleaseCommit, EnvironmentClass: model.EnvironmentClass, EnvironmentID: model.EnvironmentID,
		ManifestSHA256: "sha256:" + hex.EncodeToString(model.ManifestSHA256), ControlPlaneOrigin: model.ControlPlaneOrigin,
		MigrationName: model.MigrationName, MigrationSHA256: "sha256:" + hex.EncodeToString(model.MigrationSHA256),
		PeriodStart: model.PeriodStart, PeriodEnd: model.PeriodEnd, ValidatedAt: model.ValidatedAt, ExecutionCount: model.ExecutionCount,
		InputTokens: model.InputTokens, OutputTokens: model.OutputTokens, CachedInputTokens: model.CachedInputTokens,
		CacheCreationInputTokens: model.CacheCreationInputTokens, ProviderCostReportedExecutionCount: model.ProviderCostReportedExecutionCount,
		ProviderCostUnavailableExecutionCount: model.ProviderCostUnavailableExecutionCount, ActualPlatformAllocationCount: model.ActualPlatformAllocationCount,
		EstimatedPlatformAllocationCount: model.EstimatedPlatformAllocationCount, ProviderCostByCurrency: receipt.Costs.ProviderByCurrency,
		PlatformCostByCurrency: receipt.Costs.PlatformByCurrency, KnownCostByCurrency: receipt.Costs.KnownByCurrency,
		EvidenceFileCount: model.EvidenceFileCount, TokenTotalsReconciled: model.TokenTotalsReconciled,
		ProviderCoverageComplete: model.ProviderCoverageComplete, ActualOverridesEstimate: model.ActualOverridesEstimate,
		CurrencySafeAggregation: model.CurrencySafeAggregation, TenantIsolationValidated: model.TenantIsolationValidated,
		NoPaymentDataPresent: model.NoPaymentDataPresent, EligibleForHumanGateReview: model.EligibleForHumanGateReview,
		CryptographicSignaturesVerified:                model.CryptographicSignaturesVerified,
		ExternalSourceAndAuthorityVerificationRequired: model.ExternalSourceAndAuthorityVerificationRequired,
		State: model.State, Version: model.Version, CreatedBy: model.CreatedBy, Approvals: approvals,
		ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

func validCostArithmetic(costs receiptCosts) bool {
	if len(costs.ProviderByCurrency) == 0 || len(costs.PlatformByCurrency) == 0 || len(costs.KnownByCurrency) == 0 {
		return false
	}
	currencies := map[string]struct{}{}
	for code, amount := range costs.ProviderByCurrency {
		if !currency.MatchString(code) || amount < 0 {
			return false
		}
		currencies[code] = struct{}{}
	}
	for code, amount := range costs.PlatformByCurrency {
		if !currency.MatchString(code) || amount < 0 {
			return false
		}
		currencies[code] = struct{}{}
	}
	if len(costs.KnownByCurrency) != len(currencies) {
		return false
	}
	for code := range currencies {
		if costs.KnownByCurrency[code] != costs.ProviderByCurrency[code]+costs.PlatformByCurrency[code] {
			return false
		}
	}
	return true
}

func validEvidence(items []receiptEvidence) bool {
	if len(items) != requiredEvidence {
		return false
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !slices.Contains(evidenceIDs, item.ID) || strings.TrimSpace(item.Path) == "" || strings.Contains(item.Path, "..") || !validSHA(item.SHA256) {
			return false
		}
		ids = append(ids, item.ID)
	}
	slices.Sort(ids)
	return slices.Equal(ids, evidenceIDs)
}

func containsPaymentKey(value any, path string) bool {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			normalized := strings.ToLower(key)
			allowed := path == "controls" && key == "noPaymentDataPresent"
			if !allowed {
				for _, token := range forbiddenKeys {
					if strings.Contains(normalized, token) {
						return true
					}
				}
			}
			next := key
			if path != "" {
				next = path + "." + key
			}
			if containsPaymentKey(child, next) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if containsPaymentKey(child, path) {
				return true
			}
		}
	}
	return false
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

func validHTTPS(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}
func validSHA(value string) bool {
	return shaPattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}
func parseSHA(value string) ([]byte, error) {
	if !shaPattern.MatchString(value) {
		return nil, errors.New("invalid sha256")
	}
	return hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
}
func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
func min64(values ...int64) int64 {
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}
func invalidReceipt(message string) error {
	return problem.New(400, "internal_cost_receipt_invalid", message)
}

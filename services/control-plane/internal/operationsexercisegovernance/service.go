package operationsexercisegovernance

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
	receiptSchema          = "synara.stage6-operations-browser-exercise-validation.v2"
	receiptAssessment      = "evidence-validated-not-operations-passed"
	receiptMaxBytes        = 2 * 1024 * 1024
	requiredAccountCount   = 11
	requiredOperationCount = 48
	requiredMatrixProfile  = "internal-self-hosted-v4"
)

var (
	approvalRoles = []string{"operations", "security"}
	accountRoles  = []string{
		"authenticated-user", "platform-operator", "platform-admin", "platform-owner", "support-engineer",
		"tenant-owner", "tenant-admin", "security-admin", "cost-admin", "auditor", "tenant-member",
	}
	commitPattern                 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	identifierPattern             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$`)
	approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	expectedOperations            = map[string]operationPolicy{
		"platform.tenant-overview":                 {"platform-admin", "platform-operator", "tenant-admin", "read"},
		"platform.tenant-provision":                {"platform-admin", "platform-admin", "platform-operator", "write"},
		"platform.tenant-entitlement":              {"platform-admin", "platform-admin", "platform-operator", "write"},
		"platform.release-candidate-lifecycle":     {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.release-role-decision":           {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.compliance-program-lifecycle":    {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.compliance-evidence-review":      {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.compliance-start-gate-decision":  {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.provider-commercial-lifecycle":   {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.provider-commercial-approval":    {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.governance-authority-lifecycle":  {"platform-admin", "platform-owner", "platform-admin", "four-eyes"},
		"platform.incident-lifecycle":              {"platform-admin", "platform-operator", "tenant-admin", "write"},
		"platform.incident-public-communication":   {"platform-admin", "platform-operator", "tenant-admin", "write"},
		"platform.incident-security-resolution":    {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.slo-window-import":               {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.slo-window-decision":             {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.recovery-drill-import":           {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.recovery-drill-decision":         {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.penetration-engagement-import":   {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.penetration-engagement-decision": {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
		"platform.capacity-run-import":             {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.capacity-run-decision":           {"platform-admin", "platform-operator", "tenant-admin", "four-eyes"},
		"platform.incident-exercise-import":        {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.incident-exercise-decision":      {"platform-admin", "platform-operator", "tenant-admin", "four-eyes"},
		"platform.internal-cost-review-import":     {"platform-admin", "platform-admin", "security-admin", "write"},
		"platform.internal-cost-review-decision":   {"platform-admin", "platform-operator", "tenant-admin", "four-eyes"},
		"user.tenant-self-service":                 {"tenant-web", "authenticated-user", "unauthenticated", "write"},
		"platform.support-request":                 {"platform-admin", "platform-operator", "tenant-admin", "write"},
		"platform.support-approve-revoke":          {"platform-admin", "platform-admin", "platform-operator", "four-eyes"},
		"support.enter-readonly":                   {"platform-admin", "support-engineer", "tenant-admin", "read-only"},
		"tenant.lifecycle-transition":              {"tenant-web", "tenant-owner", "tenant-admin", "write"},
		"tenant.deletion-request-recovery":         {"tenant-web", "tenant-owner", "tenant-admin", "write"},
		"tenant.member-governance":                 {"tenant-web", "tenant-admin", "auditor", "write"},
		"tenant.identity-governance":               {"tenant-web", "security-admin", "tenant-member", "write"},
		"tenant.service-account-governance":        {"tenant-web", "security-admin", "tenant-member", "write"},
		"tenant.credential-governance":             {"tenant-web", "security-admin", "tenant-member", "write"},
		"support.credential-read":                  {"tenant-web", "support-engineer", "tenant-member", "read-only"},
		"tenant.worker-governance":                 {"tenant-web", "tenant-admin", "auditor", "write"},
		"tenant.worker-release":                    {"tenant-web", "tenant-admin", "auditor", "write"},
		"tenant.outbox-read":                       {"tenant-web", "support-engineer", "tenant-member", "read-only"},
		"tenant.outbox-replay":                     {"tenant-web", "tenant-admin", "auditor", "write"},
		"tenant.audit-search-export":               {"tenant-web", "auditor", "tenant-member", "read"},
		"tenant.legal-hold":                        {"tenant-web", "security-admin", "tenant-member", "write"},
		"tenant.privacy-request":                   {"tenant-web", "security-admin", "tenant-member", "write"},
		"tenant.data-export":                       {"tenant-web", "security-admin", "tenant-member", "write"},
		"tenant.data-residency":                    {"tenant-web", "security-admin", "tenant-member", "write"},
		"tenant.usage-quota":                       {"tenant-web", "cost-admin", "auditor", "write"},
		"tenant.support-policy":                    {"tenant-web", "tenant-admin", "auditor", "write"},
	}
)

type operationPolicy struct {
	Surface, Actor, NegativeActor, Access string
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

type Exercise struct {
	ID                                    uuid.UUID  `json:"id"`
	CandidateRecordID                     uuid.UUID  `json:"candidateRecordId"`
	CandidateID                           string     `json:"candidateId"`
	ReceiptSHA256                         string     `json:"receiptSha256"`
	ReceiptSizeBytes                      int64      `json:"receiptSizeBytes"`
	ReleaseCommit                         string     `json:"releaseCommit"`
	EnvironmentClass                      string     `json:"environmentClass"`
	EnvironmentID                         string     `json:"environmentId"`
	MatrixSHA256                          string     `json:"matrixSha256"`
	WebOrigin                             string     `json:"webOrigin"`
	AdminOrigin                           string     `json:"adminOrigin"`
	StartedAt                             time.Time  `json:"startedAt"`
	CompletedAt                           time.Time  `json:"completedAt"`
	ValidatedAt                           time.Time  `json:"validatedAt"`
	AccountCount                          int64      `json:"accountCount"`
	OperationCount                        int64      `json:"operationCount"`
	MatrixProfile                         string     `json:"matrixProfile"`
	EvidenceFileCount                     int64      `json:"evidenceFileCount"`
	AllOperationsPassed                   bool       `json:"allOperationsPassed"`
	AllNegativeAuthorizationsDenied       bool       `json:"allNegativeAuthorizationsDenied"`
	NoDeveloperFallbacks                  bool       `json:"noDeveloperFallbacks"`
	ProductionAuthenticationDeclared      bool       `json:"productionAuthenticationDeclared"`
	SupportLifecycleComplete              bool       `json:"supportLifecycleComplete"`
	ReceiptApprovalsComplete              bool       `json:"receiptApprovalsComplete"`
	ReleaseEligibleEnvironment            bool       `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview            bool       `json:"eligibleForHumanGateReview"`
	CryptographicSignaturesVerified       bool       `json:"cryptographicSignaturesVerified"`
	ExternalAuthorityVerificationRequired bool       `json:"externalAuthorityVerificationRequired"`
	State                                 string     `json:"state"`
	Version                               int64      `json:"version"`
	CreatedBy                             uuid.UUID  `json:"createdBy"`
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
	return &Service{
		db: db, operatorTenantID: operatorTenantID,
		authority: governanceauthority.NewService(db, operatorTenantID),
		now:       func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(ctx context.Context) ([]Exercise, error) {
	var models []persistence.Stage6OperationsExercise
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "operations_exercises_load_failed", "Operations exercises could not be loaded.", err)
	}
	items := make([]Exercise, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) Import(ctx context.Context, actorID uuid.UUID, input ImportInput, requestID, ipAddress string) (Exercise, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", input.CandidateRecordID, s.operatorTenantID).Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Exercise{}, problem.New(404, "operations_exercise_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Exercise{}, problem.Wrap(500, "operations_exercise_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Exercise{}, problem.New(403, "operations_exercise_import_forbidden", "The release candidate creator must import its exact Operations exercise receipt.")
	}
	model, err := bindReceipt(input, candidate, s.now())
	if err != nil {
		return Exercise{}, err
	}
	model.ID = uuid.New()
	model.OperatorTenantID = s.operatorTenantID
	model.CandidateRecordID = candidate.ID
	model.CreatedBy = actorID
	model.CreatedAt = s.now()
	model.UpdatedAt = model.CreatedAt
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "operations_exercise_exists", "This exact Operations exercise already exists.")
		} else if err != nil {
			return problem.Wrap(500, "operations_exercise_import_failed", "The Operations exercise could not be imported.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "operations.exercise_imported", ResourceType: "stage6_operations_exercise", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"candidateId": candidate.CandidateID, "receiptSha256": input.ReceiptSHA256,
				"operationCount": model.OperationCount, "eligibleForHumanGateReview": model.EligibleForHumanGateReview,
			},
		})
	})
	if err != nil {
		return Exercise{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, exerciseID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Exercise, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(approvalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) ||
		len(input.Reason) < 20 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validApprovalEvidenceSHA256(input.EvidenceSHA256) {
		return Exercise{}, problem.New(400, "operations_exercise_approval_invalid", "Operations exercise approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero lowercase SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6OperationsExercise
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", exerciseID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "operations_exercise_not_found", "The Operations exercise was not found.")
		} else if err != nil {
			return problem.Wrap(500, "operations_exercise_load_failed", "The Operations exercise could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "operations_exercise_not_reviewable", "The Operations exercise is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "operations_exercise_ineligible", "An ineligible or failed Operations exercise cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.OperationsExercisePrefix+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6OperationsExerciseApproval{
			ID: uuid.New(), OperationsExerciseID: model.ID, OperatorTenantID: s.operatorTenantID,
			Role: input.Role, Decision: input.Decision, ApproverUserID: actorID,
			Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now(),
		}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "operations_exercise_approval_conflict", "This role or operator already recorded an Operations exercise decision.")
		} else if err != nil {
			return problem.Wrap(500, "operations_exercise_approval_create_failed", "The Operations exercise decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "operations.exercise_approval_recorded", ResourceType: "stage6_operations_exercise", ResourceID: &model.ID,
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
			if err := tx.Model(&persistence.Stage6OperationsExerciseApproval{}).Where("operations_exercise_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", model.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Count(&count).Error; err != nil {
				return problem.Wrap(500, "operations_exercise_approvals_load_failed", "Operations exercise decisions could not be verified.", err)
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
		result := tx.Model(&persistence.Stage6OperationsExercise{}).Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "operations_exercise_transition_failed", "The Operations exercise decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "operations_exercise_version_conflict", "The Operations exercise changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "operations.exercise_" + target, ResourceType: "stage6_operations_exercise", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Exercise{}, err
	}
	return s.get(ctx, exerciseID)
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Exercise, error) {
	var model persistence.Stage6OperationsExercise
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Exercise{}, problem.New(404, "operations_exercise_not_found", "The Operations exercise was not found.")
	} else if err != nil {
		return Exercise{}, problem.Wrap(500, "operations_exercise_load_failed", "The Operations exercise could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6OperationsExercise) (Exercise, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Exercise{}, problem.Wrap(500, "operations_exercise_candidate_load_failed", "The linked release candidate could not be loaded.", err)
	}
	type approvalRow struct {
		persistence.Stage6OperationsExerciseApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_operations_exercise_approvals approval").
		Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").
		Joins("JOIN users approver ON approver.id = approval.approver_user_id").
		Where("approval.operations_exercise_id = ?", model.ID).Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
		return Exercise{}, problem.Wrap(500, "operations_exercise_approvals_load_failed", "Operations exercise decisions could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, Approval{
			ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID,
			ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName,
			Reason: row.Reason, EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256,
			SupersededAt: row.SupersededAt, SupersededReason: row.SupersededReason, CreatedAt: row.CreatedAt,
		})
	}
	return Exercise{
		ID: model.ID, CandidateRecordID: model.CandidateRecordID, CandidateID: candidate.CandidateID,
		ReceiptSHA256: formatDigest(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes,
		ReleaseCommit: model.ReleaseCommit, EnvironmentClass: model.EnvironmentClass, EnvironmentID: model.EnvironmentID,
		MatrixSHA256: formatDigest(model.MatrixSHA256), WebOrigin: model.WebOrigin, AdminOrigin: model.AdminOrigin,
		StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, ValidatedAt: model.ValidatedAt,
		AccountCount: model.AccountCount, OperationCount: model.OperationCount, MatrixProfile: model.MatrixProfile, EvidenceFileCount: model.EvidenceFileCount,
		AllOperationsPassed:              model.AllOperationsPassed,
		AllNegativeAuthorizationsDenied:  model.AllNegativeAuthorizationsDenied,
		NoDeveloperFallbacks:             model.NoDeveloperFallbacks,
		ProductionAuthenticationDeclared: model.ProductionAuthenticationDeclared,
		SupportLifecycleComplete:         model.SupportLifecycleComplete, ReceiptApprovalsComplete: model.ReceiptApprovalsComplete,
		ReleaseEligibleEnvironment: model.ReleaseEligibleEnvironment, EligibleForHumanGateReview: model.EligibleForHumanGateReview,
		CryptographicSignaturesVerified:       model.CryptographicSignaturesVerified,
		ExternalAuthorityVerificationRequired: model.ExternalAuthorityVerificationRequired,
		State:                                 model.State, Version: model.Version, CreatedBy: model.CreatedBy, Approvals: approvals,
		ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

func validApprovalEvidenceSHA256(raw string) bool {
	return approvalEvidenceSHA256Pattern.MatchString(raw) && raw != "sha256:"+strings.Repeat("0", 64)
}

type receiptReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type receiptCandidate struct {
	CandidateID   string            `json:"candidateId"`
	SourceCommit  string            `json:"sourceCommit"`
	Environment   string            `json:"environment"`
	EnvironmentID string            `json:"environmentId"`
	Artifacts     map[string]string `json:"artifacts"`
	WebBaseURL    string            `json:"webBaseUrl"`
	AdminBaseURL  string            `json:"adminBaseUrl"`
}

type receiptMatrix struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type receiptWindow struct {
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

type receiptAccount struct {
	Role                 string    `json:"role"`
	SubjectReference     string    `json:"subjectReference"`
	AuthenticationMethod string    `json:"authenticationMethod"`
	SessionFreshAt       time.Time `json:"sessionFreshAt"`
}

type receiptOperation struct {
	ID                       string           `json:"id"`
	Surface                  string           `json:"surface"`
	Actor                    string           `json:"actor"`
	NegativeActor            string           `json:"negativeActor"`
	Access                   string           `json:"access"`
	ActorSubjectReference    string           `json:"actorSubjectReference"`
	Status                   string           `json:"status"`
	StartedAt                time.Time        `json:"startedAt"`
	CompletedAt              time.Time        `json:"completedAt"`
	RequestIDs               []string         `json:"requestIds"`
	NegativeSubjectReference *string          `json:"negativeSubjectReference"`
	NegativeResult           string           `json:"negativeResult"`
	UsedCLI                  bool             `json:"usedCli"`
	UsedDatabaseClient       bool             `json:"usedDatabaseClient"`
	UsedDeveloperTools       bool             `json:"usedDeveloperTools"`
	PositiveEvidence         receiptReference `json:"positiveEvidence"`
	NegativeEvidence         receiptReference `json:"negativeEvidence"`
}

type receiptSupport struct {
	RequesterSubjectReference string           `json:"requesterSubjectReference"`
	ApproverSubjectReference  string           `json:"approverSubjectReference"`
	SupportSubjectReference   string           `json:"supportSubjectReference"`
	FourEyesProved            bool             `json:"fourEyesProved"`
	ReadOnlyWriteDenied       bool             `json:"readOnlyWriteDenied"`
	RevocationAuditAction     string           `json:"revocationAuditAction"`
	ExpiryAuditAction         string           `json:"expiryAuditAction"`
	RevocationAuditVisible    bool             `json:"revocationAuditVisible"`
	ExpiryAuditVisible        bool             `json:"expiryAuditVisible"`
	TenantAuditVisible        bool             `json:"tenantAuditVisible"`
	Evidence                  receiptReference `json:"evidence"`
}

type receiptApproval struct {
	Role             string           `json:"role"`
	SubjectReference string           `json:"subjectReference"`
	Approved         bool             `json:"approved"`
	ApprovedAt       time.Time        `json:"approvedAt"`
	Evidence         receiptReference `json:"evidence"`
}

type operationsReceipt struct {
	SchemaVersion                    string             `json:"schemaVersion"`
	Candidate                        receiptCandidate   `json:"candidate"`
	Matrix                           receiptMatrix      `json:"matrix"`
	Window                           receiptWindow      `json:"window"`
	Accounts                         []receiptAccount   `json:"accounts"`
	Operations                       []receiptOperation `json:"operations"`
	OperationCounts                  map[string]int64   `json:"operationCounts"`
	NegativeAuthorizationCounts      map[string]int64   `json:"negativeAuthorizationCounts"`
	FallbackCounts                   map[string]int64   `json:"fallbackCounts"`
	SupportAccess                    receiptSupport     `json:"supportAccess"`
	Approvals                        []receiptApproval  `json:"approvals"`
	EvidenceFileCount                int64              `json:"evidenceFileCount"`
	EnvironmentEligible              bool               `json:"environmentEligible"`
	ProductionAuthenticationDeclared bool               `json:"productionAuthenticationDeclared"`
	SupportLifecycleComplete         bool               `json:"supportLifecycleComplete"`
	ApprovalsComplete                bool               `json:"approvalsComplete"`
	EligibleForHumanGateReview       bool               `json:"eligibleForHumanGateReview"`
	Assessment                       string             `json:"assessment"`
	ValidatedAt                      time.Time          `json:"validatedAt"`
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (persistence.Stage6OperationsExercise, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt must be bounded UTF-8 JSON.")
	}
	digest := sha256.Sum256(data)
	expected, err := parseDigest(input.ReceiptSHA256)
	if err != nil || !bytes.Equal(expected, digest[:]) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt SHA-256 does not match its exact bytes.")
	}
	var top map[string]json.RawMessage
	topKeys := []string{
		"schemaVersion", "candidate", "matrix", "window", "accounts", "operations", "operationCounts",
		"negativeAuthorizationCounts", "fallbackCounts", "supportAccess", "approvals", "evidenceFileCount",
		"environmentEligible", "productionAuthenticationDeclared", "supportLifecycleComplete", "approvalsComplete",
		"eligibleForHumanGateReview", "assessment", "validatedAt",
	}
	if err := json.Unmarshal(data, &top); err != nil || !exactKeys(top, topKeys) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt schema is invalid.")
	}
	if !validNestedSchema(top) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt nested schema is invalid.")
	}
	var receipt operationsReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt JSON is invalid.")
	}
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment ||
		!commitPattern.MatchString(receipt.Candidate.SourceCommit) || !receipt.Window.CompletedAt.After(receipt.Window.StartedAt) ||
		receipt.ValidatedAt.Before(receipt.Window.CompletedAt) || receipt.ValidatedAt.After(receipt.Window.CompletedAt.Add(30*24*time.Hour)) ||
		receipt.ValidatedAt.After(now.Add(5*time.Minute)) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt identity or timestamps are invalid.")
	}
	projection, err := candidateProjection(candidate)
	if err != nil || projection.OperationsReceiptSHA256 != formatDigest(digest[:]) ||
		projection.CandidateID != receipt.Candidate.CandidateID || projection.SourceCommit != receipt.Candidate.SourceCommit ||
		projection.EnvironmentID != receipt.Candidate.EnvironmentID || projection.EnvironmentClass != receipt.Candidate.Environment ||
		projection.WebOrigin != receipt.Candidate.WebBaseURL || projection.AdminOrigin != receipt.Candidate.AdminBaseURL ||
		!mapsEqual(projection.Artifacts, receipt.Candidate.Artifacts) {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt is not bound to the exact Release candidate.")
	}
	if !slices.Contains([]string{"production", "production-like"}, receipt.Candidate.Environment) ||
		!identifierPattern.MatchString(receipt.Candidate.EnvironmentID) || !validIndependentOrigins(receipt.Candidate.WebBaseURL, receipt.Candidate.AdminBaseURL) ||
		!validDigest(receipt.Matrix.SHA256) || receipt.Matrix.Path != "docs/release-matrices/stage-6-operations-ui-v1.json" {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise environment, origins or matrix are invalid.")
	}
	accountsComplete, roleSubjects := validAccounts(receipt.Accounts, receipt.Window.StartedAt)
	operationsComplete := validOperations(receipt.Operations, receipt.Window, roleSubjects)
	evidenceCount, evidenceComplete := validEvidenceCardinality(receipt)
	countsComplete := exactCountMap(receipt.OperationCounts, map[string]int64{"blocked": 0, "failed": 0, "passed": requiredOperationCount}) &&
		exactCountMap(receipt.NegativeAuthorizationCounts, map[string]int64{"denied": requiredOperationCount, "not-run": 0, "unexpectedly-allowed": 0}) &&
		exactCountMap(receipt.FallbackCounts, map[string]int64{"cli": 0, "databaseClient": 0, "developerTools": 0})
	supportComplete := validSupport(receipt.SupportAccess, roleSubjects)
	receiptApprovalsComplete := validReceiptApprovals(receipt.Approvals, receipt.Window.CompletedAt)
	eligible := accountsComplete && operationsComplete && countsComplete && supportComplete && receiptApprovalsComplete && evidenceComplete &&
		receipt.EvidenceFileCount == evidenceCount && receipt.EnvironmentEligible && receipt.ProductionAuthenticationDeclared &&
		receipt.SupportLifecycleComplete && receipt.ApprovalsComplete
	if !eligible || !receipt.EligibleForHumanGateReview {
		return persistence.Stage6OperationsExercise{}, invalidReceipt("Operations exercise receipt projections or evidence are incomplete.")
	}
	matrixDigest, _ := parseDigest(receipt.Matrix.SHA256)
	return persistence.Stage6OperationsExercise{
		Receipt: data, ReceiptSHA256: digest[:], ReceiptSizeBytes: int64(len(data)), Schema: receipt.SchemaVersion,
		Assessment: receipt.Assessment, ReleaseCommit: receipt.Candidate.SourceCommit,
		EnvironmentClass: receipt.Candidate.Environment, EnvironmentID: receipt.Candidate.EnvironmentID,
		MatrixSHA256: matrixDigest, WebOrigin: receipt.Candidate.WebBaseURL, AdminOrigin: receipt.Candidate.AdminBaseURL,
		StartedAt: receipt.Window.StartedAt.UTC(), CompletedAt: receipt.Window.CompletedAt.UTC(), ValidatedAt: receipt.ValidatedAt.UTC(),
		AccountCount: int64(len(receipt.Accounts)), OperationCount: int64(len(receipt.Operations)), MatrixProfile: requiredMatrixProfile, EvidenceFileCount: receipt.EvidenceFileCount,
		AllOperationsPassed: true, AllNegativeAuthorizationsDenied: true, NoDeveloperFallbacks: true,
		ProductionAuthenticationDeclared: true, SupportLifecycleComplete: true, ReceiptApprovalsComplete: true,
		ReleaseEligibleEnvironment: true, EligibleForHumanGateReview: true,
		CryptographicSignaturesVerified: false, ExternalAuthorityVerificationRequired: true,
		State: "recorded", Version: 1,
	}, nil
}

type projectedCandidate struct {
	CandidateID, SourceCommit, EnvironmentClass, EnvironmentID, OperationsReceiptSHA256 string
	WebOrigin, AdminOrigin                                                              string
	Artifacts                                                                           map[string]string
}

func candidateProjection(candidate persistence.Stage6ReleaseCandidate) (projectedCandidate, error) {
	var receipt struct {
		Candidate struct {
			CandidateID      string `json:"candidateId"`
			SourceCommit     string `json:"sourceCommit"`
			EnvironmentClass string `json:"environmentClass"`
			EnvironmentID    string `json:"environmentId"`
			Artifacts        struct {
				ControlPlaneImage string `json:"controlPlaneImage"`
				WebArtifact       string `json:"webArtifact"`
				AdminArtifact     string `json:"adminArtifact"`
			} `json:"artifacts"`
			Origins struct {
				WebBaseURL   string `json:"webBaseUrl"`
				AdminBaseURL string `json:"adminBaseUrl"`
			} `json:"origins"`
		} `json:"candidate"`
		Receipts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		return projectedCandidate{}, err
	}
	operations, ok := receipt.Receipts["operations"]
	if !ok || operations.SHA256 == "" || receipt.Candidate.CandidateID != candidate.CandidateID ||
		receipt.Candidate.SourceCommit != candidate.SourceCommit || receipt.Candidate.EnvironmentID != candidate.EnvironmentID {
		return projectedCandidate{}, errors.New("candidate Operations exercise receipt reference is missing")
	}
	return projectedCandidate{
		CandidateID: receipt.Candidate.CandidateID, SourceCommit: receipt.Candidate.SourceCommit,
		EnvironmentClass: receipt.Candidate.EnvironmentClass, EnvironmentID: receipt.Candidate.EnvironmentID,
		OperationsReceiptSHA256: operations.SHA256, WebOrigin: receipt.Candidate.Origins.WebBaseURL,
		AdminOrigin: receipt.Candidate.Origins.AdminBaseURL,
		Artifacts: map[string]string{
			"controlPlane": receipt.Candidate.Artifacts.ControlPlaneImage,
			"web":          receipt.Candidate.Artifacts.WebArtifact,
			"admin":        receipt.Candidate.Artifacts.AdminArtifact,
		},
	}, nil
}

func validAccounts(values []receiptAccount, started time.Time) (bool, map[string]string) {
	if len(values) != requiredAccountCount {
		return false, nil
	}
	roles, subjects := map[string]struct{}{}, map[string]struct{}{}
	roleSubjects := map[string]string{}
	for _, value := range values {
		if !slices.Contains(accountRoles, value.Role) || value.AuthenticationMethod == "fixture" ||
			!slices.Contains([]string{"sso", "password-mfa"}, value.AuthenticationMethod) ||
			!identifierPattern.MatchString(value.SubjectReference) || value.SessionFreshAt.After(started) {
			return false, nil
		}
		if _, exists := roles[value.Role]; exists {
			return false, nil
		}
		if _, exists := subjects[value.SubjectReference]; exists {
			return false, nil
		}
		roles[value.Role] = struct{}{}
		subjects[value.SubjectReference] = struct{}{}
		roleSubjects[value.Role] = value.SubjectReference
	}
	return len(roles) == requiredAccountCount, roleSubjects
}

func validOperations(values []receiptOperation, window receiptWindow, roleSubjects map[string]string) bool {
	if len(values) != requiredOperationCount || len(expectedOperations) != requiredOperationCount {
		return false
	}
	ids, requestIDs := map[string]struct{}{}, map[string]struct{}{}
	for _, value := range values {
		policy, expected := expectedOperations[value.ID]
		if !expected || value.Surface != policy.Surface || value.Actor != policy.Actor || value.NegativeActor != policy.NegativeActor || value.Access != policy.Access ||
			value.Status != "passed" || value.NegativeResult != "denied" ||
			value.UsedCLI || value.UsedDatabaseClient || value.UsedDeveloperTools || len(value.RequestIDs) == 0 ||
			value.StartedAt.Before(window.StartedAt) || !value.CompletedAt.After(value.StartedAt) || value.CompletedAt.After(window.CompletedAt) ||
			!validReference(value.PositiveEvidence) || !validReference(value.NegativeEvidence) ||
			!slices.Contains([]string{"tenant-web", "platform-admin"}, value.Surface) ||
			!slices.Contains([]string{"read", "write", "read-only", "four-eyes"}, value.Access) ||
			roleSubjects[value.Actor] == "" || roleSubjects[value.Actor] != value.ActorSubjectReference {
			return false
		}
		if value.NegativeActor == "unauthenticated" {
			if value.NegativeSubjectReference != nil {
				return false
			}
		} else if roleSubjects[value.NegativeActor] == "" || value.NegativeSubjectReference == nil ||
			*value.NegativeSubjectReference != roleSubjects[value.NegativeActor] {
			return false
		}
		if _, exists := ids[value.ID]; exists {
			return false
		}
		ids[value.ID] = struct{}{}
		for _, requestID := range value.RequestIDs {
			parsed, err := uuid.Parse(requestID)
			if err != nil || parsed.String() != requestID {
				return false
			}
			if _, exists := requestIDs[requestID]; exists {
				return false
			}
			requestIDs[requestID] = struct{}{}
		}
	}
	return len(ids) == len(expectedOperations)
}

func validEvidenceCardinality(receipt operationsReceipt) (int64, bool) {
	if len(receipt.Approvals) != 2 {
		return 0, false
	}
	paths := map[string]struct{}{}
	for _, operation := range receipt.Operations {
		for _, reference := range []receiptReference{operation.PositiveEvidence, operation.NegativeEvidence} {
			if _, duplicate := paths[reference.Path]; duplicate {
				return 0, false
			}
			paths[reference.Path] = struct{}{}
		}
	}
	for _, reference := range []receiptReference{receipt.SupportAccess.Evidence, receipt.Approvals[0].Evidence, receipt.Approvals[1].Evidence} {
		if _, duplicate := paths[reference.Path]; duplicate {
			return 0, false
		}
		paths[reference.Path] = struct{}{}
	}
	return int64(len(paths)), len(paths) == requiredOperationCount*2+3
}

func validNestedSchema(top map[string]json.RawMessage) bool {
	if !rawObjectKeys(top["candidate"], []string{"candidateId", "sourceCommit", "environment", "environmentId", "artifacts", "webBaseUrl", "adminBaseUrl"}) ||
		!rawNestedObjectKeys(top["candidate"], "artifacts", []string{"controlPlane", "web", "admin"}) ||
		!rawObjectKeys(top["matrix"], []string{"path", "sha256"}) ||
		!rawObjectKeys(top["window"], []string{"startedAt", "completedAt"}) ||
		!rawObjectKeys(top["operationCounts"], []string{"blocked", "failed", "passed"}) ||
		!rawObjectKeys(top["negativeAuthorizationCounts"], []string{"denied", "not-run", "unexpectedly-allowed"}) ||
		!rawObjectKeys(top["fallbackCounts"], []string{"cli", "databaseClient", "developerTools"}) ||
		!rawObjectKeys(top["supportAccess"], []string{"requesterSubjectReference", "approverSubjectReference", "supportSubjectReference", "fourEyesProved", "readOnlyWriteDenied", "revocationAuditAction", "expiryAuditAction", "revocationAuditVisible", "expiryAuditVisible", "tenantAuditVisible", "evidence"}) ||
		!rawNestedObjectKeys(top["supportAccess"], "evidence", []string{"path", "sha256"}) {
		return false
	}
	var accounts []map[string]json.RawMessage
	if json.Unmarshal(top["accounts"], &accounts) != nil || len(accounts) != requiredAccountCount {
		return false
	}
	for _, account := range accounts {
		if !exactKeys(account, []string{"role", "subjectReference", "authenticationMethod", "sessionFreshAt"}) {
			return false
		}
	}
	var operations []map[string]json.RawMessage
	if json.Unmarshal(top["operations"], &operations) != nil || len(operations) != requiredOperationCount {
		return false
	}
	operationKeys := []string{
		"id", "surface", "actor", "negativeActor", "access", "actorSubjectReference", "status", "startedAt",
		"completedAt", "requestIds", "negativeSubjectReference", "negativeResult", "usedCli", "usedDatabaseClient",
		"usedDeveloperTools", "positiveEvidence", "negativeEvidence",
	}
	for _, operation := range operations {
		if !exactKeys(operation, operationKeys) || !rawObjectKeys(operation["positiveEvidence"], []string{"path", "sha256"}) ||
			!rawObjectKeys(operation["negativeEvidence"], []string{"path", "sha256"}) {
			return false
		}
	}
	var approvals []map[string]json.RawMessage
	if json.Unmarshal(top["approvals"], &approvals) != nil || len(approvals) != 2 {
		return false
	}
	for _, approval := range approvals {
		if !exactKeys(approval, []string{"role", "subjectReference", "approved", "approvedAt", "evidence"}) ||
			!rawObjectKeys(approval["evidence"], []string{"path", "sha256"}) {
			return false
		}
	}
	return true
}

func rawObjectKeys(raw json.RawMessage, keys []string) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && exactKeys(value, keys)
}

func rawNestedObjectKeys(raw json.RawMessage, key string, keys []string) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && rawObjectKeys(value[key], keys)
}

func validSupport(value receiptSupport, roleSubjects map[string]string) bool {
	return value.RequesterSubjectReference == roleSubjects["platform-operator"] &&
		value.ApproverSubjectReference == roleSubjects["platform-admin"] &&
		value.SupportSubjectReference == roleSubjects["support-engineer"] &&
		value.RequesterSubjectReference != value.ApproverSubjectReference && value.FourEyesProved && value.ReadOnlyWriteDenied &&
		value.RevocationAuditAction == "support.access_revoked" && value.ExpiryAuditAction == "support.access_expired" &&
		value.RevocationAuditVisible && value.ExpiryAuditVisible && value.TenantAuditVisible && validReference(value.Evidence)
}

func validReceiptApprovals(values []receiptApproval, completed time.Time) bool {
	if len(values) != 2 {
		return false
	}
	roles, subjects, paths := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for _, value := range values {
		if !slices.Contains(approvalRoles, value.Role) || !identifierPattern.MatchString(value.SubjectReference) ||
			!value.Approved || value.ApprovedAt.Before(completed) || !validReference(value.Evidence) {
			return false
		}
		if _, exists := roles[value.Role]; exists {
			return false
		}
		if _, exists := subjects[value.SubjectReference]; exists {
			return false
		}
		if _, exists := paths[value.Evidence.Path]; exists {
			return false
		}
		roles[value.Role], subjects[value.SubjectReference], paths[value.Evidence.Path] = struct{}{}, struct{}{}, struct{}{}
	}
	return true
}

func exactCountMap(actual, expected map[string]int64) bool {
	if len(actual) != len(expected) {
		return false
	}
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func validIndependentOrigins(webRaw, adminRaw string) bool {
	web, webOK := parseHTTPSOrigin(webRaw)
	admin, adminOK := parseHTTPSOrigin(adminRaw)
	return webOK && adminOK && !strings.EqualFold(web.Host, admin.Host)
}

func parseHTTPSOrigin(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	return parsed, err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		(parsed.Path == "" || parsed.Path == "/") && parsed.RawQuery == "" && parsed.Fragment == "" && len(raw) <= 512
}

func validReference(value receiptReference) bool {
	return value.Path != "" && !strings.Contains(value.Path, "..") && !strings.HasPrefix(value.Path, "/") && validDigest(value.SHA256)
}

func formatDigest(value []byte) string { return "sha256:" + hex.EncodeToString(value) }

func parseDigest(raw string) ([]byte, error) {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	if len(value) != 64 {
		return nil, errors.New("invalid digest")
	}
	return hex.DecodeString(value)
}

func validDigest(raw string) bool { _, err := parseDigest(raw); return err == nil }

func validHTTPSReference(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && len(raw) <= 2048
}

func invalidReceipt(detail string) error {
	return problem.New(400, "operations_exercise_receipt_invalid", detail)
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

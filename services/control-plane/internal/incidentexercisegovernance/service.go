package incidentexercisegovernance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
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
	receiptSchema     = "synara.incident-communication-exercise-evidence-receipt.v2"
	receiptAssessment = "evidence-validated-not-operations-ready"
	receiptMaxBytes   = 512 * 1024
)

var (
	approvalRoles  = []string{"operations", "communications"}
	evidenceFields = []string{
		"onCallRotaEvidence", "pagingProviderEvidence", "internalStatusBoardTimelineEvidence",
		"employeeNotificationDeliveryEvidence", "internalProbeEvidence", "internalUserPathEvidence",
		"exerciseReviewEvidence",
	}
	componentNames = []string{
		"control-plane-api", "authentication-sso", "execution-scheduling",
		"worker-runtime", "artifact-service", "web-application",
	}
	roleNames = []string{
		"incidentCommander", "operationsLead", "communicationsLead", "scribe",
		"securityPrivacyLead", "releaseObserver",
	}
	identifierPattern             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$`)
	commitPattern                 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
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
	EvidenceSHA256    *string    `json:"evidenceSha256"`
	SupersededAt      *time.Time `json:"supersededAt"`
	SupersededReason  *string    `json:"supersededReason"`
	CreatedAt         time.Time  `json:"createdAt"`
}

type Exercise struct {
	ID                                      uuid.UUID  `json:"id"`
	CandidateRecordID                       uuid.UUID  `json:"candidateRecordId"`
	CandidateID                             string     `json:"candidateId"`
	ExerciseID                              uuid.UUID  `json:"exerciseId"`
	ReceiptSHA256                           string     `json:"receiptSha256"`
	ReceiptSizeBytes                        int64      `json:"receiptSizeBytes"`
	ReleaseCommit                           string     `json:"releaseCommit"`
	EnvironmentClass                        string     `json:"environmentClass"`
	EnvironmentID                           string     `json:"environmentId"`
	ExerciseMode                            string     `json:"exerciseMode"`
	Severity                                string     `json:"severity"`
	ServiceOrigin                           string     `json:"serviceOrigin"`
	InternalStatusBoardOrigin               string     `json:"internalStatusBoardOrigin"`
	StartedAt                               time.Time  `json:"startedAt"`
	CompletedAt                             time.Time  `json:"completedAt"`
	ValidatedAt                             time.Time  `json:"validatedAt"`
	IndependentInternalStatusBoardDeclared  bool       `json:"independentInternalStatusBoardDeclared"`
	RoleSeparationComplete                  bool       `json:"roleSeparationComplete"`
	PagingExerciseComplete                  bool       `json:"pagingExerciseComplete"`
	InternalStatusBoardComponentsComplete   bool       `json:"internalStatusBoardComponentsComplete"`
	InternalTimelineWithinTargets           bool       `json:"internalTimelineWithinTargets"`
	EmployeeNotificationDeliveryComplete    bool       `json:"employeeNotificationDeliveryComplete"`
	RecoveryVerificationComplete            bool       `json:"recoveryVerificationComplete"`
	ReviewComplete                          bool       `json:"reviewComplete"`
	ReleaseEligibleEnvironment              bool       `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview              bool       `json:"eligibleForHumanGateReview"`
	CryptographicSignaturesVerified         bool       `json:"cryptographicSignaturesVerified"`
	DeploymentAuthorityVerificationRequired bool       `json:"deploymentAuthorityVerificationRequired"`
	State                                   string     `json:"state"`
	Version                                 int64      `json:"version"`
	CreatedBy                               uuid.UUID  `json:"createdBy"`
	Approvals                               []Approval `json:"approvals"`
	ApprovedAt                              *time.Time `json:"approvedAt"`
	RejectedAt                              *time.Time `json:"rejectedAt"`
	CreatedAt                               time.Time  `json:"createdAt"`
	UpdatedAt                               time.Time  `json:"updatedAt"`
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
	var models []persistence.Stage6IncidentExercise
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "incident_exercises_load_failed", "Incident exercises could not be loaded.", err)
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
		return Exercise{}, problem.New(404, "incident_exercise_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Exercise{}, problem.Wrap(500, "incident_exercise_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Exercise{}, problem.New(403, "incident_exercise_import_forbidden", "The release candidate creator must import its exact Incident exercise receipt.")
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
			return problem.New(409, "incident_exercise_exists", "This exact Incident exercise already exists.")
		} else if err != nil {
			return problem.Wrap(500, "incident_exercise_import_failed", "The Incident exercise could not be imported.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.exercise_imported", ResourceType: "stage6_incident_exercise", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"candidateId": candidate.CandidateID, "exerciseId": model.ExerciseID,
				"receiptSha256":              input.ReceiptSHA256,
				"eligibleForHumanGateReview": model.EligibleForHumanGateReview,
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
		return Exercise{}, problem.New(400, "incident_exercise_approval_invalid", "Incident exercise approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6IncidentExercise
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", exerciseID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "incident_exercise_not_found", "The Incident exercise was not found.")
		} else if err != nil {
			return problem.Wrap(500, "incident_exercise_load_failed", "The Incident exercise could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "incident_exercise_not_reviewable", "The Incident exercise is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "incident_exercise_ineligible", "An ineligible or failed Incident exercise cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.IncidentExercisePrefix+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6IncidentExerciseApproval{
			ID: uuid.New(), IncidentExerciseID: model.ID, OperatorTenantID: s.operatorTenantID,
			Role: input.Role, Decision: input.Decision, ApproverUserID: actorID,
			Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now(),
		}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "incident_exercise_approval_conflict", "This role or operator already recorded an Incident exercise decision.")
		} else if err != nil {
			return problem.Wrap(500, "incident_exercise_approval_create_failed", "The Incident exercise decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.exercise_approval_recorded", ResourceType: "stage6_incident_exercise", ResourceID: &model.ID,
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
			if err := tx.Model(&persistence.Stage6IncidentExerciseApproval{}).Where("incident_exercise_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", model.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Count(&count).Error; err != nil {
				return problem.Wrap(500, "incident_exercise_approvals_load_failed", "Incident exercise decisions could not be verified.", err)
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
		result := tx.Model(&persistence.Stage6IncidentExercise{}).Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "incident_exercise_transition_failed", "The Incident exercise decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "incident_exercise_version_conflict", "The Incident exercise changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.exercise_" + target, ResourceType: "stage6_incident_exercise", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Exercise{}, err
	}
	return s.get(ctx, exerciseID)
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Exercise, error) {
	var model persistence.Stage6IncidentExercise
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Exercise{}, problem.New(404, "incident_exercise_not_found", "The Incident exercise was not found.")
	} else if err != nil {
		return Exercise{}, problem.Wrap(500, "incident_exercise_load_failed", "The Incident exercise could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6IncidentExercise) (Exercise, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Exercise{}, problem.Wrap(500, "incident_exercise_candidate_load_failed", "The linked release candidate could not be loaded.", err)
	}
	type approvalRow struct {
		persistence.Stage6IncidentExerciseApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_incident_exercise_approvals approval").
		Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").
		Joins("JOIN users approver ON approver.id = approval.approver_user_id").
		Where("approval.incident_exercise_id = ?", model.ID).Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
		return Exercise{}, problem.Wrap(500, "incident_exercise_approvals_load_failed", "Incident exercise decisions could not be loaded.", err)
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
		ExerciseID: model.ExerciseID, ReceiptSHA256: formatDigest(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes,
		ReleaseCommit: model.ReleaseCommit, EnvironmentClass: model.EnvironmentClass, EnvironmentID: model.EnvironmentID,
		ExerciseMode: model.ExerciseMode, Severity: model.Severity, ServiceOrigin: model.ServiceOrigin,
		InternalStatusBoardOrigin: model.InternalStatusBoardOrigin, StartedAt: model.StartedAt, CompletedAt: model.CompletedAt,
		ValidatedAt: model.ValidatedAt, IndependentInternalStatusBoardDeclared: model.IndependentInternalStatusBoardDeclared,
		RoleSeparationComplete: model.RoleSeparationComplete, PagingExerciseComplete: model.PagingExerciseComplete,
		InternalStatusBoardComponentsComplete: model.InternalStatusBoardComponentsComplete,
		InternalTimelineWithinTargets:         model.InternalTimelineWithinTargets,
		EmployeeNotificationDeliveryComplete:  model.EmployeeNotificationDeliveryComplete,
		RecoveryVerificationComplete:          model.RecoveryVerificationComplete, ReviewComplete: model.ReviewComplete,
		ReleaseEligibleEnvironment:              model.ReleaseEligibleEnvironment,
		EligibleForHumanGateReview:              model.EligibleForHumanGateReview,
		CryptographicSignaturesVerified:         model.CryptographicSignaturesVerified,
		DeploymentAuthorityVerificationRequired: model.DeploymentAuthorityVerificationRequired,
		State:                                   model.State, Version: model.Version, CreatedBy: model.CreatedBy, Approvals: approvals,
		ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

type receiptReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type receiptPaging struct {
	ProviderReference      string    `json:"providerReference"`
	PrimaryResponder       string    `json:"primaryResponder"`
	SecondaryResponder     string    `json:"secondaryResponder"`
	PageTriggeredAt        time.Time `json:"pageTriggeredAt"`
	AcknowledgedAt         time.Time `json:"acknowledgedAt"`
	AcknowledgedBy         string    `json:"acknowledgedBy"`
	AcknowledgementSeconds float64   `json:"acknowledgementSeconds"`
	TargetSeconds          int64     `json:"targetSeconds"`
	WithinTarget           bool      `json:"withinTarget"`
	EscalationExercised    bool      `json:"escalationExercised"`
	PagingSucceeded        bool      `json:"pagingSucceeded"`
}

type receiptComponent struct {
	Published             bool `json:"published"`
	RegionDetailPublished bool `json:"regionDetailPublished"`
}

type receiptUpdate struct {
	Kind        string    `json:"kind"`
	PublishedAt time.Time `json:"publishedAt"`
}

type receiptTimeline struct {
	ImpactConfirmedAt                time.Time       `json:"impactConfirmedAt"`
	IncidentOpenedAt                 time.Time       `json:"incidentOpenedAt"`
	Updates                          []receiptUpdate `json:"updates"`
	FirstInternalUpdateSeconds       float64         `json:"firstInternalUpdateSeconds"`
	FirstInternalUpdateTargetSeconds int64           `json:"firstInternalUpdateTargetSeconds"`
	FirstInternalUpdateWithinTarget  bool            `json:"firstInternalUpdateWithinTarget"`
	UpdateCadenceTargetSeconds       int64           `json:"updateCadenceTargetSeconds"`
	UpdateCadenceWithinTarget        bool            `json:"updateCadenceWithinTarget"`
	InternalHistoryVisible           bool            `json:"internalHistoryVisible"`
}

type receiptDelivery struct {
	ChannelReference                 string    `json:"channelReference"`
	InitialNotificationReceivedAt    time.Time `json:"initialNotificationReceivedAt"`
	InitialDeliverySeconds           float64   `json:"initialDeliverySeconds"`
	ResolutionNotificationReceivedAt time.Time `json:"resolutionNotificationReceivedAt"`
	ResolutionDeliverySeconds        float64   `json:"resolutionDeliverySeconds"`
	DeliverySucceeded                bool      `json:"deliverySucceeded"`
}

type receiptRecovery struct {
	RecoveredAt                        time.Time `json:"recoveredAt"`
	ObservationSecondsBeforeResolution float64   `json:"observationSecondsBeforeResolution"`
	MinimumObservationSeconds          int64     `json:"minimumObservationSeconds"`
	InternalProbePassed                bool      `json:"internalProbePassed"`
	InternalUserPathPassed             bool      `json:"internalUserPathPassed"`
	CleanupCompleted                   bool      `json:"cleanupCompleted"`
}

type incidentReceipt struct {
	SchemaVersion                          string                      `json:"schemaVersion"`
	ExerciseID                             uuid.UUID                   `json:"exerciseId"`
	ReleaseCommit                          string                      `json:"releaseCommit"`
	EnvironmentClass                       string                      `json:"environmentClass"`
	EnvironmentID                          string                      `json:"environmentId"`
	ExerciseMode                           string                      `json:"exerciseMode"`
	Severity                               string                      `json:"severity"`
	ServiceOrigin                          string                      `json:"serviceOrigin"`
	InternalStatusBoardOrigin              string                      `json:"internalStatusBoardOrigin"`
	IndependentInternalStatusBoardDeclared bool                        `json:"independentInternalStatusBoardDeclared"`
	RegionPromiseEnabled                   bool                        `json:"regionPromiseEnabled"`
	StartedAt                              time.Time                   `json:"startedAt"`
	CompletedAt                            time.Time                   `json:"completedAt"`
	Manifest                               receiptReference            `json:"manifest"`
	Roles                                  map[string]string           `json:"roles"`
	RoleSeparationComplete                 bool                        `json:"roleSeparationComplete"`
	Paging                                 receiptPaging               `json:"paging"`
	PagingExerciseComplete                 bool                        `json:"pagingExerciseComplete"`
	InternalStatusBoardComponents          map[string]receiptComponent `json:"internalStatusBoardComponents"`
	InternalStatusBoardComponentsComplete  bool                        `json:"internalStatusBoardComponentsComplete"`
	InternalTimeline                       receiptTimeline             `json:"internalTimeline"`
	InternalTimelineWithinTargets          bool                        `json:"internalTimelineWithinTargets"`
	EmployeeNotificationDelivery           receiptDelivery             `json:"employeeNotificationDelivery"`
	EmployeeNotificationDeliveryComplete   bool                        `json:"employeeNotificationDeliveryComplete"`
	RecoveryVerification                   receiptRecovery             `json:"recoveryVerification"`
	RecoveryVerificationComplete           bool                        `json:"recoveryVerificationComplete"`
	Review                                 map[string]bool             `json:"review"`
	ReviewComplete                         bool                        `json:"reviewComplete"`
	Evidence                               map[string]receiptReference `json:"evidence"`
	ReleaseEligibleEnvironment             bool                        `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview             bool                        `json:"eligibleForHumanGateReview"`
	ValidatedAt                            time.Time                   `json:"validatedAt"`
	Assessment                             string                      `json:"assessment"`
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (persistence.Stage6IncidentExercise, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt must be bounded UTF-8 JSON.")
	}
	digest := sha256.Sum256(data)
	expected, err := parseDigest(input.ReceiptSHA256)
	if err != nil || !bytes.Equal(expected, digest[:]) {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt SHA-256 does not match its exact bytes.")
	}
	var top map[string]json.RawMessage
	topKeys := []string{
		"schemaVersion", "exerciseId", "releaseCommit", "environmentClass", "environmentId", "exerciseMode", "severity",
		"serviceOrigin", "internalStatusBoardOrigin", "independentInternalStatusBoardDeclared", "regionPromiseEnabled", "startedAt", "completedAt",
		"manifest", "roles", "roleSeparationComplete", "paging", "pagingExerciseComplete", "internalStatusBoardComponents",
		"internalStatusBoardComponentsComplete", "internalTimeline", "internalTimelineWithinTargets", "employeeNotificationDelivery",
		"employeeNotificationDeliveryComplete", "recoveryVerification", "recoveryVerificationComplete", "review", "reviewComplete",
		"evidence", "releaseEligibleEnvironment", "eligibleForHumanGateReview", "validatedAt", "assessment",
	}
	if err := json.Unmarshal(data, &top); err != nil || !exactKeys(top, topKeys) {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt schema is invalid.")
	}
	var receipt incidentReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt JSON is invalid.")
	}
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment || receipt.ExerciseID == uuid.Nil ||
		!commitPattern.MatchString(receipt.ReleaseCommit) || !receipt.CompletedAt.After(receipt.StartedAt) ||
		receipt.ValidatedAt.Before(receipt.CompletedAt) || receipt.ValidatedAt.After(receipt.CompletedAt.Add(30*24*time.Hour)) ||
		receipt.ValidatedAt.After(now.Add(5*time.Minute)) {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt identity or timestamps are invalid.")
	}
	projection, err := candidateProjection(candidate)
	if err != nil || projection.IncidentReceiptSHA256 != formatDigest(digest[:]) || projection.SourceCommit != receipt.ReleaseCommit ||
		projection.EnvironmentID != receipt.EnvironmentID || projection.EnvironmentClass != receipt.EnvironmentClass {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt is not bound to the exact Release candidate.")
	}
	if !slices.Contains([]string{"production", "production-like"}, receipt.EnvironmentClass) ||
		receipt.ExerciseMode != "live-internal-communication" || !slices.Contains([]string{"SEV-0", "SEV-1", "SEV-2"}, receipt.Severity) ||
		!identifierPattern.MatchString(receipt.EnvironmentID) {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise is not release eligible.")
	}
	independent := validIndependentOrigins(receipt.ServiceOrigin, receipt.InternalStatusBoardOrigin)
	roleSeparation := validRoleSeparation(receipt.Roles)
	pagingComplete := validPaging(receipt.Paging, receipt.Severity, receipt.StartedAt, receipt.CompletedAt)
	componentsComplete := validComponents(receipt.InternalStatusBoardComponents, receipt.RegionPromiseEnabled)
	timelineComplete, firstUpdate, resolved := validTimeline(receipt.InternalTimeline, receipt.Severity, receipt.StartedAt, receipt.CompletedAt)
	deliveryComplete := validDelivery(receipt.EmployeeNotificationDelivery, firstUpdate, resolved, receipt.CompletedAt)
	recoveryComplete := validRecovery(receipt.RecoveryVerification, receipt.StartedAt, resolved)
	reviewComplete := validReview(receipt.Review)
	evidenceComplete := validEvidenceSet(receipt.Manifest, receipt.Evidence)
	eligible := independent && roleSeparation && pagingComplete && componentsComplete && timelineComplete && deliveryComplete &&
		recoveryComplete && reviewComplete && evidenceComplete
	if receipt.IndependentInternalStatusBoardDeclared != independent || receipt.RoleSeparationComplete != roleSeparation ||
		receipt.PagingExerciseComplete != pagingComplete || receipt.InternalStatusBoardComponentsComplete != componentsComplete ||
		receipt.InternalTimelineWithinTargets != timelineComplete || receipt.EmployeeNotificationDeliveryComplete != deliveryComplete ||
		receipt.RecoveryVerificationComplete != recoveryComplete || receipt.ReviewComplete != reviewComplete ||
		!receipt.ReleaseEligibleEnvironment || receipt.EligibleForHumanGateReview != eligible || !eligible {
		return persistence.Stage6IncidentExercise{}, invalidReceipt("Incident exercise receipt projections or evidence are incomplete.")
	}
	return persistence.Stage6IncidentExercise{
		ExerciseID: receipt.ExerciseID, Receipt: data, ReceiptSHA256: digest[:], ReceiptSizeBytes: int64(len(data)),
		Schema: receipt.SchemaVersion, Assessment: receipt.Assessment, ReleaseCommit: receipt.ReleaseCommit,
		EnvironmentClass: receipt.EnvironmentClass, EnvironmentID: receipt.EnvironmentID, ExerciseMode: receipt.ExerciseMode,
		Severity: receipt.Severity, ServiceOrigin: receipt.ServiceOrigin, InternalStatusBoardOrigin: receipt.InternalStatusBoardOrigin,
		StartedAt: receipt.StartedAt.UTC(), CompletedAt: receipt.CompletedAt.UTC(), ValidatedAt: receipt.ValidatedAt.UTC(),
		IndependentInternalStatusBoardDeclared: independent, RoleSeparationComplete: roleSeparation,
		PagingExerciseComplete: pagingComplete, InternalStatusBoardComponentsComplete: componentsComplete,
		InternalTimelineWithinTargets: timelineComplete, EmployeeNotificationDeliveryComplete: deliveryComplete,
		RecoveryVerificationComplete: recoveryComplete, ReviewComplete: reviewComplete,
		ReleaseEligibleEnvironment: true, EligibleForHumanGateReview: true,
		CryptographicSignaturesVerified: false, DeploymentAuthorityVerificationRequired: true,
		State: "recorded", Version: 1,
	}, nil
}

type projectedCandidate struct {
	SourceCommit, EnvironmentClass, EnvironmentID, IncidentReceiptSHA256 string
}

func candidateProjection(candidate persistence.Stage6ReleaseCandidate) (projectedCandidate, error) {
	var receipt struct {
		Candidate struct {
			SourceCommit, EnvironmentClass, EnvironmentID string
		} `json:"candidate"`
		Receipts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		return projectedCandidate{}, err
	}
	incident, ok := receipt.Receipts["incident"]
	if !ok || incident.SHA256 == "" || receipt.Candidate.SourceCommit != candidate.SourceCommit || receipt.Candidate.EnvironmentID != candidate.EnvironmentID {
		return projectedCandidate{}, errors.New("candidate Incident exercise receipt reference is missing")
	}
	return projectedCandidate{
		SourceCommit: receipt.Candidate.SourceCommit, EnvironmentClass: receipt.Candidate.EnvironmentClass,
		EnvironmentID: receipt.Candidate.EnvironmentID, IncidentReceiptSHA256: incident.SHA256,
	}, nil
}

func validIndependentOrigins(serviceRaw, statusRaw string) bool {
	service, serviceOK := parseHTTPSOrigin(serviceRaw)
	status, statusOK := parseHTTPSOrigin(statusRaw)
	return serviceOK && statusOK && !strings.EqualFold(service.Host, status.Host)
}

func parseHTTPSOrigin(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	return parsed, err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		(parsed.Path == "" || parsed.Path == "/") && parsed.RawQuery == "" && parsed.Fragment == "" && len(raw) <= 512
}

func validRoleSeparation(roles map[string]string) bool {
	if !exactKeys(roles, roleNames) {
		return false
	}
	for _, role := range roleNames {
		if !identifierPattern.MatchString(roles[role]) {
			return false
		}
	}
	return roles["incidentCommander"] != roles["communicationsLead"] && roles["incidentCommander"] != roles["releaseObserver"]
}

func severityTargets(severity string) (ack, first, cadence int64, ok bool) {
	switch severity {
	case "SEV-0", "SEV-1":
		return 300, 900, 1800, true
	case "SEV-2":
		return 900, 1800, 3600, true
	default:
		return 0, 0, 0, false
	}
}

func validPaging(value receiptPaging, severity string, started, completed time.Time) bool {
	ackTarget, _, _, ok := severityTargets(severity)
	seconds := value.AcknowledgedAt.Sub(value.PageTriggeredAt).Seconds()
	responders := value.AcknowledgedBy == value.PrimaryResponder || value.AcknowledgedBy == value.SecondaryResponder
	return ok && identifierPattern.MatchString(value.ProviderReference) && identifierPattern.MatchString(value.PrimaryResponder) &&
		identifierPattern.MatchString(value.SecondaryResponder) && value.PrimaryResponder != value.SecondaryResponder && responders &&
		!value.PageTriggeredAt.Before(started) && !value.AcknowledgedAt.Before(value.PageTriggeredAt) && !value.AcknowledgedAt.After(completed) &&
		finite(value.AcknowledgementSeconds) && value.AcknowledgementSeconds == seconds && value.TargetSeconds == ackTarget &&
		value.WithinTarget == (value.PagingSucceeded && seconds <= float64(ackTarget)) && value.WithinTarget &&
		value.PagingSucceeded && value.EscalationExercised
}

func validComponents(values map[string]receiptComponent, regionPromise bool) bool {
	if !exactKeys(values, componentNames) {
		return false
	}
	for _, name := range componentNames {
		if !values[name].Published || (regionPromise && !values[name].RegionDetailPublished) {
			return false
		}
	}
	return true
}

func validTimeline(value receiptTimeline, severity string, started, completed time.Time) (bool, time.Time, time.Time) {
	_, firstTarget, cadenceTarget, ok := severityTargets(severity)
	if !ok || len(value.Updates) < 3 || value.ImpactConfirmedAt.Before(started) || value.IncidentOpenedAt.Before(value.ImpactConfirmedAt) || value.IncidentOpenedAt.After(completed) {
		return false, time.Time{}, time.Time{}
	}
	previous := value.IncidentOpenedAt
	progressCount := 0
	cadenceOK := true
	for index, update := range value.Updates {
		if update.PublishedAt.Before(previous) || update.PublishedAt.After(completed) {
			return false, time.Time{}, time.Time{}
		}
		if index > 0 && update.PublishedAt.Sub(previous).Seconds() > float64(cadenceTarget) {
			cadenceOK = false
		}
		if index == 0 && update.Kind != "initial" || index == len(value.Updates)-1 && update.Kind != "resolved" || index > 0 && index < len(value.Updates)-1 && update.Kind != "progress" {
			return false, time.Time{}, time.Time{}
		}
		if update.Kind == "progress" {
			progressCount++
		}
		previous = update.PublishedAt
	}
	first := value.Updates[0].PublishedAt
	resolved := value.Updates[len(value.Updates)-1].PublishedAt
	firstSeconds := first.Sub(value.ImpactConfirmedAt).Seconds()
	firstOK := firstSeconds <= float64(firstTarget)
	valid := progressCount > 0 && finite(value.FirstInternalUpdateSeconds) && value.FirstInternalUpdateSeconds == firstSeconds &&
		value.FirstInternalUpdateTargetSeconds == firstTarget && value.FirstInternalUpdateWithinTarget == firstOK && firstOK &&
		value.UpdateCadenceTargetSeconds == cadenceTarget && value.UpdateCadenceWithinTarget == cadenceOK && cadenceOK && value.InternalHistoryVisible
	return valid, first, resolved
}

func validDelivery(value receiptDelivery, first, resolved, completed time.Time) bool {
	initialSeconds := value.InitialNotificationReceivedAt.Sub(first).Seconds()
	resolutionSeconds := value.ResolutionNotificationReceivedAt.Sub(resolved).Seconds()
	return identifierPattern.MatchString(value.ChannelReference) && !value.InitialNotificationReceivedAt.Before(first) &&
		!value.InitialNotificationReceivedAt.After(resolved) && !value.ResolutionNotificationReceivedAt.Before(resolved) &&
		!value.ResolutionNotificationReceivedAt.After(completed) && finite(value.InitialDeliverySeconds) &&
		value.InitialDeliverySeconds == initialSeconds && finite(value.ResolutionDeliverySeconds) &&
		value.ResolutionDeliverySeconds == resolutionSeconds && value.DeliverySucceeded
}

func validRecovery(value receiptRecovery, started, resolved time.Time) bool {
	seconds := resolved.Sub(value.RecoveredAt).Seconds()
	return !value.RecoveredAt.Before(started) && !value.RecoveredAt.After(resolved) && finite(value.ObservationSecondsBeforeResolution) &&
		value.ObservationSecondsBeforeResolution == seconds && value.MinimumObservationSeconds == 900 && seconds >= 900 &&
		value.InternalProbePassed && value.InternalUserPathPassed && value.CleanupCompleted
}

func validReview(values map[string]bool) bool {
	keys := []string{"incidentTimelineConsistent", "correctiveActionsOwned", "sensitiveDataAbsent", "operationsApproved", "communicationsApproved"}
	if !exactKeys(values, keys) {
		return false
	}
	for _, key := range keys {
		if !values[key] {
			return false
		}
	}
	return true
}

func validEvidenceSet(manifest receiptReference, values map[string]receiptReference) bool {
	if !validReference(manifest) || !exactKeys(values, evidenceFields) {
		return false
	}
	paths := map[string]struct{}{manifest.Path: {}}
	for _, key := range evidenceFields {
		value := values[key]
		if !validReference(value) {
			return false
		}
		if _, duplicate := paths[value.Path]; duplicate {
			return false
		}
		paths[value.Path] = struct{}{}
	}
	return true
}

func validReference(value receiptReference) bool {
	return value.Path != "" && !strings.Contains(value.Path, "..") && !strings.HasPrefix(value.Path, "/") && parseDigestOnly(value.SHA256)
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func formatDigest(value []byte) string { return "sha256:" + hex.EncodeToString(value) }

func parseDigest(raw string) ([]byte, error) {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	if len(value) != 64 {
		return nil, errors.New("invalid digest")
	}
	return hex.DecodeString(value)
}

func parseDigestOnly(raw string) bool {
	_, err := parseDigest(raw)
	return err == nil
}

func validHTTPSReference(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && len(raw) <= 2048
}

func validApprovalEvidenceSHA256(raw string) bool {
	return approvalEvidenceSHA256Pattern.MatchString(raw) && raw != "sha256:"+strings.Repeat("0", 64)
}

func invalidReceipt(detail string) error {
	return problem.New(400, "incident_exercise_receipt_invalid", detail)
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

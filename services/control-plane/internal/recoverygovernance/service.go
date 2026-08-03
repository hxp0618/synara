package recoverygovernance

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
	receiptSchema     = "synara.recovery-drill-evidence-receipt.v2"
	receiptAssessment = "evidence-validated-not-control-passed"
	receiptMaxBytes   = 512 * 1024
)

var approvalRoles = []string{"database", "kms", "operations", "security", "storage"}
var approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var componentObjectives = map[string]map[string][2]float64{
	"postgresql":     {"postgresql-pitr": {300, 3600}},
	"object-storage": {"object-versioned-replica": {900, 7200}},
	"kms": {
		"kms-cloud-multi-region":  {0, 3600},
		"kms-vault-raft-snapshot": {86400, 7200},
	},
	"queue": {"queue-postgres-outbox-replay": {300, 3600}},
}

type Component struct {
	Key                 string  `json:"key"`
	Profile             string  `json:"profile"`
	SourceRegion        string  `json:"sourceRegion"`
	RestoreRegion       string  `json:"restoreRegion"`
	MeasuredRPOSeconds  float64 `json:"measuredRpoSeconds"`
	RPOObjectiveSeconds float64 `json:"rpoObjectiveSeconds"`
	MeasuredRTOSeconds  float64 `json:"measuredRtoSeconds"`
	RTOObjectiveSeconds float64 `json:"rtoObjectiveSeconds"`
	RPOWithinObjective  bool    `json:"rpoWithinObjective"`
	RTOWithinObjective  bool    `json:"rtoWithinObjective"`
	RestoreServedCanary bool    `json:"restoreServedCanary"`
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

type Drill struct {
	ID                                    uuid.UUID   `json:"id"`
	CandidateRecordID                     uuid.UUID   `json:"candidateRecordId"`
	CandidateID                           string      `json:"candidateId"`
	DrillID                               uuid.UUID   `json:"drillId"`
	ReceiptSHA256                         string      `json:"receiptSha256"`
	ReceiptSizeBytes                      int64       `json:"receiptSizeBytes"`
	CandidateBindingSHA256                string      `json:"candidateBindingSha256"`
	RecoverySubjectSHA256                 string      `json:"recoverySubjectSha256"`
	StartedAt                             time.Time   `json:"startedAt"`
	CompletedAt                           time.Time   `json:"completedAt"`
	ValidatedAt                           time.Time   `json:"validatedAt"`
	MeasurementsWithinObjectives          bool        `json:"measurementsWithinObjectives"`
	AllRestoreCanariesPassed              bool        `json:"allRestoreCanariesPassed"`
	AllSourceApprovalsApproved            bool        `json:"allSourceApprovalsApproved"`
	EligibleForHumanGateReview            bool        `json:"eligibleForHumanGateReview"`
	CryptographicSignaturesVerified       bool        `json:"cryptographicSignaturesVerified"`
	ExternalAuthorityVerificationRequired bool        `json:"externalAuthorityVerificationRequired"`
	State                                 string      `json:"state"`
	Version                               int64       `json:"version"`
	CreatedBy                             uuid.UUID   `json:"createdBy"`
	Components                            []Component `json:"components"`
	Approvals                             []Approval  `json:"approvals"`
	ApprovedAt                            *time.Time  `json:"approvedAt"`
	RejectedAt                            *time.Time  `json:"rejectedAt"`
	CreatedAt                             time.Time   `json:"createdAt"`
	UpdatedAt                             time.Time   `json:"updatedAt"`
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

func (s *Service) List(ctx context.Context) ([]Drill, error) {
	var models []persistence.Stage6RecoveryDrill
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "recovery_drills_load_failed", "Recovery drills could not be loaded.", err)
	}
	items := make([]Drill, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) Import(ctx context.Context, actorID uuid.UUID, input ImportInput, requestID, ipAddress string) (Drill, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", input.CandidateRecordID, s.operatorTenantID).Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Drill{}, problem.New(404, "recovery_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Drill{}, problem.Wrap(500, "recovery_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Drill{}, problem.New(403, "recovery_drill_import_forbidden", "The release candidate creator must import its exact Recovery receipt.")
	}
	binding, err := bindReceipt(input, candidate, s.now())
	if err != nil {
		return Drill{}, err
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
			return problem.New(409, "recovery_drill_exists", "This exact Recovery drill already exists.")
		} else if err != nil {
			return problem.Wrap(500, "recovery_drill_import_failed", "The Recovery drill could not be imported.", err)
		}
		for _, component := range binding.components {
			component.ID = uuid.New()
			component.RecoveryDrillRecordID = model.ID
			component.OperatorTenantID = s.operatorTenantID
			component.CreatedAt = model.CreatedAt
			if err := tx.Create(&component).Error; err != nil {
				return problem.Wrap(500, "recovery_components_import_failed", "Recovery component projections could not be imported.", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "recovery.drill_imported", ResourceType: "stage6_recovery_drill", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"candidateId": candidate.CandidateID, "drillId": model.DrillID, "eligibleForHumanGateReview": model.EligibleForHumanGateReview, "receiptSha256": input.ReceiptSHA256}})
	})
	if err != nil {
		return Drill{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, recordID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Drill, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(approvalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) || len(input.Reason) < 20 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validApprovalEvidenceSHA256(input.EvidenceSHA256) {
		return Drill{}, problem.New(400, "recovery_approval_invalid", "Recovery approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6RecoveryDrill
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", recordID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "recovery_drill_not_found", "The Recovery drill was not found.")
		} else if err != nil {
			return problem.Wrap(500, "recovery_drill_load_failed", "The Recovery drill could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "recovery_drill_not_reviewable", "The Recovery drill is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "recovery_drill_ineligible", "An ineligible Recovery drill cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, "recovery."+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6RecoveryApproval{ID: uuid.New(), RecoveryDrillRecordID: model.ID, OperatorTenantID: s.operatorTenantID, Role: input.Role, Decision: input.Decision, ApproverUserID: actorID, Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now()}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "recovery_approval_conflict", "This role or operator already recorded a Recovery decision.")
		} else if err != nil {
			return problem.Wrap(500, "recovery_approval_create_failed", "The Recovery decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "recovery.approval_recorded", ResourceType: "stage6_recovery_drill", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"role": input.Role, "decision": input.Decision, "evidenceReference": input.EvidenceReference, "evidenceSha256": input.EvidenceSHA256}}); err != nil {
			return err
		}
		target := ""
		if input.Decision == "rejected" {
			target = "rejected"
		} else {
			var count int64
			if err := tx.Model(&persistence.Stage6RecoveryApproval{}).Where("recovery_drill_record_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", model.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Count(&count).Error; err != nil {
				return problem.Wrap(500, "recovery_approvals_load_failed", "Recovery decisions could not be verified.", err)
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
		result := tx.Model(&persistence.Stage6RecoveryDrill{}).Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "recovery_drill_transition_failed", "The Recovery drill decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "recovery_drill_version_conflict", "The Recovery drill changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "recovery.drill_" + target, ResourceType: "stage6_recovery_drill", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress})
	})
	if err != nil {
		return Drill{}, err
	}
	return s.get(ctx, recordID)
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Drill, error) {
	var model persistence.Stage6RecoveryDrill
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Drill{}, problem.New(404, "recovery_drill_not_found", "The Recovery drill was not found.")
	} else if err != nil {
		return Drill{}, problem.Wrap(500, "recovery_drill_load_failed", "The Recovery drill could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6RecoveryDrill) (Drill, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Drill{}, problem.Wrap(500, "recovery_candidate_load_failed", "The linked release candidate could not be loaded.", err)
	}
	var componentModels []persistence.Stage6RecoveryComponent
	if err := db.WithContext(ctx).Where("recovery_drill_record_id = ?", model.ID).Order("component_key").Find(&componentModels).Error; err != nil {
		return Drill{}, problem.Wrap(500, "recovery_components_load_failed", "Recovery components could not be loaded.", err)
	}
	components := make([]Component, 0, len(componentModels))
	for _, item := range componentModels {
		components = append(components, Component{Key: item.ComponentKey, Profile: item.Profile, SourceRegion: item.SourceRegion, RestoreRegion: item.RestoreRegion, MeasuredRPOSeconds: item.MeasuredRPOSeconds, RPOObjectiveSeconds: item.RPOObjectiveSeconds, MeasuredRTOSeconds: item.MeasuredRTOSeconds, RTOObjectiveSeconds: item.RTOObjectiveSeconds, RPOWithinObjective: item.RPOWithinObjective, RTOWithinObjective: item.RTOWithinObjective, RestoreServedCanary: item.RestoreServedCanary})
	}
	type approvalRow struct {
		persistence.Stage6RecoveryApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_recovery_approvals approval").Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").Joins("JOIN users approver ON approver.id = approval.approver_user_id").Where("approval.recovery_drill_record_id = ?", model.ID).Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
		return Drill{}, problem.Wrap(500, "recovery_approvals_load_failed", "Recovery decisions could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, Approval{ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID, ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName, Reason: row.Reason, EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256, SupersededAt: row.SupersededAt, SupersededReason: row.SupersededReason, CreatedAt: row.CreatedAt})
	}
	return Drill{ID: model.ID, CandidateRecordID: model.CandidateRecordID, CandidateID: candidate.CandidateID, DrillID: model.DrillID, ReceiptSHA256: formatDigest(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes, CandidateBindingSHA256: formatDigest(model.CandidateBindingSHA256), RecoverySubjectSHA256: formatDigest(model.RecoverySubjectSHA256), StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, ValidatedAt: model.ValidatedAt, MeasurementsWithinObjectives: model.MeasurementsWithinObjectives, AllRestoreCanariesPassed: model.AllRestoreCanariesPassed, AllSourceApprovalsApproved: model.AllSourceApprovalsApproved, EligibleForHumanGateReview: model.EligibleForHumanGateReview, CryptographicSignaturesVerified: model.CryptographicSignaturesVerified, ExternalAuthorityVerificationRequired: model.ExternalAuthorityVerificationRequired, State: model.State, Version: model.Version, CreatedBy: model.CreatedBy, Components: components, Approvals: approvals, ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt}, nil
}

type receiptBinding struct {
	model      persistence.Stage6RecoveryDrill
	components []persistence.Stage6RecoveryComponent
}

type receiptComponent struct {
	Profile              string                                `json:"profile"`
	SourceAuthority      string                                `json:"sourceAuthority"`
	RestoreTarget        string                                `json:"restoreTarget"`
	SourceRegion         string                                `json:"sourceRegion"`
	RestoreRegion        string                                `json:"restoreRegion"`
	SourceFailureDomain  string                                `json:"sourceFailureDomain"`
	RestoreFailureDomain string                                `json:"restoreFailureDomain"`
	SourceCutoffAt       time.Time                             `json:"sourceCutoffAt"`
	RestoredThroughAt    time.Time                             `json:"restoredThroughAt"`
	RecoveryStartedAt    time.Time                             `json:"recoveryStartedAt"`
	ServiceReadyAt       time.Time                             `json:"serviceReadyAt"`
	MeasuredRPOSeconds   float64                               `json:"measuredRpoSeconds"`
	RPOObjectiveSeconds  float64                               `json:"rpoObjectiveSeconds"`
	RPOWithinObjective   bool                                  `json:"rpoWithinObjective"`
	MeasuredRTOSeconds   float64                               `json:"measuredRtoSeconds"`
	RTOObjectiveSeconds  float64                               `json:"rtoObjectiveSeconds"`
	RTOWithinObjective   bool                                  `json:"rtoWithinObjective"`
	RestoreServedCanary  bool                                  `json:"restoreServedCanary"`
	Evidence             map[string]map[string]json.RawMessage `json:"evidence"`
}

type sourceApproval struct {
	Role          string                     `json:"role"`
	ApproverID    string                     `json:"approverId"`
	Decision      string                     `json:"decision"`
	ApprovedAt    time.Time                  `json:"approvedAt"`
	ExpiresAt     time.Time                  `json:"expiresAt"`
	SubjectSHA256 string                     `json:"subjectSha256"`
	Evidence      map[string]json.RawMessage `json:"evidence"`
}

type recoveryReceipt struct {
	SchemaVersion                        string                                `json:"schemaVersion"`
	DrillID                              uuid.UUID                             `json:"drillId"`
	Candidate                            json.RawMessage                       `json:"candidate"`
	CandidateBindingSHA256               string                                `json:"candidateBindingSha256"`
	RestoredReleaseIdentity              json.RawMessage                       `json:"restoredReleaseIdentity"`
	StartedAt                            time.Time                             `json:"startedAt"`
	CompletedAt                          time.Time                             `json:"completedAt"`
	RecoverySubjectSHA256                string                                `json:"recoverySubjectSha256"`
	Manifest                             map[string]json.RawMessage            `json:"manifest"`
	Components                           map[string]receiptComponent           `json:"components"`
	Approvals                            map[string]sourceApproval             `json:"approvals"`
	ApprovalEvidence                     map[string]map[string]json.RawMessage `json:"approvalEvidence"`
	DeclaredMeasurementsWithinObjectives bool                                  `json:"declaredMeasurementsWithinObjectives"`
	AllRestoreCanariesPassed             bool                                  `json:"allRestoreCanariesPassed"`
	AllRequiredApprovalsApproved         bool                                  `json:"allRequiredApprovalsApproved"`
	ReleaseEligibleEnvironment           bool                                  `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview           bool                                  `json:"eligibleForHumanGateReview"`
	VerificationBoundary                 map[string]json.RawMessage            `json:"verificationBoundary"`
	ValidatedAt                          time.Time                             `json:"validatedAt"`
	Assessment                           string                                `json:"assessment"`
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (receiptBinding, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return receiptBinding{}, invalidReceipt("Recovery receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return receiptBinding{}, invalidReceipt("Recovery receipt must be bounded UTF-8 JSON.")
	}
	digest := sha256.Sum256(data)
	expectedDigest, err := parseDigest(input.ReceiptSHA256)
	if err != nil || !bytes.Equal(expectedDigest, digest[:]) {
		return receiptBinding{}, invalidReceipt("Recovery receipt SHA-256 does not match its exact bytes.")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || !exactKeys(top, []string{"schemaVersion", "drillId", "candidate", "candidateBindingSha256", "restoredReleaseIdentity", "startedAt", "completedAt", "recoverySubjectSha256", "manifest", "components", "approvals", "approvalEvidence", "declaredMeasurementsWithinObjectives", "allRestoreCanariesPassed", "allRequiredApprovalsApproved", "releaseEligibleEnvironment", "eligibleForHumanGateReview", "verificationBoundary", "validatedAt", "assessment"}) {
		return receiptBinding{}, invalidReceipt("Recovery receipt schema is invalid.")
	}
	var receipt recoveryReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return receiptBinding{}, invalidReceipt("Recovery receipt JSON is invalid.")
	}
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment || receipt.DrillID == uuid.Nil || !receipt.CompletedAt.After(receipt.StartedAt) || receipt.ValidatedAt.Before(receipt.CompletedAt) || receipt.ValidatedAt.After(receipt.CompletedAt.Add(7*24*time.Hour)) || receipt.ValidatedAt.After(now.Add(5*time.Minute)) {
		return receiptBinding{}, invalidReceipt("Recovery receipt identity or timestamps are invalid.")
	}
	expectedCandidate, err := expectedRecoveryCandidate(candidate)
	if err != nil || !canonicalEqual(receipt.Candidate, expectedCandidate) {
		return receiptBinding{}, invalidReceipt("Recovery receipt is not bound to the exact Release candidate.")
	}
	candidateBinding := sha256.Sum256(canonicalJSON(receipt.Candidate))
	declaredCandidateBinding, err := parseDigest(receipt.CandidateBindingSHA256)
	if err != nil || !bytes.Equal(candidateBinding[:], declaredCandidateBinding) {
		return receiptBinding{}, invalidReceipt("Recovery candidate binding digest is invalid.")
	}
	expectedRestored, err := expectedRestoredIdentity(expectedCandidate)
	if err != nil || !canonicalEqual(receipt.RestoredReleaseIdentity, expectedRestored) {
		return receiptBinding{}, invalidReceipt("Restored release identity does not match the exact candidate.")
	}
	if !exactKeys(receipt.Manifest, []string{"path", "sha256"}) || !validEvidenceReference(receipt.Manifest) {
		return receiptBinding{}, invalidReceipt("Recovery manifest reference is invalid.")
	}
	if len(receipt.Components) != len(componentObjectives) || len(receipt.Approvals) != len(approvalRoles) || len(receipt.ApprovalEvidence) != len(approvalRoles) {
		return receiptBinding{}, invalidReceipt("Recovery component or source approval set is incomplete.")
	}
	components := make([]persistence.Stage6RecoveryComponent, 0, len(componentObjectives))
	allWithin := true
	allCanaries := true
	componentSubject := make(map[string]any, len(componentObjectives))
	for key := range componentObjectives {
		item, ok := receipt.Components[key]
		raw := topComponent(top["components"], key)
		if !ok || raw == nil || !validComponentKeys(raw) {
			return receiptBinding{}, invalidReceipt("Recovery component projection is invalid.")
		}
		objectives, ok := componentObjectives[key][item.Profile]
		rpo := item.SourceCutoffAt.Sub(item.RestoredThroughAt).Seconds()
		rto := item.ServiceReadyAt.Sub(item.RecoveryStartedAt).Seconds()
		if !ok || item.SourceAuthority == item.RestoreTarget || item.SourceRegion == item.RestoreRegion || item.SourceFailureDomain == item.RestoreFailureDomain || !(receipt.StartedAt.Compare(item.RestoredThroughAt) <= 0 && item.RestoredThroughAt.Compare(item.SourceCutoffAt) <= 0 && item.SourceCutoffAt.Compare(item.RecoveryStartedAt) <= 0 && item.RecoveryStartedAt.Compare(item.ServiceReadyAt) <= 0 && item.ServiceReadyAt.Compare(receipt.CompletedAt) <= 0) || math.Abs(item.MeasuredRPOSeconds-rpo) > 1e-9 || math.Abs(item.MeasuredRTOSeconds-rto) > 1e-9 || item.RPOObjectiveSeconds != objectives[0] || item.RTOObjectiveSeconds != objectives[1] || item.RPOWithinObjective != (rpo <= objectives[0]) || item.RTOWithinObjective != (rto <= objectives[1]) || !validComponentEvidence(item.Evidence) {
			return receiptBinding{}, invalidReceipt("Recovery component measurements do not match the frozen contract.")
		}
		allWithin = allWithin && item.RPOWithinObjective && item.RTOWithinObjective
		allCanaries = allCanaries && item.RestoreServedCanary
		components = append(components, persistence.Stage6RecoveryComponent{ComponentKey: key, Profile: item.Profile, SourceRegion: item.SourceRegion, RestoreRegion: item.RestoreRegion, MeasuredRPOSeconds: rpo, RPOObjectiveSeconds: objectives[0], MeasuredRTOSeconds: rto, RTOObjectiveSeconds: objectives[1], RPOWithinObjective: item.RPOWithinObjective, RTOWithinObjective: item.RTOWithinObjective, RestoreServedCanary: item.RestoreServedCanary})
		componentSubject[key] = decodeAnyUseNumber(raw)
	}
	slices.SortFunc(components, func(a, b persistence.Stage6RecoveryComponent) int {
		return strings.Compare(a.ComponentKey, b.ComponentKey)
	})
	recoverySubject := map[string]any{"drillId": receipt.DrillID.String(), "candidate": decodeAnyUseNumber(receipt.Candidate), "startedAt": formatUTC(receipt.StartedAt), "completedAt": formatUTC(receipt.CompletedAt), "restoredReleaseIdentity": decodeAnyUseNumber(receipt.RestoredReleaseIdentity), "components": componentSubject}
	subjectDigest := sha256.Sum256(mustJSON(recoverySubject))
	declaredSubject, err := parseDigest(receipt.RecoverySubjectSHA256)
	if err != nil || !bytes.Equal(subjectDigest[:], declaredSubject) {
		return receiptBinding{}, invalidReceipt("Recovery subject digest is invalid.")
	}
	allSourceApproved := true
	approverIDs := map[string]struct{}{}
	for _, role := range approvalRoles {
		approval, ok := receipt.Approvals[role]
		reference, referenceOK := receipt.ApprovalEvidence[role]
		if !ok || !referenceOK || approval.Role != role || approval.SubjectSHA256 != receipt.RecoverySubjectSHA256 || !canonicalMapsEqual(approval.Evidence, reference) || !validEvidenceReference(reference) || approval.ApproverID == "" || !approval.ExpiresAt.After(receipt.ValidatedAt) || approval.ApprovedAt.Before(receipt.CompletedAt) || approval.ApprovedAt.After(receipt.ValidatedAt) {
			return receiptBinding{}, invalidReceipt("Recovery source approval binding is invalid.")
		}
		if _, duplicate := approverIDs[approval.ApproverID]; duplicate {
			return receiptBinding{}, invalidReceipt("Recovery source approvals require distinct declared approvers.")
		}
		approverIDs[approval.ApproverID] = struct{}{}
		if approval.Decision != "approved-for-human-gate-review" && approval.Decision != "reviewed-not-approved" {
			return receiptBinding{}, invalidReceipt("Recovery source approval decision is invalid.")
		}
		allSourceApproved = allSourceApproved && approval.Decision == "approved-for-human-gate-review"
	}
	var boundary struct {
		ApprovalContentAndSubjectValidated                        bool `json:"approvalContentAndSubjectValidated"`
		CryptographicSignaturesVerified                           bool `json:"cryptographicSignaturesVerified"`
		RealBackupRestoreAndApproverAuthorityVerificationRequired bool `json:"realBackupRestoreAndApproverAuthorityVerificationRequired"`
	}
	if !exactKeys(receipt.VerificationBoundary, []string{"approvalContentAndSubjectValidated", "cryptographicSignaturesVerified", "realBackupRestoreAndApproverAuthorityVerificationRequired"}) || json.Unmarshal(top["verificationBoundary"], &boundary) != nil || !boundary.ApprovalContentAndSubjectValidated || boundary.CryptographicSignaturesVerified || !boundary.RealBackupRestoreAndApproverAuthorityVerificationRequired {
		return receiptBinding{}, invalidReceipt("Recovery external verification boundary is invalid.")
	}
	environmentEligible := recoveryEnvironmentEligible(receipt.Candidate)
	eligible := environmentEligible && allWithin && allCanaries && allSourceApproved
	if receipt.DeclaredMeasurementsWithinObjectives != allWithin || receipt.AllRestoreCanariesPassed != allCanaries || receipt.AllRequiredApprovalsApproved != allSourceApproved || receipt.ReleaseEligibleEnvironment != environmentEligible || receipt.EligibleForHumanGateReview != eligible {
		return receiptBinding{}, invalidReceipt("Recovery eligibility projection is inconsistent.")
	}
	return receiptBinding{model: persistence.Stage6RecoveryDrill{DrillID: receipt.DrillID, Receipt: slices.Clone(data), ReceiptSHA256: slices.Clone(digest[:]), ReceiptSizeBytes: int64(len(data)), Schema: receipt.SchemaVersion, Assessment: receipt.Assessment, CandidateBindingSHA256: slices.Clone(candidateBinding[:]), RecoverySubjectSHA256: slices.Clone(subjectDigest[:]), StartedAt: receipt.StartedAt.UTC(), CompletedAt: receipt.CompletedAt.UTC(), ValidatedAt: receipt.ValidatedAt.UTC(), MeasurementsWithinObjectives: allWithin, AllRestoreCanariesPassed: allCanaries, AllSourceApprovalsApproved: allSourceApproved, EligibleForHumanGateReview: eligible, CryptographicSignaturesVerified: false, ExternalAuthorityVerificationRequired: true, State: "recorded", Version: 1}, components: components}, nil
}

func expectedRecoveryCandidate(candidate persistence.Stage6ReleaseCandidate) (json.RawMessage, error) {
	var receipt struct {
		Candidate map[string]json.RawMessage `json:"candidate"`
	}
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil || receipt.Candidate == nil {
		return nil, errors.New("candidate evidence receipt is invalid")
	}
	delete(receipt.Candidate, "desktopArtifactSetSha256")
	return json.Marshal(receipt.Candidate)
}

func expectedRestoredIdentity(candidate json.RawMessage) (json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(candidate, &value); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"sourceCommit": value["sourceCommit"], "lockfileSha256": value["lockfileSha256"], "artifacts": value["artifacts"], "migrationTail": value["migrationTail"]})
}

func validComponentKeys(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	return json.Unmarshal(raw, &fields) == nil && exactKeys(fields, []string{"profile", "sourceAuthority", "restoreTarget", "sourceRegion", "restoreRegion", "sourceFailureDomain", "restoreFailureDomain", "sourceCutoffAt", "restoredThroughAt", "recoveryStartedAt", "serviceReadyAt", "measuredRpoSeconds", "rpoObjectiveSeconds", "rpoWithinObjective", "measuredRtoSeconds", "rtoObjectiveSeconds", "rtoWithinObjective", "restoreServedCanary", "evidence"})
}

func topComponent(raw json.RawMessage, key string) json.RawMessage {
	var values map[string]json.RawMessage
	_ = json.Unmarshal(raw, &values)
	return values[key]
}

func validComponentEvidence(values map[string]map[string]json.RawMessage) bool {
	if len(values) != 3 {
		return false
	}
	for _, key := range []string{"backupEvidence", "restoreEvidence", "clientEvidence"} {
		if !validEvidenceReference(values[key]) {
			return false
		}
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

func recoveryEnvironmentEligible(candidate json.RawMessage) bool {
	var value struct {
		EnvironmentClass string `json:"environmentClass"`
	}
	return json.Unmarshal(candidate, &value) == nil && slices.Contains([]string{"production", "production-like"}, value.EnvironmentClass)
}

func canonicalEqual(a, b json.RawMessage) bool {
	return bytes.Equal(canonicalJSON(a), canonicalJSON(b))
}
func canonicalJSON(raw json.RawMessage) []byte { return mustJSON(decodeAnyUseNumber(raw)) }
func canonicalMapsEqual(a, b map[string]json.RawMessage) bool {
	return bytes.Equal(mustJSON(a), mustJSON(b))
}
func decodeAnyUseNumber(raw json.RawMessage) any {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	return value
}
func mustJSON(value any) []byte        { encoded, _ := json.Marshal(value); return encoded }
func formatUTC(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
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
func invalidReceipt(detail string) error { return problem.New(400, "recovery_receipt_invalid", detail) }
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

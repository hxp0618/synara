package slogovernance

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
	receiptSchema     = "synara.slo-window-evidence-receipt.v1"
	receiptAssessment = "evidence-validated-not-slo-passed"
	receiptMaxBytes   = 256 * 1024
)

var approvalRoles = []string{"engineering", "operations", "security", "product"}
var gitRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var boundedIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$`)
var publicOriginPattern = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
var approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var objectiveTargets = map[string]float64{
	"availability": 0.999, "apiLatency": 0.99, "executionStartDelay": 0.99, "eventDelay": 0.999,
}
var objectiveMinimumSamples = map[string]int64{
	"availability": 0, "apiLatency": 10_000, "executionStartDelay": 100, "eventDelay": 1_000,
}

type Objective struct {
	Key                       string  `json:"key"`
	TargetRatio               float64 `json:"targetRatio"`
	GoodRatio                 float64 `json:"goodRatio"`
	SampleCount               int64   `json:"sampleCount"`
	ErrorBudgetRemainingRatio float64 `json:"errorBudgetRemainingRatio"`
	PolicyState               string  `json:"policyState"`
	Assessable                bool    `json:"assessable"`
	ObjectiveMet              bool    `json:"objectiveMet"`
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

type Window struct {
	ID                         uuid.UUID   `json:"id"`
	CandidateRecordID          uuid.UUID   `json:"candidateRecordId"`
	CandidateID                string      `json:"candidateId"`
	WindowID                   uuid.UUID   `json:"windowId"`
	ReceiptSHA256              string      `json:"receiptSha256"`
	ReceiptSizeBytes           int64       `json:"receiptSizeBytes"`
	ReleaseCommit              string      `json:"releaseCommit"`
	EnvironmentClass           string      `json:"environmentClass"`
	EnvironmentID              string      `json:"environmentId"`
	PublicOrigin               string      `json:"publicOrigin"`
	WindowStartedAt            time.Time   `json:"windowStartedAt"`
	WindowCompletedAt          time.Time   `json:"windowCompletedAt"`
	ValidatedAt                time.Time   `json:"validatedAt"`
	QueryRevision              string      `json:"queryRevision"`
	AllObjectivesAssessable    bool        `json:"allObjectivesAssessable"`
	AllObjectivesMet           bool        `json:"allObjectivesMet"`
	EligibleForHumanGateReview bool        `json:"eligibleForHumanGateReview"`
	WorstBudgetRemainingRatio  float64     `json:"worstBudgetRemainingRatio"`
	BudgetPolicyState          string      `json:"budgetPolicyState"`
	State                      string      `json:"state"`
	Version                    int64       `json:"version"`
	CreatedBy                  uuid.UUID   `json:"createdBy"`
	Objectives                 []Objective `json:"objectives"`
	Approvals                  []Approval  `json:"approvals"`
	ApprovedAt                 *time.Time  `json:"approvedAt"`
	RejectedAt                 *time.Time  `json:"rejectedAt"`
	CreatedAt                  time.Time   `json:"createdAt"`
	UpdatedAt                  time.Time   `json:"updatedAt"`
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

func (s *Service) List(ctx context.Context) ([]Window, error) {
	var models []persistence.Stage6SLOWindow
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "slo_windows_load_failed", "SLO windows could not be loaded.", err)
	}
	result := make([]Window, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Service) Import(ctx context.Context, actorID uuid.UUID, input ImportInput, requestID, ipAddress string) (Window, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", input.CandidateRecordID, s.operatorTenantID).Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Window{}, problem.New(404, "slo_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Window{}, problem.Wrap(500, "slo_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Window{}, problem.New(403, "slo_window_import_forbidden", "The release candidate creator must import its exact SLO window receipt.")
	}
	binding, err := bindReceipt(input, candidate, s.now())
	if err != nil {
		return Window{}, err
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
			return problem.New(409, "slo_window_exists", "This exact SLO window already exists.")
		} else if err != nil {
			return problem.Wrap(500, "slo_window_import_failed", "The SLO window could not be imported.", err)
		}
		for _, objective := range binding.objectives {
			objective.ID = uuid.New()
			objective.SLOWindowRecordID = model.ID
			objective.OperatorTenantID = s.operatorTenantID
			objective.CreatedAt = model.CreatedAt
			if err := tx.Create(&objective).Error; err != nil {
				return problem.Wrap(500, "slo_objectives_import_failed", "SLO objective projections could not be imported.", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "slo.window_imported", ResourceType: "stage6_slo_window", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"candidateId": candidate.CandidateID, "windowId": model.WindowID, "receiptSha256": input.ReceiptSHA256, "eligibleForHumanGateReview": model.EligibleForHumanGateReview, "budgetPolicyState": model.BudgetPolicyState},
		})
	})
	if err != nil {
		return Window{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, recordID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Window, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(approvalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) || len(input.Reason) < 20 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validApprovalEvidenceSHA256(input.EvidenceSHA256) {
		return Window{}, problem.New(400, "slo_approval_invalid", "SLO approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6SLOWindow
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", recordID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "slo_window_not_found", "The SLO window was not found.")
		} else if err != nil {
			return problem.Wrap(500, "slo_window_load_failed", "The SLO window could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "slo_window_not_reviewable", "The SLO window is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "slo_window_ineligible", "An ineligible or failed SLO window cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, "release."+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6SLOApproval{ID: uuid.New(), SLOWindowRecordID: model.ID, OperatorTenantID: s.operatorTenantID, Role: input.Role, Decision: input.Decision, ApproverUserID: actorID, Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now()}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "slo_approval_conflict", "This role or operator already recorded an SLO decision.")
		} else if err != nil {
			return problem.Wrap(500, "slo_approval_create_failed", "The SLO decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "slo.approval_recorded", ResourceType: "stage6_slo_window", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"role": input.Role, "decision": input.Decision, "evidenceReference": input.EvidenceReference, "evidenceSha256": input.EvidenceSHA256}}); err != nil {
			return err
		}
		target := ""
		if input.Decision == "rejected" {
			target = "rejected"
		} else {
			var count int64
			if err := tx.Model(&persistence.Stage6SLOApproval{}).Where("slo_window_record_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", model.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Count(&count).Error; err != nil {
				return problem.Wrap(500, "slo_approvals_load_failed", "SLO decisions could not be verified.", err)
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
		result := tx.Model(&persistence.Stage6SLOWindow{}).Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "slo_window_transition_failed", "The SLO window decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "slo_window_version_conflict", "The SLO window changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "slo.window_" + target, ResourceType: "stage6_slo_window", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress})
	})
	if err != nil {
		return Window{}, err
	}
	return s.get(ctx, recordID)
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Window, error) {
	var model persistence.Stage6SLOWindow
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Window{}, problem.New(404, "slo_window_not_found", "The SLO window was not found.")
	} else if err != nil {
		return Window{}, problem.Wrap(500, "slo_window_load_failed", "The SLO window could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6SLOWindow) (Window, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Window{}, problem.Wrap(500, "slo_candidate_load_failed", "The linked release candidate could not be loaded.", err)
	}
	var objectiveModels []persistence.Stage6SLOObjective
	if err := db.WithContext(ctx).Where("slo_window_record_id = ?", model.ID).Order("objective_key").Find(&objectiveModels).Error; err != nil {
		return Window{}, problem.Wrap(500, "slo_objectives_load_failed", "SLO objectives could not be loaded.", err)
	}
	objectives := make([]Objective, 0, len(objectiveModels))
	for _, item := range objectiveModels {
		objectives = append(objectives, Objective{Key: item.ObjectiveKey, TargetRatio: item.TargetRatio, GoodRatio: item.GoodRatio, SampleCount: item.SampleCount, ErrorBudgetRemainingRatio: item.ErrorBudgetRemainingRatio, PolicyState: item.PolicyState, Assessable: item.Assessable, ObjectiveMet: item.ObjectiveMet})
	}
	type approvalRow struct {
		persistence.Stage6SLOApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_slo_approvals approval").Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").Joins("JOIN users approver ON approver.id = approval.approver_user_id").Where("approval.slo_window_record_id = ?", model.ID).Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
		return Window{}, problem.Wrap(500, "slo_approvals_load_failed", "SLO decisions could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, Approval{ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID, ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName, Reason: row.Reason, EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256, SupersededAt: row.SupersededAt, SupersededReason: row.SupersededReason, CreatedAt: row.CreatedAt})
	}
	return Window{ID: model.ID, CandidateRecordID: model.CandidateRecordID, CandidateID: candidate.CandidateID, WindowID: model.WindowID, ReceiptSHA256: "sha256:" + hex.EncodeToString(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes, ReleaseCommit: model.ReleaseCommit, EnvironmentClass: model.EnvironmentClass, EnvironmentID: model.EnvironmentID, PublicOrigin: model.PublicOrigin, WindowStartedAt: model.WindowStartedAt, WindowCompletedAt: model.WindowCompletedAt, ValidatedAt: model.ValidatedAt, QueryRevision: model.QueryRevision, AllObjectivesAssessable: model.AllObjectivesAssessable, AllObjectivesMet: model.AllObjectivesMet, EligibleForHumanGateReview: model.EligibleForHumanGateReview, WorstBudgetRemainingRatio: model.WorstBudgetRemainingRatio, BudgetPolicyState: model.BudgetPolicyState, State: model.State, Version: model.Version, CreatedBy: model.CreatedBy, Objectives: objectives, Approvals: approvals, ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt}, nil
}

type receiptBinding struct {
	model      persistence.Stage6SLOWindow
	objectives []persistence.Stage6SLOObjective
}

type receiptObjective struct {
	TargetRatio               float64 `json:"targetRatio"`
	GoodRatio                 float64 `json:"goodRatio"`
	SampleCount               int64   `json:"sampleCount"`
	MinimumSamples            int64   `json:"minimumSamples"`
	VolumeSufficient          bool    `json:"volumeSufficient"`
	ErrorBudgetRemainingRatio float64 `json:"errorBudgetRemainingRatio"`
	ErrorBudgetPolicyState    string  `json:"errorBudgetPolicyState"`
	NoDataIntervalCount       int64   `json:"noDataIntervalCount"`
	ExcludedSampleCount       int64   `json:"excludedSampleCount"`
	QuerySucceeded            bool    `json:"querySucceeded"`
	Assessable                bool    `json:"assessable"`
	ObjectiveMet              bool    `json:"objectiveMet"`
}

type receiptProbe struct {
	Regions         []string `json:"regions"`
	RegionCount     int      `json:"regionCount"`
	CadenceSeconds  int      `json:"cadenceSeconds"`
	ExpectedSamples int64    `json:"expectedSamples"`
	ObservedSamples int64    `json:"observedSamples"`
	CoverageRatio   float64  `json:"coverageRatio"`
}

type receiptCompanionSignals struct {
	Reviewed bool `json:"reviewed"`
}

type sloReceipt struct {
	SchemaVersion                        string                      `json:"schemaVersion"`
	WindowID                             uuid.UUID                   `json:"windowId"`
	ReleaseCommit                        string                      `json:"releaseCommit"`
	EnvironmentClass                     string                      `json:"environmentClass"`
	EnvironmentID                        string                      `json:"environmentId"`
	PublicOrigin                         string                      `json:"publicOrigin"`
	WindowStartedAt                      time.Time                   `json:"windowStartedAt"`
	WindowCompletedAt                    time.Time                   `json:"windowCompletedAt"`
	QueryRevision                        string                      `json:"queryRevision"`
	ExternalProbe                        receiptProbe                `json:"externalProbe"`
	Objectives                           map[string]receiptObjective `json:"objectives"`
	AllObjectivesAssessable              bool                        `json:"allObjectivesAssessable"`
	DeclaredMeasurementsWithinObjectives bool                        `json:"declaredMeasurementsWithinObjectives"`
	ReleaseEligibleEnvironment           bool                        `json:"releaseEligibleEnvironment"`
	ExternalProbeCoverageComplete        bool                        `json:"externalProbeCoverageComplete"`
	BurnReviewComplete                   bool                        `json:"burnReviewComplete"`
	CompanionSignals                     receiptCompanionSignals     `json:"companionSignals"`
	EligibleForHumanGateReview           bool                        `json:"eligibleForHumanGateReview"`
	ValidatedAt                          time.Time                   `json:"validatedAt"`
	Assessment                           string                      `json:"assessment"`
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (receiptBinding, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return receiptBinding{}, invalidReceipt("SLO receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return receiptBinding{}, invalidReceipt("SLO receipt must be bounded UTF-8 JSON.")
	}
	digest := sha256.Sum256(data)
	expected, err := parseSHA256(input.ReceiptSHA256)
	if err != nil || !bytes.Equal(expected, digest[:]) {
		return receiptBinding{}, invalidReceipt("SLO receipt SHA-256 does not match its exact bytes.")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || !exactKeys(top, []string{
		"alertSummary", "allObjectivesAssessable", "assessment", "burnReview", "burnReviewComplete",
		"companionSignals", "declaredMeasurementsWithinObjectives", "eligibleForHumanGateReview", "environmentClass",
		"environmentId", "evidence", "externalProbe", "externalProbeCoverageComplete", "manifest", "objectives",
		"publicOrigin", "queryRevision", "releaseCommit", "releaseEligibleEnvironment", "schemaVersion", "validatedAt",
		"windowCompletedAt", "windowId", "windowSeconds", "windowStartedAt",
	}) {
		return receiptBinding{}, invalidReceipt("SLO receipt schema is invalid.")
	}
	var rawProbe map[string]json.RawMessage
	if err := json.Unmarshal(top["externalProbe"], &rawProbe); err != nil || !exactKeys(rawProbe, []string{
		"regions", "regionCount", "cadenceSeconds", "expectedSamples", "observedSamples", "coverageRatio",
	}) {
		return receiptBinding{}, invalidReceipt("SLO external probe projection is invalid.")
	}
	var rawObjectives map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["objectives"], &rawObjectives); err != nil || len(rawObjectives) != len(objectiveTargets) {
		return receiptBinding{}, invalidReceipt("SLO objective projection is invalid.")
	}
	objectiveFields := []string{
		"targetRatio", "goodRatio", "sampleCount", "minimumSamples", "volumeSufficient", "errorBudgetRemainingRatio",
		"errorBudgetPolicyState", "noDataIntervalCount", "excludedSampleCount", "querySucceeded", "assessable", "objectiveMet",
	}
	for key := range objectiveTargets {
		if !exactKeys(rawObjectives[key], objectiveFields) {
			return receiptBinding{}, invalidReceipt("SLO objective projection is invalid.")
		}
	}
	var receipt sloReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return receiptBinding{}, invalidReceipt("SLO receipt JSON is invalid.")
	}
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment || receipt.WindowID == uuid.Nil ||
		receipt.ReleaseCommit != candidate.SourceCommit || receipt.EnvironmentID != candidate.EnvironmentID ||
		!boundedIdentifierPattern.MatchString(receipt.EnvironmentID) || !gitRevisionPattern.MatchString(receipt.QueryRevision) ||
		!slices.Contains([]string{"production", "production-like"}, receipt.EnvironmentClass) || !validOrigin(receipt.PublicOrigin) ||
		receipt.WindowCompletedAt.Sub(receipt.WindowStartedAt) < 30*24*time.Hour || receipt.ValidatedAt.Before(receipt.WindowCompletedAt) || receipt.ValidatedAt.After(now.Add(5*time.Minute)) ||
		!receipt.ReleaseEligibleEnvironment || len(receipt.Objectives) != len(objectiveTargets) ||
		receipt.ExternalProbe.RegionCount != len(receipt.ExternalProbe.Regions) || receipt.ExternalProbe.RegionCount < 3 ||
		receipt.ExternalProbe.CadenceSeconds != 30 || receipt.ExternalProbe.ExpectedSamples <= 0 ||
		receipt.ExternalProbe.ExpectedSamples < int64(math.Floor(receipt.WindowCompletedAt.Sub(receipt.WindowStartedAt).Seconds()/float64(receipt.ExternalProbe.CadenceSeconds)))*int64(receipt.ExternalProbe.RegionCount) ||
		receipt.ExternalProbe.ObservedSamples <= 0 || receipt.ExternalProbe.ObservedSamples > receipt.ExternalProbe.ExpectedSamples ||
		math.Abs(receipt.ExternalProbe.CoverageRatio-float64(receipt.ExternalProbe.ObservedSamples)/float64(receipt.ExternalProbe.ExpectedSamples)) > 1e-9 ||
		receipt.ExternalProbeCoverageComplete != (receipt.ExternalProbe.CoverageRatio >= .95) {
		return receiptBinding{}, invalidReceipt("SLO receipt identity or release eligibility is invalid.")
	}
	regions := make(map[string]struct{}, len(receipt.ExternalProbe.Regions))
	for _, region := range receipt.ExternalProbe.Regions {
		if !boundedIdentifierPattern.MatchString(region) {
			return receiptBinding{}, invalidReceipt("SLO external probe regions must be bounded identifiers.")
		}
		if _, exists := regions[region]; exists {
			return receiptBinding{}, invalidReceipt("SLO external probe regions must be unique.")
		}
		regions[region] = struct{}{}
	}
	objectives := make([]persistence.Stage6SLOObjective, 0, len(objectiveTargets))
	worst := 1.0
	allAssessable := true
	allMet := true
	for key, target := range objectiveTargets {
		item, ok := receipt.Objectives[key]
		calculated := budgetRemaining(item.GoodRatio, target)
		minimum := objectiveMinimumSamples[key]
		volumeSufficient := item.SampleCount >= minimum
		assessable := item.QuerySucceeded && item.NoDataIntervalCount == 0 && item.ExcludedSampleCount == 0 && volumeSufficient
		if !ok || item.TargetRatio != target || item.GoodRatio < 0 || item.GoodRatio > 1 || item.SampleCount < 0 ||
			item.MinimumSamples != minimum || item.VolumeSufficient != volumeSufficient || item.NoDataIntervalCount < 0 || item.ExcludedSampleCount < 0 ||
			item.Assessable != assessable || math.Abs(item.ErrorBudgetRemainingRatio-calculated) > 1e-9 ||
			item.ErrorBudgetPolicyState != budgetPolicy(calculated) || item.ObjectiveMet != (assessable && item.GoodRatio >= target) {
			return receiptBinding{}, invalidReceipt("SLO objective projection does not match the frozen contract.")
		}
		if key == "availability" && item.SampleCount != receipt.ExternalProbe.ObservedSamples {
			return receiptBinding{}, invalidReceipt("Availability samples do not match the external probe authority.")
		}
		worst = math.Min(worst, calculated)
		allAssessable = allAssessable && item.Assessable
		allMet = allMet && item.ObjectiveMet
		objectives = append(objectives, persistence.Stage6SLOObjective{ObjectiveKey: key, TargetRatio: target, GoodRatio: item.GoodRatio, SampleCount: item.SampleCount, ErrorBudgetRemainingRatio: calculated, PolicyState: item.ErrorBudgetPolicyState, Assessable: item.Assessable, ObjectiveMet: item.ObjectiveMet})
	}
	eligible := receipt.ReleaseEligibleEnvironment && receipt.ExternalProbeCoverageComplete && allAssessable && allMet && receipt.BurnReviewComplete && receipt.CompanionSignals.Reviewed
	if receipt.AllObjectivesAssessable != allAssessable || receipt.DeclaredMeasurementsWithinObjectives != allMet || receipt.EligibleForHumanGateReview != eligible {
		return receiptBinding{}, invalidReceipt("SLO receipt eligibility projection is inconsistent.")
	}
	slices.SortFunc(objectives, func(a, b persistence.Stage6SLOObjective) int { return strings.Compare(a.ObjectiveKey, b.ObjectiveKey) })
	return receiptBinding{model: persistence.Stage6SLOWindow{WindowID: receipt.WindowID, Receipt: slices.Clone(data), ReceiptSHA256: slices.Clone(digest[:]), ReceiptSizeBytes: int64(len(data)), Schema: receipt.SchemaVersion, Assessment: receipt.Assessment, ReleaseCommit: receipt.ReleaseCommit, EnvironmentClass: receipt.EnvironmentClass, EnvironmentID: receipt.EnvironmentID, PublicOrigin: receipt.PublicOrigin, WindowStartedAt: receipt.WindowStartedAt.UTC(), WindowCompletedAt: receipt.WindowCompletedAt.UTC(), ValidatedAt: receipt.ValidatedAt.UTC(), QueryRevision: receipt.QueryRevision, AllObjectivesAssessable: allAssessable, AllObjectivesMet: allMet, EligibleForHumanGateReview: eligible, WorstBudgetRemainingRatio: worst, BudgetPolicyState: budgetPolicy(worst), State: "recorded", Version: 1}, objectives: objectives}, nil
}

func budgetRemaining(good, target float64) float64 {
	return math.Min(math.Max(1-(1-good)/(1-target), 0), 1)
}

func budgetPolicy(value float64) string {
	if value == 0 {
		return "reliability-freeze"
	}
	if value <= .25 {
		return "risky-rollout-paused"
	}
	if value <= .5 {
		return "risk-note-required"
	}
	return "normal-delivery"
}

func parseSHA256(raw string) ([]byte, error) {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	if len(value) != 64 {
		return nil, errors.New("invalid sha256")
	}
	return hex.DecodeString(value)
}

func validOrigin(raw string) bool {
	return len(raw) <= 512 && publicOriginPattern.MatchString(raw)
}

func validHTTPSReference(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && len(raw) <= 2048
}

func validApprovalEvidenceSHA256(value string) bool {
	return approvalEvidenceSHA256Pattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}

func invalidReceipt(detail string) error { return problem.New(400, "slo_receipt_invalid", detail) }

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

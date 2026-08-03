package capacitygovernance

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
	receiptSchema     = "synara.capacity-soak-evidence-receipt.v1"
	receiptAssessment = "evidence-validated-not-capacity-passed"
	receiptMaxBytes   = 512 * 1024
)

var approvalRoles = []string{"engineering", "operations"}
var approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var phaseNames = []string{"steady-peak", "burst", "tenant-hotspot", "rolling-disruption", "cooldown"}
var capacityDimensions = []string{"peakConcurrentSessions", "peakExecutionStartsPerMinute", "peakEventAppendsPerSecond", "peakSSEConnections"}
var evidenceFields = []string{"workloadGeneratorEvidence", "prometheusRangeEvidence", "externalCanaryEvidence", "databaseEvidence", "kubernetesEvidence", "applicationLogEvidence", "resultSummaryEvidence"}

type Phase struct {
	Name            string    `json:"name"`
	StartedAt       time.Time `json:"startedAt"`
	CompletedAt     time.Time `json:"completedAt"`
	DurationSeconds int64     `json:"durationSeconds"`
	LoadMultiplier  float64   `json:"loadMultiplier"`
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

type Run struct {
	ID                                    uuid.UUID  `json:"id"`
	CandidateRecordID                     uuid.UUID  `json:"candidateRecordId"`
	CandidateID                           string     `json:"candidateId"`
	RunID                                 uuid.UUID  `json:"runId"`
	ReceiptSHA256                         string     `json:"receiptSha256"`
	ReceiptSizeBytes                      int64      `json:"receiptSizeBytes"`
	ReleaseCommit                         string     `json:"releaseCommit"`
	EnvironmentClass                      string     `json:"environmentClass"`
	EnvironmentID                         string     `json:"environmentId"`
	StartedAt                             time.Time  `json:"startedAt"`
	CompletedAt                           time.Time  `json:"completedAt"`
	ValidatedAt                           time.Time  `json:"validatedAt"`
	DurationSeconds                       int64      `json:"durationSeconds"`
	MinimumDurationSeconds                int64      `json:"minimumDurationSeconds"`
	SampleIntervalSeconds                 int64      `json:"sampleIntervalSeconds"`
	ExternalProbeRegions                  int64      `json:"externalProbeRegions"`
	ExternalProbeCoverageRatio            float64    `json:"externalProbeCoverageRatio"`
	ForecastHeadroomCovered               bool       `json:"forecastHeadroomCovered"`
	PhaseCoverageComplete                 bool       `json:"phaseCoverageComplete"`
	ExerciseCoverageComplete              bool       `json:"exerciseCoverageComplete"`
	MeasurementsWithinObjectives          bool       `json:"measurementsWithinObjectives"`
	ReleaseEligibleEnvironment            bool       `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview            bool       `json:"eligibleForHumanGateReview"`
	CryptographicSignaturesVerified       bool       `json:"cryptographicSignaturesVerified"`
	ExternalAuthorityVerificationRequired bool       `json:"externalAuthorityVerificationRequired"`
	State                                 string     `json:"state"`
	Version                               int64      `json:"version"`
	CreatedBy                             uuid.UUID  `json:"createdBy"`
	Phases                                []Phase    `json:"phases"`
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

func (s *Service) List(ctx context.Context) ([]Run, error) {
	var models []persistence.Stage6CapacityRun
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "capacity_runs_load_failed", "Capacity runs could not be loaded.", err)
	}
	items := make([]Run, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) Import(ctx context.Context, actorID uuid.UUID, input ImportInput, requestID, ipAddress string) (Run, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", input.CandidateRecordID, s.operatorTenantID).Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Run{}, problem.New(404, "capacity_candidate_not_found", "The release candidate was not found.")
	} else if err != nil {
		return Run{}, problem.Wrap(500, "capacity_candidate_load_failed", "The release candidate could not be loaded.", err)
	}
	if candidate.CreatedBy != actorID {
		return Run{}, problem.New(403, "capacity_run_import_forbidden", "The release candidate creator must import its exact Capacity receipt.")
	}
	binding, err := bindReceipt(input, candidate, s.now())
	if err != nil {
		return Run{}, err
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
			return problem.New(409, "capacity_run_exists", "This exact Capacity run already exists.")
		} else if err != nil {
			return problem.Wrap(500, "capacity_run_import_failed", "The Capacity run could not be imported.", err)
		}
		for _, phase := range binding.phases {
			phase.ID = uuid.New()
			phase.CapacityRunID = model.ID
			phase.OperatorTenantID = s.operatorTenantID
			phase.CreatedAt = model.CreatedAt
			if err := tx.Create(&phase).Error; err != nil {
				return problem.Wrap(500, "capacity_phases_import_failed", "Capacity phase projections could not be imported.", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "capacity.run_imported", ResourceType: "stage6_capacity_run", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"candidateId": candidate.CandidateID, "runId": model.RunID, "receiptSha256": input.ReceiptSHA256, "eligibleForHumanGateReview": model.EligibleForHumanGateReview}})
	})
	if err != nil {
		return Run{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, recordID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Run, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(approvalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) || len(input.Reason) < 20 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validApprovalEvidenceSHA256(input.EvidenceSHA256) {
		return Run{}, problem.New(400, "capacity_approval_invalid", "Capacity approval requires an exact role, decision, bounded reason, HTTPS evidence and non-zero SHA-256.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.Stage6CapacityRun
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND operator_tenant_id = ?", recordID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "capacity_run_not_found", "The Capacity run was not found.")
		} else if err != nil {
			return problem.Wrap(500, "capacity_run_load_failed", "The Capacity run could not be loaded.", err)
		}
		if model.State != "recorded" || model.CreatedBy == actorID {
			return problem.New(409, "capacity_run_not_reviewable", "The Capacity run is not reviewable by this operator.")
		}
		if input.Decision == "approved" && !model.EligibleForHumanGateReview {
			return problem.New(409, "capacity_run_ineligible", "An ineligible or failed Capacity run cannot be approved.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.CapacityPrefix+input.Role); err != nil {
			return err
		}
		approval := persistence.Stage6CapacityApproval{ID: uuid.New(), CapacityRunID: model.ID, OperatorTenantID: s.operatorTenantID, Role: input.Role, Decision: input.Decision, ApproverUserID: actorID, Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now()}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "capacity_approval_conflict", "This role or operator already recorded a Capacity decision.")
		} else if err != nil {
			return problem.Wrap(500, "capacity_approval_create_failed", "The Capacity decision could not be recorded.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "capacity.approval_recorded", ResourceType: "stage6_capacity_run", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"role": input.Role, "decision": input.Decision, "evidenceReference": input.EvidenceReference, "evidenceSha256": input.EvidenceSHA256}}); err != nil {
			return err
		}
		target := ""
		if input.Decision == "rejected" {
			target = "rejected"
		} else {
			var count int64
			if err := tx.Model(&persistence.Stage6CapacityApproval{}).Where("capacity_run_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", model.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Count(&count).Error; err != nil {
				return problem.Wrap(500, "capacity_approvals_load_failed", "Capacity decisions could not be verified.", err)
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
		result := tx.Model(&persistence.Stage6CapacityRun{}).Where("id = ? AND state = ? AND version = ?", model.ID, "recorded", model.Version).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "capacity_run_transition_failed", "The Capacity run decision could not be finalized.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "capacity_run_version_conflict", "The Capacity run changed before the decision committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: "capacity.run_" + target, ResourceType: "stage6_capacity_run", ResourceID: &model.ID, RequestID: requestID, IPAddress: ipAddress})
	})
	if err != nil {
		return Run{}, err
	}
	return s.get(ctx, recordID)
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Run, error) {
	var model persistence.Stage6CapacityRun
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Run{}, problem.New(404, "capacity_run_not_found", "The Capacity run was not found.")
	} else if err != nil {
		return Run{}, problem.Wrap(500, "capacity_run_load_failed", "The Capacity run could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6CapacityRun) (Run, error) {
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.WithContext(ctx).Where("id = ?", model.CandidateRecordID).Take(&candidate).Error; err != nil {
		return Run{}, problem.Wrap(500, "capacity_candidate_load_failed", "The linked release candidate could not be loaded.", err)
	}
	var phaseModels []persistence.Stage6CapacityPhase
	if err := db.WithContext(ctx).Where("capacity_run_id = ?", model.ID).Order("started_at, id").Find(&phaseModels).Error; err != nil {
		return Run{}, problem.Wrap(500, "capacity_phases_load_failed", "Capacity phases could not be loaded.", err)
	}
	phases := make([]Phase, 0, len(phaseModels))
	for _, item := range phaseModels {
		phases = append(phases, Phase{Name: item.Name, StartedAt: item.StartedAt, CompletedAt: item.CompletedAt, DurationSeconds: item.DurationSeconds, LoadMultiplier: item.LoadMultiplier})
	}
	type approvalRow struct {
		persistence.Stage6CapacityApproval
		ApproverEmail string `gorm:"column:approver_email"`
		ApproverName  string `gorm:"column:approver_name"`
	}
	var rows []approvalRow
	if err := db.WithContext(ctx).Table("stage6_capacity_approvals approval").Select("approval.*, approver.email AS approver_email, approver.display_name AS approver_name").Joins("JOIN users approver ON approver.id = approval.approver_user_id").Where("approval.capacity_run_id = ?", model.ID).Order("approval.created_at, approval.id").Scan(&rows).Error; err != nil {
		return Run{}, problem.Wrap(500, "capacity_approvals_load_failed", "Capacity decisions could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, Approval{ID: row.ID, Role: row.Role, Decision: row.Decision, ApproverUserID: row.ApproverUserID, ApproverEmail: row.ApproverEmail, ApproverName: row.ApproverName, Reason: row.Reason, EvidenceReference: row.EvidenceReference, EvidenceSHA256: row.EvidenceSHA256, SupersededAt: row.SupersededAt, SupersededReason: row.SupersededReason, CreatedAt: row.CreatedAt})
	}
	return Run{ID: model.ID, CandidateRecordID: model.CandidateRecordID, CandidateID: candidate.CandidateID, RunID: model.RunID, ReceiptSHA256: formatDigest(model.ReceiptSHA256), ReceiptSizeBytes: model.ReceiptSizeBytes, ReleaseCommit: model.ReleaseCommit, EnvironmentClass: model.EnvironmentClass, EnvironmentID: model.EnvironmentID, StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, ValidatedAt: model.ValidatedAt, DurationSeconds: model.DurationSeconds, MinimumDurationSeconds: model.MinimumDurationSeconds, SampleIntervalSeconds: model.SampleIntervalSeconds, ExternalProbeRegions: model.ExternalProbeRegions, ExternalProbeCoverageRatio: model.ExternalProbeCoverageRatio, ForecastHeadroomCovered: model.ForecastHeadroomCovered, PhaseCoverageComplete: model.PhaseCoverageComplete, ExerciseCoverageComplete: model.ExerciseCoverageComplete, MeasurementsWithinObjectives: model.MeasurementsWithinObjectives, ReleaseEligibleEnvironment: model.ReleaseEligibleEnvironment, EligibleForHumanGateReview: model.EligibleForHumanGateReview, CryptographicSignaturesVerified: model.CryptographicSignaturesVerified, ExternalAuthorityVerificationRequired: model.ExternalAuthorityVerificationRequired, State: model.State, Version: model.Version, CreatedBy: model.CreatedBy, Phases: phases, Approvals: approvals, ApprovedAt: model.ApprovedAt, RejectedAt: model.RejectedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt}, nil
}

type receiptPhase struct {
	Name            string    `json:"name"`
	StartedAt       time.Time `json:"startedAt"`
	CompletedAt     time.Time `json:"completedAt"`
	DurationSeconds float64   `json:"durationSeconds"`
	LoadMultiplier  float64   `json:"loadMultiplier"`
}

type receiptMeasurements struct {
	AvailabilityGoodRatio                     float64 `json:"availabilityGoodRatio"`
	APILatencyGoodRatio                       float64 `json:"apiLatencyGoodRatio"`
	ExecutionStartGoodRatio                   float64 `json:"executionStartGoodRatio"`
	EventDelayGoodRatio                       float64 `json:"eventDelayGoodRatio"`
	HTTPRequestCount                          int64   `json:"httpRequestCount"`
	ExecutionStartCount                       int64   `json:"executionStartCount"`
	EventAppendCount                          int64   `json:"eventAppendCount"`
	MaximumDatabaseConnectionUtilizationRatio float64 `json:"maximumDatabaseConnectionUtilizationRatio"`
	MaximumControlPlaneCPUUtilizationRatio    float64 `json:"maximumControlPlaneCPUUtilizationRatio"`
	MaximumControlPlaneMemoryUtilizationRatio float64 `json:"maximumControlPlaneMemoryUtilizationRatio"`
	MaximumOutboxOldestSeconds                float64 `json:"maximumOutboxOldestSeconds"`
	MaximumQueueOldestSeconds                 float64 `json:"maximumQueueOldestSeconds"`
	MaximumWarmDeficitUnits                   int64   `json:"maximumWarmDeficitUnits"`
	DeadLetterCount                           int64   `json:"deadLetterCount"`
	OOMKillCount                              int64   `json:"oomKillCount"`
	UnexpectedRestartCount                    int64   `json:"unexpectedRestartCount"`
	MinimumTenantSuccessRatio                 float64 `json:"minimumTenantSuccessRatio"`
	MaximumTenantSuccessRatio                 float64 `json:"maximumTenantSuccessRatio"`
	FailedAssertionCount                      int64   `json:"failedAssertionCount"`
	TenantFairnessWithinObjective             bool    `json:"tenantFairnessWithinObjective"`
}

type receiptExercises struct {
	ExternalProbeRegions       int  `json:"externalProbeRegions"`
	ControlPlaneRollingRestart bool `json:"controlPlaneRollingRestart"`
	WorkerChurn                bool `json:"workerChurn"`
	DatabaseConnectionPressure bool `json:"databaseConnectionPressure"`
	OutboxBackpressure         bool `json:"outboxBackpressure"`
	TenantHotspot              bool `json:"tenantHotspot"`
}

type capacityReceipt struct {
	SchemaVersion                        string                                `json:"schemaVersion"`
	RunID                                uuid.UUID                             `json:"runId"`
	ReleaseCommit                        string                                `json:"releaseCommit"`
	EnvironmentClass                     string                                `json:"environmentClass"`
	EnvironmentID                        string                                `json:"environmentId"`
	StartedAt                            time.Time                             `json:"startedAt"`
	CompletedAt                          time.Time                             `json:"completedAt"`
	DurationSeconds                      float64                               `json:"durationSeconds"`
	MinimumDurationSeconds               float64                               `json:"minimumDurationSeconds"`
	SampleIntervalSeconds                int64                                 `json:"sampleIntervalSeconds"`
	ExternalProbeRegions                 int64                                 `json:"externalProbeRegions"`
	ExternalProbeCoverageRatio           float64                               `json:"externalProbeCoverageRatio"`
	Forecast                             map[string]float64                    `json:"forecast"`
	Load                                 map[string]float64                    `json:"load"`
	ForecastHeadroomCovered              bool                                  `json:"forecastHeadroomCovered"`
	Phases                               []receiptPhase                        `json:"phases"`
	Measurements                         receiptMeasurements                   `json:"measurements"`
	DeclaredMeasurementsWithinObjectives bool                                  `json:"declaredMeasurementsWithinObjectives"`
	Exercises                            receiptExercises                      `json:"exercises"`
	Manifest                             map[string]json.RawMessage            `json:"manifest"`
	Evidence                             map[string]map[string]json.RawMessage `json:"evidence"`
	ReleaseEligibleEnvironment           bool                                  `json:"releaseEligibleEnvironment"`
	EligibleForHumanGateReview           bool                                  `json:"eligibleForHumanGateReview"`
	ValidatedAt                          time.Time                             `json:"validatedAt"`
	Assessment                           string                                `json:"assessment"`
}

type receiptBinding struct {
	model  persistence.Stage6CapacityRun
	phases []persistence.Stage6CapacityPhase
}

func bindReceipt(input ImportInput, candidate persistence.Stage6ReleaseCandidate, now time.Time) (receiptBinding, error) {
	encoded := strings.TrimSpace(input.ReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(receiptMaxBytes) {
		return receiptBinding{}, invalidReceipt("Capacity receipt must be bounded base64.")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > receiptMaxBytes || !utf8.Valid(data) {
		return receiptBinding{}, invalidReceipt("Capacity receipt must be bounded UTF-8 JSON.")
	}
	digest := sha256.Sum256(data)
	expected, err := parseDigest(input.ReceiptSHA256)
	if err != nil || !bytes.Equal(expected, digest[:]) {
		return receiptBinding{}, invalidReceipt("Capacity receipt SHA-256 does not match its exact bytes.")
	}
	var top map[string]json.RawMessage
	topKeys := []string{"schemaVersion", "runId", "releaseCommit", "environmentClass", "environmentId", "startedAt", "completedAt", "durationSeconds", "minimumDurationSeconds", "sampleIntervalSeconds", "externalProbeRegions", "externalProbeCoverageRatio", "forecast", "load", "forecastHeadroomCovered", "phases", "measurements", "declaredMeasurementsWithinObjectives", "exercises", "manifest", "evidence", "releaseEligibleEnvironment", "eligibleForHumanGateReview", "validatedAt", "assessment"}
	if err := json.Unmarshal(data, &top); err != nil || !exactKeys(top, topKeys) {
		return receiptBinding{}, invalidReceipt("Capacity receipt schema is invalid.")
	}
	var receipt capacityReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return receiptBinding{}, invalidReceipt("Capacity receipt JSON is invalid.")
	}
	if receipt.SchemaVersion != receiptSchema || receipt.Assessment != receiptAssessment || receipt.RunID == uuid.Nil || !receipt.CompletedAt.After(receipt.StartedAt) || receipt.ValidatedAt.Before(receipt.CompletedAt) || receipt.ValidatedAt.After(receipt.CompletedAt.Add(30*24*time.Hour)) || receipt.ValidatedAt.After(now.Add(5*time.Minute)) {
		return receiptBinding{}, invalidReceipt("Capacity receipt identity or timestamps are invalid.")
	}
	projection, err := candidateProjection(candidate)
	if err != nil || projection.CapacityReceiptSHA256 != formatDigest(digest[:]) || projection.SourceCommit != receipt.ReleaseCommit || projection.EnvironmentID != receipt.EnvironmentID || projection.EnvironmentClass != receipt.EnvironmentClass {
		return receiptBinding{}, invalidReceipt("Capacity receipt is not bound to the exact Release candidate.")
	}
	minimumDuration := int64(72 * 60 * 60)
	if receipt.EnvironmentClass == "production-like" {
		minimumDuration = 24 * 60 * 60
	} else if receipt.EnvironmentClass != "production" {
		return receiptBinding{}, invalidReceipt("Capacity receipt environment is not release eligible.")
	}
	duration, ok := exactPositiveInteger(receipt.DurationSeconds)
	declaredMinimum, minimumOK := exactPositiveInteger(receipt.MinimumDurationSeconds)
	if !ok || !minimumOK || duration != int64(receipt.CompletedAt.Sub(receipt.StartedAt).Seconds()) || declaredMinimum != minimumDuration || duration < minimumDuration || !receipt.ReleaseEligibleEnvironment {
		return receiptBinding{}, invalidReceipt("Capacity duration or environment projection is inconsistent.")
	}
	if receipt.SampleIntervalSeconds < 1 || receipt.SampleIntervalSeconds > 60 || receipt.ExternalProbeRegions < 3 || !finiteRatio(receipt.ExternalProbeCoverageRatio) || receipt.ExternalProbeCoverageRatio < 0.95 {
		return receiptBinding{}, invalidReceipt("Capacity external probe projection is incomplete.")
	}
	headroom := validateHeadroom(receipt.Forecast, receipt.Load)
	if !headroom || receipt.ForecastHeadroomCovered != headroom {
		return receiptBinding{}, invalidReceipt("Capacity forecast headroom projection is invalid.")
	}
	phases, phasesOK := bindPhases(receipt.Phases, receipt.StartedAt, receipt.CompletedAt)
	if !phasesOK {
		return receiptBinding{}, invalidReceipt("Capacity phase projection is incomplete.")
	}
	exercisesOK := receipt.Exercises.ExternalProbeRegions == int(receipt.ExternalProbeRegions) && receipt.Exercises.ControlPlaneRollingRestart && receipt.Exercises.WorkerChurn && receipt.Exercises.DatabaseConnectionPressure && receipt.Exercises.OutboxBackpressure && receipt.Exercises.TenantHotspot
	if !exercisesOK {
		return receiptBinding{}, invalidReceipt("Capacity exercise projection is incomplete.")
	}
	measurementsWithin := validateMeasurements(receipt.Measurements)
	if receipt.DeclaredMeasurementsWithinObjectives != measurementsWithin || !measurementsWithin {
		return receiptBinding{}, invalidReceipt("Capacity measurement projection does not meet the declared objectives.")
	}
	if !validEvidenceReference(receipt.Manifest) || !validEvidenceSet(receipt.Evidence) {
		return receiptBinding{}, invalidReceipt("Capacity evidence references are incomplete.")
	}
	eligible := receipt.ReleaseEligibleEnvironment && headroom && phasesOK && exercisesOK && measurementsWithin
	if receipt.EligibleForHumanGateReview != eligible || !eligible {
		return receiptBinding{}, invalidReceipt("Capacity eligibility projection is inconsistent.")
	}
	return receiptBinding{model: persistence.Stage6CapacityRun{RunID: receipt.RunID, Receipt: slices.Clone(data), ReceiptSHA256: slices.Clone(digest[:]), ReceiptSizeBytes: int64(len(data)), Schema: receipt.SchemaVersion, Assessment: receipt.Assessment, ReleaseCommit: receipt.ReleaseCommit, EnvironmentClass: receipt.EnvironmentClass, EnvironmentID: receipt.EnvironmentID, StartedAt: receipt.StartedAt.UTC(), CompletedAt: receipt.CompletedAt.UTC(), ValidatedAt: receipt.ValidatedAt.UTC(), DurationSeconds: duration, MinimumDurationSeconds: declaredMinimum, SampleIntervalSeconds: receipt.SampleIntervalSeconds, ExternalProbeRegions: receipt.ExternalProbeRegions, ExternalProbeCoverageRatio: receipt.ExternalProbeCoverageRatio, ForecastHeadroomCovered: true, PhaseCoverageComplete: true, ExerciseCoverageComplete: true, MeasurementsWithinObjectives: true, ReleaseEligibleEnvironment: true, EligibleForHumanGateReview: true, CryptographicSignaturesVerified: false, ExternalAuthorityVerificationRequired: true, State: "recorded", Version: 1}, phases: phases}, nil
}

type projectedCandidate struct {
	SourceCommit          string
	EnvironmentClass      string
	EnvironmentID         string
	CapacityReceiptSHA256 string
}

func candidateProjection(candidate persistence.Stage6ReleaseCandidate) (projectedCandidate, error) {
	var receipt struct {
		Candidate struct {
			SourceCommit     string `json:"sourceCommit"`
			EnvironmentClass string `json:"environmentClass"`
			EnvironmentID    string `json:"environmentId"`
		} `json:"candidate"`
		Receipts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &receipt); err != nil {
		return projectedCandidate{}, err
	}
	capacity, ok := receipt.Receipts["capacity"]
	if !ok || capacity.SHA256 == "" || receipt.Candidate.SourceCommit != candidate.SourceCommit || receipt.Candidate.EnvironmentID != candidate.EnvironmentID {
		return projectedCandidate{}, errors.New("candidate capacity receipt reference is missing")
	}
	return projectedCandidate{SourceCommit: receipt.Candidate.SourceCommit, EnvironmentClass: receipt.Candidate.EnvironmentClass, EnvironmentID: receipt.Candidate.EnvironmentID, CapacityReceiptSHA256: capacity.SHA256}, nil
}

func validateHeadroom(forecast, load map[string]float64) bool {
	if !exactKeys(forecast, append(slices.Clone(capacityDimensions), "requiredHeadroomPercent")) || !exactKeys(load, capacityDimensions) {
		return false
	}
	headroom := forecast["requiredHeadroomPercent"]
	if !finite(headroom) || headroom < 20 || headroom > 200 {
		return false
	}
	multiplier := 1 + headroom/100
	for _, key := range capacityDimensions {
		if !finite(forecast[key]) || !finite(load[key]) || forecast[key] < 1 || load[key] < forecast[key]*multiplier {
			return false
		}
	}
	return true
}

func bindPhases(values []receiptPhase, started, completed time.Time) ([]persistence.Stage6CapacityPhase, bool) {
	if len(values) != len(phaseNames) {
		return nil, false
	}
	seen := map[string]struct{}{}
	previousEnd := started
	result := make([]persistence.Stage6CapacityPhase, 0, len(values))
	for _, value := range values {
		duration, ok := exactPositiveInteger(value.DurationSeconds)
		if !ok || !slices.Contains(phaseNames, value.Name) || !value.CompletedAt.After(value.StartedAt) || value.StartedAt.Before(started) || value.CompletedAt.After(completed) || value.StartedAt.Before(previousEnd) || duration != int64(value.CompletedAt.Sub(value.StartedAt).Seconds()) || !finite(value.LoadMultiplier) || value.LoadMultiplier < 0.1 {
			return nil, false
		}
		if _, duplicate := seen[value.Name]; duplicate {
			return nil, false
		}
		seen[value.Name] = struct{}{}
		previousEnd = value.CompletedAt
		result = append(result, persistence.Stage6CapacityPhase{Name: value.Name, StartedAt: value.StartedAt.UTC(), CompletedAt: value.CompletedAt.UTC(), DurationSeconds: duration, LoadMultiplier: value.LoadMultiplier})
	}
	return result, len(seen) == len(phaseNames)
}

func validateMeasurements(value receiptMeasurements) bool {
	fairness := finiteRatio(value.MaximumTenantSuccessRatio) && finiteRatio(value.MinimumTenantSuccessRatio) && value.MaximumTenantSuccessRatio > 0 && value.MinimumTenantSuccessRatio/value.MaximumTenantSuccessRatio >= 0.8
	return finiteRatio(value.AvailabilityGoodRatio) && value.AvailabilityGoodRatio >= 0.999 && finiteRatio(value.APILatencyGoodRatio) && value.APILatencyGoodRatio >= 0.99 && finiteRatio(value.ExecutionStartGoodRatio) && value.ExecutionStartGoodRatio >= 0.99 && finiteRatio(value.EventDelayGoodRatio) && value.EventDelayGoodRatio >= 0.999 && value.HTTPRequestCount >= 10_000 && value.ExecutionStartCount >= 100 && value.EventAppendCount >= 1_000 && finiteRatio(value.MaximumDatabaseConnectionUtilizationRatio) && value.MaximumDatabaseConnectionUtilizationRatio <= 0.8 && finiteRatio(value.MaximumControlPlaneCPUUtilizationRatio) && value.MaximumControlPlaneCPUUtilizationRatio <= 0.8 && finiteRatio(value.MaximumControlPlaneMemoryUtilizationRatio) && value.MaximumControlPlaneMemoryUtilizationRatio <= 0.8 && finite(value.MaximumOutboxOldestSeconds) && value.MaximumOutboxOldestSeconds >= 0 && value.MaximumOutboxOldestSeconds <= 300 && finite(value.MaximumQueueOldestSeconds) && value.MaximumQueueOldestSeconds >= 0 && value.MaximumQueueOldestSeconds <= 60 && value.MaximumWarmDeficitUnits == 0 && value.DeadLetterCount == 0 && value.OOMKillCount == 0 && value.UnexpectedRestartCount == 0 && value.FailedAssertionCount == 0 && value.TenantFairnessWithinObjective == fairness && fairness
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

func exactPositiveInteger(value float64) (int64, bool) {
	if !finite(value) || value <= 0 || value > math.MaxInt64 || math.Trunc(value) != value {
		return 0, false
	}
	return int64(value), true
}

func finite(value float64) bool        { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func finiteRatio(value float64) bool   { return finite(value) && value >= 0 && value <= 1 }
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
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && len(raw) <= 2048
}
func validApprovalEvidenceSHA256(raw string) bool {
	return approvalEvidenceSHA256Pattern.MatchString(raw) && raw != "sha256:"+strings.Repeat("0", 64)
}
func invalidReceipt(detail string) error { return problem.New(400, "capacity_receipt_invalid", detail) }
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

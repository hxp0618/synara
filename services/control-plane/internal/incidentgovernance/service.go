package incidentgovernance

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var (
	incidentKeyPattern            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{2,119}$`)
	regionPattern                 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$`)
	statusRefPattern              = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{2,299}$`)
	approvalEvidenceSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	employeeSensitiveAssignment   = regexp.MustCompile(`(?i)\b(?:authorization|bearer|token|secret|password|credential|api[_-]?key|cookie|prompt)\b\s*[:=]`)
	employeeBearerCredential      = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
)

var severities = []string{"SEV-0", "SEV-1", "SEV-2", "SEV-3"}

const internalIncidentUpdateTopic = "incident.internal-update"

var components = []string{
	"control-plane-api", "authentication-sso", "execution-scheduling",
	"worker-runtime", "artifact-service", "web-application",
}

type Operator struct {
	UserID      uuid.UUID `json:"userId"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
}

type InternalUpdate struct {
	ID                uuid.UUID `json:"id"`
	Kind              string    `json:"kind"`
	Summary           string    `json:"summary"`
	PublishedAt       time.Time `json:"publishedAt"`
	EvidenceReference string    `json:"evidenceReference"`
	CreatedBy         Operator  `json:"createdBy"`
	CreatedAt         time.Time `json:"createdAt"`
}

type InternalNotification struct {
	ID             uuid.UUID  `json:"id"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	AvailableAt    time.Time  `json:"availableAt"`
	PublishedAt    *time.Time `json:"publishedAt"`
	DeadLetteredAt *time.Time `json:"deadLetteredAt"`
}

type ResolutionApproval struct {
	ID                uuid.UUID  `json:"id"`
	Decision          string     `json:"decision"`
	Reason            string     `json:"reason"`
	EvidenceReference string     `json:"evidenceReference"`
	EvidenceSHA256    *string    `json:"evidenceSha256"`
	SupersededAt      *time.Time `json:"supersededAt"`
	SupersededReason  *string    `json:"supersededReason"`
	Approver          Operator   `json:"approver"`
	CreatedAt         time.Time  `json:"createdAt"`
}

type Cadence struct {
	Status                          string     `json:"status"`
	FirstInternalUpdateTargetAt     *time.Time `json:"firstInternalUpdateTargetAt"`
	NextInternalUpdateTargetAt      *time.Time `json:"nextInternalUpdateTargetAt"`
	FirstInternalUpdateWithinTarget *bool      `json:"firstInternalUpdateWithinTarget"`
	OverdueSeconds                  int64      `json:"overdueSeconds"`
}

type Incident struct {
	ID                                   uuid.UUID              `json:"id"`
	IncidentKey                          string                 `json:"incidentKey"`
	Severity                             string                 `json:"severity"`
	State                                string                 `json:"state"`
	Title                                string                 `json:"title"`
	InternalImpactSummary                string                 `json:"internalImpactSummary"`
	BroadInternalImpact                  bool                   `json:"broadInternalImpact"`
	SecurityPrivacyImpact                bool                   `json:"securityPrivacyImpact"`
	InternalStatusBoardOrigin            *string                `json:"internalStatusBoardOrigin"`
	InternalStatusBoardIncidentReference *string                `json:"internalStatusBoardIncidentReference"`
	AffectedComponents                   []string               `json:"affectedComponents"`
	AffectedRegions                      []string               `json:"affectedRegions"`
	IncidentCommander                    Operator               `json:"incidentCommander"`
	CommunicationsLead                   Operator               `json:"communicationsLead"`
	SecurityPrivacyLead                  *Operator              `json:"securityPrivacyLead"`
	StartedAt                            time.Time              `json:"startedAt"`
	ImpactConfirmedAt                    time.Time              `json:"impactConfirmedAt"`
	Cadence                              Cadence                `json:"cadence"`
	InternalUpdates                      []InternalUpdate       `json:"internalUpdates"`
	InternalNotifications                []InternalNotification `json:"internalNotifications"`
	ResolutionApproval                   *ResolutionApproval    `json:"resolutionApproval"`
	ResolutionApprovals                  []ResolutionApproval   `json:"resolutionApprovals"`
	ResolvedAt                           *time.Time             `json:"resolvedAt"`
	CancelledAt                          *time.Time             `json:"cancelledAt"`
	Version                              int64                  `json:"version"`
	CreatedBy                            uuid.UUID              `json:"createdBy"`
	CreatedAt                            time.Time              `json:"createdAt"`
	UpdatedAt                            time.Time              `json:"updatedAt"`
}

type CreateInput struct {
	IncidentKey               string     `json:"incidentKey"`
	Severity                  string     `json:"severity"`
	Title                     string     `json:"title"`
	InternalImpactSummary     string     `json:"internalImpactSummary"`
	BroadInternalImpact       bool       `json:"broadInternalImpact"`
	SecurityPrivacyImpact     bool       `json:"securityPrivacyImpact"`
	AffectedComponents        []string   `json:"affectedComponents"`
	AffectedRegions           []string   `json:"affectedRegions"`
	CommunicationsLeadUserID  uuid.UUID  `json:"communicationsLeadUserId"`
	SecurityPrivacyLeadUserID *uuid.UUID `json:"securityPrivacyLeadUserId"`
	StartedAt                 time.Time  `json:"startedAt"`
	ImpactConfirmedAt         time.Time  `json:"impactConfirmedAt"`
}

type BindStatusBoardInput struct {
	ExpectedVersion                      int64  `json:"expectedVersion"`
	InternalStatusBoardIncidentReference string `json:"internalStatusBoardIncidentReference"`
	Reason                               string `json:"reason"`
}

type AddUpdateInput struct {
	ExpectedVersion   int64     `json:"expectedVersion"`
	Kind              string    `json:"kind"`
	Summary           string    `json:"summary"`
	PublishedAt       time.Time `json:"publishedAt"`
	EvidenceReference string    `json:"evidenceReference"`
}

type ResolutionApprovalInput struct {
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
	db                *gorm.DB
	operatorTenantID  uuid.UUID
	statusBoardOrigin string
	now               func() time.Time
}

func NewService(db *gorm.DB, operatorTenantID uuid.UUID, internalStatusBoardURL string) *Service {
	return &Service{
		db: db, operatorTenantID: operatorTenantID,
		statusBoardOrigin: normalizedOrigin(internalStatusBoardURL),
		now:               func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(ctx context.Context) ([]Incident, error) {
	var models []persistence.Stage6Incident
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "incidents_load_failed", "Incidents could not be loaded.", err)
	}
	result := make([]Incident, 0, len(models))
	for _, model := range models {
		view, err := s.view(ctx, s.db, model)
		if err != nil {
			return nil, err
		}
		result = append(result, view)
	}
	return result, nil
}

func (s *Service) Create(
	ctx context.Context,
	actorID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (Incident, error) {
	model, err := s.normalizeCreate(actorID, input)
	if err != nil {
		return Incident{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, userID := range assignedUsers(model) {
			if err := s.requireActiveOperator(ctx, tx, userID); err != nil {
				return err
			}
		}
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "incident_exists", "This incident identifier already exists.")
		} else if err != nil {
			return problem.Wrap(500, "incident_create_failed", "Incident could not be created.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.created", ResourceType: "stage6_incident", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"incidentKey": model.IncidentKey, "severity": model.Severity,
				"broadInternalImpact": model.BroadInternalImpact, "securityPrivacyImpact": model.SecurityPrivacyImpact,
				"affectedComponents": model.AffectedComponents, "affectedRegions": model.AffectedRegions,
				"incidentCommanderUserId":  model.IncidentCommanderUserID,
				"communicationsLeadUserId": model.CommunicationsLeadUserID,
			},
		})
	})
	if err != nil {
		return Incident{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) BindStatusBoard(
	ctx context.Context,
	actorID, incidentID uuid.UUID,
	input BindStatusBoardInput,
	requestID, ipAddress string,
) (Incident, error) {
	input.InternalStatusBoardIncidentReference = strings.TrimSpace(input.InternalStatusBoardIncidentReference)
	input.Reason = strings.TrimSpace(input.Reason)
	if input.ExpectedVersion <= 0 || !statusRefPattern.MatchString(input.InternalStatusBoardIncidentReference) ||
		len(input.Reason) < 10 || len(input.Reason) > 1000 {
		return Incident{}, problem.New(400, "incident_status_board_binding_invalid", "Internal Status Board binding requires a version, bounded reference and reason.")
	}
	if s.statusBoardOrigin == "" {
		return Incident{}, problem.New(409, "internal_status_board_unconfigured", "An internal Status Board must be configured before binding a broad-impact incident.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		incident, err := s.lock(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		if incident.Version != input.ExpectedVersion || !incident.BroadInternalImpact || incident.InternalStatusBoardIncidentReference != nil ||
			(incident.State == "resolved" || incident.State == "cancelled") ||
			(actorID != incident.IncidentCommanderUserID && actorID != incident.CommunicationsLeadUserID) {
			return problem.New(409, "incident_status_board_binding_conflict", "Incident state, version or role does not allow internal Status Board binding.")
		}
		if err := s.requireActiveOperator(ctx, tx, actorID); err != nil {
			return err
		}
		now := s.now()
		result := tx.Model(&persistence.Stage6Incident{}).
			Where("id = ? AND version = ? AND status_page_incident_reference IS NULL", incident.ID, incident.Version).
			Updates(map[string]any{
				"status_page_incident_reference": input.InternalStatusBoardIncidentReference,
				"version":                        incident.Version + 1, "updated_at": now,
			})
		if result.Error != nil {
			return problem.Wrap(500, "incident_status_board_binding_failed", "Internal Status Board incident could not be bound.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "incident_version_conflict", "Incident changed before internal Status Board binding.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.status_board_bound", ResourceType: "stage6_incident", ResourceID: &incident.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"incidentKey": incident.IncidentKey, "reason": input.Reason},
		})
	})
	if err != nil {
		return Incident{}, err
	}
	return s.get(ctx, incidentID)
}

func (s *Service) AddInternalUpdate(
	ctx context.Context,
	actorID, incidentID uuid.UUID,
	input AddUpdateInput,
	requestID, ipAddress string,
) (Incident, error) {
	input.Kind = strings.TrimSpace(strings.ToLower(input.Kind))
	input.Summary = strings.TrimSpace(input.Summary)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	if input.ExpectedVersion <= 0 || !slices.Contains([]string{"initial", "progress", "resolved"}, input.Kind) ||
		len(input.Summary) < 20 || len(input.Summary) > 2000 || !s.validStatusBoardReference(input.EvidenceReference) ||
		input.PublishedAt.IsZero() {
		return Incident{}, problem.New(400, "incident_internal_update_invalid", "Internal incident update fields or Status Board evidence reference are invalid.")
	}
	if employeeSensitiveAssignment.MatchString(input.Summary) || employeeBearerCredential.MatchString(input.Summary) {
		return Incident{}, problem.New(400, "incident_internal_update_sensitive_content", "Internal incident updates must contain employee-safe content without credentials or private payloads.")
	}
	input.PublishedAt = input.PublishedAt.UTC()
	update := persistence.Stage6IncidentUpdate{
		ID: uuid.New(), IncidentID: incidentID, OperatorTenantID: s.operatorTenantID,
		Kind: input.Kind, Summary: input.Summary, PublishedAt: input.PublishedAt,
		EvidenceReference: input.EvidenceReference, CreatedBy: actorID, CreatedAt: s.now(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		incident, err := s.lock(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		if incident.Version != input.ExpectedVersion || actorID != incident.CommunicationsLeadUserID ||
			!incident.BroadInternalImpact || incident.InternalStatusBoardIncidentReference == nil ||
			incident.State == "resolved" || incident.State == "cancelled" {
			return problem.New(409, "incident_internal_update_conflict", "Incident state, version or role does not allow this internal update.")
		}
		if err := s.requireActiveOperator(ctx, tx, actorID); err != nil {
			return err
		}
		var previous persistence.Stage6IncidentUpdate
		previousErr := tx.Where("incident_id = ?", incident.ID).Order("published_at DESC, id DESC").Take(&previous).Error
		if previousErr != nil && !errors.Is(previousErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "incident_updates_load_failed", "Incident update history could not be loaded.", previousErr)
		}
		if errors.Is(previousErr, gorm.ErrRecordNotFound) {
			if input.Kind != "initial" {
				return problem.New(409, "incident_initial_update_required", "The first internal incident update must be initial.")
			}
		} else if previous.Kind == "resolved" || input.Kind == "initial" || !input.PublishedAt.After(previous.PublishedAt) {
			return problem.New(409, "incident_internal_update_order_invalid", "Internal incident updates must be append-only and chronologically ordered.")
		}
		if input.Kind == "resolved" && incident.State != "monitoring" {
			return problem.New(409, "incident_not_monitoring", "A resolved internal update requires the incident to be monitoring.")
		}
		now := s.now()
		if input.PublishedAt.Before(incident.ImpactConfirmedAt) || input.PublishedAt.After(now.Add(5*time.Minute)) {
			return problem.New(400, "incident_internal_update_time_invalid", "Internal update time is outside the incident timeline.")
		}
		if err := tx.Create(&update).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "incident_internal_update_exists", "This internal incident update already exists.")
		} else if err != nil {
			return problem.Wrap(500, "incident_internal_update_create_failed", "Internal incident update could not be recorded.", err)
		}
		if err := enqueueInternalUpdate(ctx, tx, incident, update); err != nil {
			return problem.Wrap(500, "incident_internal_notification_enqueue_failed", "The internal incident notification could not be queued.", err)
		}
		var nextDue *time.Time
		if input.Kind != "resolved" {
			value := input.PublishedAt.Add(updateCadence(incident.Severity))
			nextDue = &value
		}
		result := tx.Model(&persistence.Stage6Incident{}).
			Where("id = ? AND version = ? AND state = ?", incident.ID, incident.Version, incident.State).
			Updates(map[string]any{"next_public_update_due_at": nextDue, "version": incident.Version + 1, "updated_at": now})
		if result.Error != nil {
			return problem.Wrap(500, "incident_internal_update_projection_failed", "Internal incident cadence could not be advanced.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "incident_version_conflict", "Incident changed before the internal update committed.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.internal_update_recorded", ResourceType: "stage6_incident", ResourceID: &incident.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"incidentKey": incident.IncidentKey, "kind": update.Kind,
				"publishedAt": update.PublishedAt, "evidenceReference": update.EvidenceReference,
			},
		})
	})
	if err != nil {
		return Incident{}, err
	}
	return s.get(ctx, incidentID)
}

// enqueueInternalUpdate records a durable, employee-safe notification intent in the
// same transaction as the immutable Incident update. It deliberately does not
// call a Status Board, paging provider or employee channel; the external Publisher
// owns delivery and must use the Outbox message ID as its idempotency key.
func enqueueInternalUpdate(ctx context.Context, tx *gorm.DB, incident persistence.Stage6Incident, update persistence.Stage6IncidentUpdate) error {
	if incident.InternalStatusBoardOrigin == nil || incident.InternalStatusBoardIncidentReference == nil {
		return errors.New("internal Status Board binding is required before enqueueing an incident update")
	}
	return outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
		TenantID:    &incident.OperatorTenantID,
		Topic:       internalIncidentUpdateTopic,
		MessageKey:  fmt.Sprintf("incident:%s:update:%s", incident.ID, update.ID),
		AvailableAt: update.PublishedAt,
		CreatedAt:   update.CreatedAt,
		Headers: map[string]any{
			"eventVersion": 1,
			"contentType":  "application/vnd.synara.internal-incident-update+json",
			"delivery":     "employee-safe-internal-notification",
		},
		Payload: map[string]any{
			"eventType":                    "incident.internal_update",
			"incidentId":                   incident.ID.String(),
			"incidentKey":                  incident.IncidentKey,
			"severity":                     incident.Severity,
			"kind":                         update.Kind,
			"summary":                      update.Summary,
			"publishedAt":                  update.PublishedAt.UTC().Format(time.RFC3339Nano),
			"statusBoardOrigin":            *incident.InternalStatusBoardOrigin,
			"statusBoardIncidentReference": *incident.InternalStatusBoardIncidentReference,
			"statusBoardEvidenceReference": update.EvidenceReference,
			"affectedComponents":           slices.Clone(incident.AffectedComponents),
			"affectedRegions":              slices.Clone(incident.AffectedRegions),
			"securityPrivacyImpact":        incident.SecurityPrivacyImpact,
		},
	})
}

func (s *Service) RecordResolutionApproval(
	ctx context.Context,
	actorID, incidentID uuid.UUID,
	input ResolutionApprovalInput,
	requestID, ipAddress string,
) (Incident, error) {
	input.Decision = strings.TrimSpace(strings.ToLower(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains([]string{"approved", "rejected"}, input.Decision) || len(input.Reason) < 20 ||
		len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validApprovalEvidenceSHA256(input.EvidenceSHA256) {
		return Incident{}, problem.New(400, "incident_resolution_approval_invalid", "Resolution approval fields are invalid.")
	}
	approval := persistence.Stage6IncidentResolutionApproval{
		ID: uuid.New(), IncidentID: incidentID, OperatorTenantID: s.operatorTenantID,
		Decision: input.Decision, Reason: input.Reason, EvidenceReference: input.EvidenceReference,
		EvidenceSHA256: &input.EvidenceSHA256,
		ApproverUserID: actorID, CreatedAt: s.now(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		incident, err := s.lock(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		if !incident.SecurityPrivacyImpact || incident.SecurityPrivacyLeadUserID == nil ||
			actorID != *incident.SecurityPrivacyLeadUserID || incident.State != "monitoring" {
			return problem.New(409, "incident_resolution_approval_forbidden", "Only the assigned Security/Privacy lead may decide resolution while monitoring.")
		}
		if err := s.requireActiveOperator(ctx, tx, actorID); err != nil {
			return err
		}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "incident_resolution_approval_exists", "Resolution approval is already immutable.")
		} else if err != nil {
			return problem.Wrap(500, "incident_resolution_approval_failed", "Resolution approval could not be recorded.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.resolution_approval_recorded", ResourceType: "stage6_incident", ResourceID: &incident.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"incidentKey": incident.IncidentKey, "decision": approval.Decision,
				"evidenceReference": approval.EvidenceReference, "evidenceSha256": input.EvidenceSHA256,
			},
		})
	})
	if err != nil {
		return Incident{}, err
	}
	return s.get(ctx, incidentID)
}

func (s *Service) Transition(
	ctx context.Context,
	actorID, incidentID uuid.UUID,
	input TransitionInput,
	requestID, ipAddress string,
) (Incident, error) {
	input.TargetState = strings.TrimSpace(strings.ToLower(input.TargetState))
	input.Reason = strings.TrimSpace(input.Reason)
	if input.ExpectedVersion <= 0 || !slices.Contains([]string{"identified", "monitoring", "resolved", "cancelled"}, input.TargetState) ||
		len(input.Reason) < 10 || len(input.Reason) > 1000 {
		return Incident{}, problem.New(400, "incident_transition_invalid", "Incident transition fields are invalid.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		incident, err := s.lock(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		if incident.Version != input.ExpectedVersion || actorID != incident.IncidentCommanderUserID ||
			!validTransition(incident.State, input.TargetState) {
			return problem.New(409, "incident_version_conflict", "Incident state, version or commander authority changed.")
		}
		if err := s.requireActiveOperator(ctx, tx, actorID); err != nil {
			return err
		}
		if input.TargetState == "cancelled" {
			var count int64
			if err := tx.Model(&persistence.Stage6IncidentUpdate{}).Where("incident_id = ?", incident.ID).Count(&count).Error; err != nil {
				return problem.Wrap(500, "incident_updates_load_failed", "Incident update history could not be loaded.", err)
			}
			if count != 0 {
				return problem.New(409, "communicated_incident_cannot_cancel", "An incident with internal communication history must be resolved, not cancelled.")
			}
		}
		if input.TargetState == "resolved" {
			if incident.BroadInternalImpact {
				var count int64
				if err := tx.Model(&persistence.Stage6IncidentUpdate{}).
					Where("incident_id = ? AND update_kind = ?", incident.ID, "resolved").Count(&count).Error; err != nil {
					return problem.Wrap(500, "incident_updates_load_failed", "Incident resolution update could not be verified.", err)
				}
				if count != 1 {
					return problem.New(409, "incident_resolution_update_required", "Broad internal incident resolution requires an immutable resolved Status Board update.")
				}
			}
			if incident.SecurityPrivacyImpact {
				var approval persistence.Stage6IncidentResolutionApproval
				if err := tx.Where("incident_id = ? AND decision = ? AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL AND evidence_sha256 <> ?", incident.ID, "approved", "sha256:"+strings.Repeat("0", 64)).Take(&approval).Error; errors.Is(err, gorm.ErrRecordNotFound) {
					return problem.New(409, "incident_security_approval_required", "Security/Privacy approval is required before resolution.")
				} else if err != nil {
					return problem.Wrap(500, "incident_resolution_approval_load_failed", "Resolution approval could not be verified.", err)
				}
			}
		}
		now := s.now()
		updates := map[string]any{"state": input.TargetState, "version": incident.Version + 1, "updated_at": now}
		if input.TargetState == "resolved" {
			updates["resolved_at"] = now
			updates["next_public_update_due_at"] = nil
		} else if input.TargetState == "cancelled" {
			updates["cancelled_at"] = now
			updates["next_public_update_due_at"] = nil
		}
		result := tx.Model(&persistence.Stage6Incident{}).
			Where("id = ? AND version = ? AND state = ?", incident.ID, incident.Version, incident.State).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "incident_transition_failed", "Incident could not be transitioned.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "incident_version_conflict", "Incident changed before transition.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "incident.transitioned", ResourceType: "stage6_incident", ResourceID: &incident.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"incidentKey": incident.IncidentKey, "from": incident.State,
				"to": input.TargetState, "reason": input.Reason,
			},
		})
	})
	if err != nil {
		return Incident{}, err
	}
	return s.get(ctx, incidentID)
}

func (s *Service) normalizeCreate(actorID uuid.UUID, input CreateInput) (persistence.Stage6Incident, error) {
	now := s.now()
	input.IncidentKey = strings.TrimSpace(input.IncidentKey)
	input.Severity = strings.ToUpper(strings.TrimSpace(input.Severity))
	input.Title = strings.TrimSpace(input.Title)
	input.InternalImpactSummary = strings.TrimSpace(input.InternalImpactSummary)
	if actorID == uuid.Nil || s.operatorTenantID == uuid.Nil || !incidentKeyPattern.MatchString(input.IncidentKey) ||
		!slices.Contains(severities, input.Severity) || len(input.Title) < 10 || len(input.Title) > 200 ||
		len(input.InternalImpactSummary) < 20 || len(input.InternalImpactSummary) > 2000 ||
		input.CommunicationsLeadUserID == uuid.Nil || actorID == input.CommunicationsLeadUserID {
		return persistence.Stage6Incident{}, problem.New(400, "incident_invalid", "Incident identity, content or separated role assignment is invalid.")
	}
	if (input.Severity == "SEV-0" || input.Severity == "SEV-1") && !input.BroadInternalImpact {
		return persistence.Stage6Incident{}, problem.New(400, "incident_broad_internal_impact_required", "SEV-0 and SEV-1 incidents require broad internal impact handling.")
	}
	if input.BroadInternalImpact && s.statusBoardOrigin == "" {
		return persistence.Stage6Incident{}, problem.New(409, "internal_status_board_unconfigured", "An internal Status Board must be configured before opening a broad-impact incident.")
	}
	if input.SecurityPrivacyImpact {
		if input.SecurityPrivacyLeadUserID == nil || *input.SecurityPrivacyLeadUserID == uuid.Nil || *input.SecurityPrivacyLeadUserID == actorID {
			return persistence.Stage6Incident{}, problem.New(400, "incident_security_lead_required", "Security/privacy impact requires a lead separated from the incident commander.")
		}
	} else if input.SecurityPrivacyLeadUserID != nil {
		return persistence.Stage6Incident{}, problem.New(400, "incident_security_lead_unexpected", "Security/Privacy lead is only valid for a declared security/privacy impact.")
	}
	affectedComponents, err := normalizeSet(input.AffectedComponents, 1, 6, func(value string) bool {
		return slices.Contains(components, value)
	})
	if err != nil {
		return persistence.Stage6Incident{}, problem.New(400, "incident_components_invalid", "Affected internal service components are invalid.")
	}
	affectedRegions, err := normalizeSet(input.AffectedRegions, 1, 32, regionPattern.MatchString)
	if err != nil {
		return persistence.Stage6Incident{}, problem.New(400, "incident_regions_invalid", "Affected Regions are invalid.")
	}
	started := input.StartedAt.UTC()
	confirmed := input.ImpactConfirmedAt.UTC()
	if started.IsZero() || confirmed.IsZero() || started.After(confirmed) || confirmed.After(now.Add(5*time.Minute)) || started.Before(now.Add(-365*24*time.Hour)) {
		return persistence.Stage6Incident{}, problem.New(400, "incident_timeline_invalid", "Incident timestamps are invalid or outside the bounded history window.")
	}
	var firstDue, nextDue *time.Time
	var statusBoardOrigin *string
	if input.BroadInternalImpact {
		origin := s.statusBoardOrigin
		statusBoardOrigin = &origin
		value := confirmed.Add(firstInternalUpdateTarget(input.Severity))
		firstDue = &value
		next := value
		nextDue = &next
	}
	return persistence.Stage6Incident{
		ID: uuid.New(), OperatorTenantID: s.operatorTenantID, IncidentKey: input.IncidentKey,
		Severity: input.Severity, State: "investigating", Title: input.Title,
		InternalImpactSummary: input.InternalImpactSummary, BroadInternalImpact: input.BroadInternalImpact,
		SecurityPrivacyImpact: input.SecurityPrivacyImpact, InternalStatusBoardOrigin: statusBoardOrigin,
		AffectedComponents: affectedComponents,
		AffectedRegions:    affectedRegions, IncidentCommanderUserID: actorID,
		CommunicationsLeadUserID:  input.CommunicationsLeadUserID,
		SecurityPrivacyLeadUserID: input.SecurityPrivacyLeadUserID,
		StartedAt:                 started, ImpactConfirmedAt: confirmed, FirstInternalUpdateDueAt: firstDue,
		NextInternalUpdateDueAt: nextDue, Version: 1, CreatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Incident, error) {
	var model persistence.Stage6Incident
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Incident{}, problem.New(404, "incident_not_found", "Incident was not found.")
	} else if err != nil {
		return Incident{}, problem.Wrap(500, "incident_load_failed", "Incident could not be loaded.", err)
	}
	return s.view(ctx, s.db, model)
}

func (s *Service) lock(ctx context.Context, tx *gorm.DB, id uuid.UUID) (persistence.Stage6Incident, error) {
	var model persistence.Stage6Incident
	query := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "")
	if err := query.Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.Stage6Incident{}, problem.New(404, "incident_not_found", "Incident was not found.")
	} else if err != nil {
		return persistence.Stage6Incident{}, problem.Wrap(500, "incident_load_failed", "Incident could not be loaded.", err)
	}
	return model, nil
}

func (s *Service) view(ctx context.Context, db *gorm.DB, model persistence.Stage6Incident) (Incident, error) {
	var updates []persistence.Stage6IncidentUpdate
	if err := db.WithContext(ctx).Where("incident_id = ?", model.ID).Order("published_at, id").Find(&updates).Error; err != nil {
		return Incident{}, problem.Wrap(500, "incident_updates_load_failed", "Incident updates could not be loaded.", err)
	}
	var approvalModels []persistence.Stage6IncidentResolutionApproval
	if err := db.WithContext(ctx).Where("incident_id = ?", model.ID).Order("created_at, id").Find(&approvalModels).Error; err != nil {
		return Incident{}, problem.Wrap(500, "incident_resolution_approval_load_failed", "Incident resolution approvals could not be loaded.", err)
	}
	var notificationModels []persistence.OutboxMessage
	messageKeyPrefix := fmt.Sprintf("incident:%s:update:", model.ID)
	if err := db.WithContext(ctx).
		Where("tenant_id = ? AND topic = ? AND message_key LIKE ?", s.operatorTenantID, internalIncidentUpdateTopic, messageKeyPrefix+"%").
		Order("created_at, id").Find(&notificationModels).Error; err != nil {
		return Incident{}, problem.Wrap(500, "incident_notifications_load_failed", "Incident notification status could not be loaded.", err)
	}
	userIDs := assignedUsers(model)
	for _, update := range updates {
		userIDs = append(userIDs, update.CreatedBy)
	}
	for _, approval := range approvalModels {
		userIDs = append(userIDs, approval.ApproverUserID)
	}
	users, err := loadOperators(ctx, db, userIDs)
	if err != nil {
		return Incident{}, err
	}
	internalUpdates := make([]InternalUpdate, 0, len(updates))
	for _, update := range updates {
		internalUpdates = append(internalUpdates, InternalUpdate{
			ID: update.ID, Kind: update.Kind, Summary: update.Summary, PublishedAt: update.PublishedAt,
			EvidenceReference: update.EvidenceReference, CreatedBy: users[update.CreatedBy], CreatedAt: update.CreatedAt,
		})
	}
	internalNotifications := make([]InternalNotification, 0, len(notificationModels))
	for _, notification := range notificationModels {
		internalNotifications = append(internalNotifications, InternalNotification{
			ID: notification.ID, Kind: notificationKind(notification.Payload), Status: outboxMessageStatus(notification),
			Attempts: notification.Attempts, AvailableAt: notification.AvailableAt,
			PublishedAt: notification.PublishedAt, DeadLetteredAt: notification.DeadLetteredAt,
		})
	}
	var securityLead *Operator
	if model.SecurityPrivacyLeadUserID != nil {
		value := users[*model.SecurityPrivacyLeadUserID]
		securityLead = &value
	}
	var resolution *ResolutionApproval
	resolutionHistory := make([]ResolutionApproval, 0, len(approvalModels))
	for _, approval := range approvalModels {
		item := ResolutionApproval{
			ID: approval.ID, Decision: approval.Decision, Reason: approval.Reason,
			EvidenceReference: approval.EvidenceReference, EvidenceSHA256: approval.EvidenceSHA256,
			SupersededAt: approval.SupersededAt, SupersededReason: approval.SupersededReason,
			Approver: users[approval.ApproverUserID], CreatedAt: approval.CreatedAt,
		}
		resolutionHistory = append(resolutionHistory, item)
		if approval.SupersededAt == nil {
			active := item
			resolution = &active
		}
	}
	return Incident{
		ID: model.ID, IncidentKey: model.IncidentKey, Severity: model.Severity, State: model.State,
		Title: model.Title, InternalImpactSummary: model.InternalImpactSummary,
		BroadInternalImpact: model.BroadInternalImpact, SecurityPrivacyImpact: model.SecurityPrivacyImpact,
		InternalStatusBoardOrigin:            model.InternalStatusBoardOrigin,
		InternalStatusBoardIncidentReference: model.InternalStatusBoardIncidentReference,
		AffectedComponents:                   slices.Clone(model.AffectedComponents), AffectedRegions: slices.Clone(model.AffectedRegions),
		IncidentCommander: users[model.IncidentCommanderUserID], CommunicationsLead: users[model.CommunicationsLeadUserID],
		SecurityPrivacyLead: securityLead, StartedAt: model.StartedAt, ImpactConfirmedAt: model.ImpactConfirmedAt,
		Cadence: cadenceView(model, updates, s.now()), InternalUpdates: internalUpdates, InternalNotifications: internalNotifications,
		ResolutionApproval: resolution, ResolutionApprovals: resolutionHistory,
		ResolvedAt: model.ResolvedAt, CancelledAt: model.CancelledAt, Version: model.Version,
		CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

func notificationKind(payload map[string]any) string {
	value, _ := payload["kind"].(string)
	return value
}

func outboxMessageStatus(message persistence.OutboxMessage) string {
	if message.PublishedAt != nil {
		return "published"
	}
	if message.DeadLetteredAt != nil {
		return "dead-letter"
	}
	if message.Attempts > 0 {
		return "retrying"
	}
	return "pending"
}

func (s *Service) requireActiveOperator(ctx context.Context, db *gorm.DB, userID uuid.UUID) error {
	var count int64
	err := db.WithContext(ctx).Table("tenant_memberships AS membership").
		Joins("JOIN tenants operator_tenant ON operator_tenant.id = membership.tenant_id AND operator_tenant.status = ? AND operator_tenant.deleted_at IS NULL", "active").
		Joins("JOIN users operator_user ON operator_user.id = membership.user_id AND operator_user.status = ? AND operator_user.deleted_at IS NULL", "active").
		Where("membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ? AND membership.role IN ?", s.operatorTenantID, userID, "active", []string{"owner", "admin", "security_admin"}).
		Count(&count).Error
	if err != nil {
		return problem.Wrap(500, "incident_operator_authorization_failed", "Incident operator authority could not be verified.", err)
	}
	if count != 1 {
		return problem.New(409, "incident_operator_inactive", "Every assigned incident role requires an active Platform Operator membership.")
	}
	return nil
}

func assignedUsers(model persistence.Stage6Incident) []uuid.UUID {
	result := []uuid.UUID{model.IncidentCommanderUserID, model.CommunicationsLeadUserID}
	if model.SecurityPrivacyLeadUserID != nil {
		result = append(result, *model.SecurityPrivacyLeadUserID)
	}
	return result
}

func loadOperators(ctx context.Context, db *gorm.DB, ids []uuid.UUID) (map[uuid.UUID]Operator, error) {
	ids = slices.Compact(ids)
	var users []persistence.User
	if err := db.WithContext(ctx).Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, problem.Wrap(500, "incident_operators_load_failed", "Incident operators could not be loaded.", err)
	}
	result := make(map[uuid.UUID]Operator, len(users))
	for _, user := range users {
		result[user.ID] = Operator{UserID: user.ID, Email: user.Email, DisplayName: user.DisplayName}
	}
	return result, nil
}

func normalizeSet(values []string, minimum, maximum int, valid func(string) bool) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if !valid(value) {
			return nil, errors.New("invalid set item")
		}
		if _, exists := seen[value]; exists {
			return nil, errors.New("duplicate set item")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) < minimum || len(result) > maximum {
		return nil, errors.New("set size outside bounds")
	}
	slices.Sort(result)
	return result, nil
}

func firstInternalUpdateTarget(severity string) time.Duration {
	if severity == "SEV-0" || severity == "SEV-1" {
		return 15 * time.Minute
	}
	if severity == "SEV-2" {
		return 30 * time.Minute
	}
	return time.Hour
}

func updateCadence(severity string) time.Duration {
	if severity == "SEV-0" || severity == "SEV-1" {
		return 30 * time.Minute
	}
	return time.Hour
}

func cadenceView(model persistence.Stage6Incident, updates []persistence.Stage6IncidentUpdate, now time.Time) Cadence {
	if !model.BroadInternalImpact {
		return Cadence{Status: "not-broad"}
	}
	result := Cadence{FirstInternalUpdateTargetAt: model.FirstInternalUpdateDueAt, NextInternalUpdateTargetAt: model.NextInternalUpdateDueAt}
	if model.State == "resolved" {
		result.Status = "resolved"
	} else if model.InternalStatusBoardIncidentReference == nil {
		result.Status = "awaiting-status-board"
	} else if len(updates) == 0 {
		result.Status = "awaiting-initial"
	} else {
		result.Status = "on-time"
	}
	if len(updates) > 0 && model.FirstInternalUpdateDueAt != nil {
		within := !updates[0].PublishedAt.After(*model.FirstInternalUpdateDueAt)
		result.FirstInternalUpdateWithinTarget = &within
	}
	if result.Status != "resolved" && model.NextInternalUpdateDueAt != nil && now.After(*model.NextInternalUpdateDueAt) {
		result.Status = "overdue"
		result.OverdueSeconds = int64(now.Sub(*model.NextInternalUpdateDueAt).Seconds())
	}
	return result
}

func validTransition(from, to string) bool {
	return (from == "investigating" && (to == "identified" || to == "cancelled")) ||
		(from == "identified" && (to == "monitoring" || to == "cancelled")) ||
		(from == "monitoring" && to == "resolved")
}

func normalizedOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return parsed.Scheme + "://" + strings.ToLower(parsed.Host)
}

func (s *Service) validStatusBoardReference(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return false
	}
	return s.statusBoardOrigin != "" && parsed.Scheme+"://"+strings.ToLower(parsed.Host) == s.statusBoardOrigin &&
		strings.HasPrefix(raw, s.statusBoardOrigin+"/")
}

func validHTTPSReference(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && len(raw) <= 2048
}

func validApprovalEvidenceSHA256(raw string) bool {
	return approvalEvidenceSHA256Pattern.MatchString(raw) && raw != "sha256:"+strings.Repeat("0", 64)
}

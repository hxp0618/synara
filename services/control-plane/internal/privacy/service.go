package privacy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var publicTransitions = map[string]map[string]struct{}{
	"requested": {"verified": {}, "denied": {}, "cancelled": {}},
	"verified":  {"approved": {}, "denied": {}, "cancelled": {}},
	"approved":  {"cancelled": {}},
	"failed":    {"approved": {}, "denied": {}},
}

type Request struct {
	ID                   uuid.UUID      `json:"id"`
	TenantID             uuid.UUID      `json:"tenantId"`
	SubjectUserID        uuid.UUID      `json:"subjectUserId"`
	RequestType          string         `json:"requestType"`
	Status               string         `json:"status"`
	Version              int64          `json:"version"`
	RequestedBy          uuid.UUID      `json:"requestedBy"`
	IntakeReason         string         `json:"intakeReason"`
	DueAt                time.Time      `json:"dueAt"`
	LastTransitionBy     uuid.UUID      `json:"lastTransitionBy"`
	LastTransitionReason string         `json:"lastTransitionReason"`
	CompletedAt          *time.Time     `json:"completedAt"`
	ResultSummary        map[string]any `json:"resultSummary"`
	ResultDigestSHA256   *string        `json:"resultDigestSha256"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
	Events               []Event        `json:"events,omitempty"`
}

type Event struct {
	ID          uuid.UUID      `json:"id"`
	Version     int64          `json:"version"`
	FromStatus  *string        `json:"fromStatus"`
	ToStatus    string         `json:"toStatus"`
	ActorUserID uuid.UUID      `json:"actorUserId"`
	Reason      string         `json:"reason"`
	Metadata    map[string]any `json:"metadata"`
	OccurredAt  time.Time      `json:"occurredAt"`
}

type CreateInput struct {
	SubjectUserID *uuid.UUID `json:"subjectUserId"`
	RequestType   string     `json:"requestType"`
	Reason        string     `json:"reason"`
}

type TransitionInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	ToStatus        string `json:"toStatus"`
	Reason          string `json:"reason"`
}

type ExecuteInput struct {
	ExpectedVersion int64 `json:"expectedVersion"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	artifacts  *artifacts.Service
	now        func() time.Time
}

func NewService(db *gorm.DB, artifactService *artifacts.Service) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), artifacts: artifactService,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) ([]Request, error) {
	role, err := s.requireTenant(ctx, principal, tenantID)
	if err != nil {
		return nil, err
	}
	query := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if !authorization.TenantAllows(role, authorization.RetentionManage) {
		query = query.Where("subject_user_id = ?", principal.UserID)
	}
	var models []persistence.PrivacyRequest
	if err := query.Order("CASE status WHEN 'processing' THEN 0 WHEN 'approved' THEN 1 WHEN 'verified' THEN 2 WHEN 'requested' THEN 3 ELSE 4 END, due_at, created_at, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "privacy_requests_load_failed", "Privacy Requests could not be loaded.", err)
	}
	items := make([]Request, 0, len(models))
	for _, model := range models {
		items = append(items, toRequest(model))
	}
	return items, nil
}

func (s *Service) Get(
	ctx context.Context,
	principal identity.Principal,
	tenantID, requestID uuid.UUID,
) (Request, error) {
	model, _, err := s.authorizedRequest(ctx, principal, tenantID, requestID)
	if err != nil {
		return Request{}, err
	}
	var eventModels []persistence.PrivacyRequestEvent
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND privacy_request_id = ?", tenantID, requestID).
		Order("version").Find(&eventModels).Error; err != nil {
		return Request{}, problem.Wrap(500, "privacy_request_history_load_failed", "Privacy Request history could not be loaded.", err)
	}
	result := toRequest(model)
	result.Events = make([]Event, 0, len(eventModels))
	for _, event := range eventModels {
		result.Events = append(result.Events, toEvent(event))
	}
	return result, nil
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (Request, error) {
	role, err := s.requireTenant(ctx, principal, tenantID)
	if err != nil {
		return Request{}, err
	}
	subjectUserID := principal.UserID
	if input.SubjectUserID != nil {
		subjectUserID = *input.SubjectUserID
	}
	if subjectUserID == uuid.Nil {
		return Request{}, problem.New(400, "invalid_privacy_subject", "Privacy Request subjectUserId is invalid.")
	}
	if subjectUserID != principal.UserID && !authorization.TenantAllows(role, authorization.RetentionManage) {
		return Request{}, problem.New(403, "privacy_request_subject_forbidden", "Only a Tenant privacy administrator can submit for another user.")
	}
	requestType := strings.ToLower(strings.TrimSpace(input.RequestType))
	if requestType != "access_export" && requestType != "erasure" {
		return Request{}, problem.New(400, "invalid_privacy_request_type", "requestType must be access_export or erasure.")
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Request{}, err
	}
	now := s.now()
	model := persistence.PrivacyRequest{
		ID: uuid.New(), TenantID: tenantID, SubjectUserID: subjectUserID,
		RequestType: requestType, Status: "requested", Version: 1,
		RequestedBy: principal.UserID, IntakeReason: reason, DueAt: now.AddDate(0, 0, 30),
		LastTransitionBy: principal.UserID, LastTransitionReason: reason,
		ResultSummary: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "privacy_request_create_rejected", "Privacy Request could not be created; verify the subject and active request uniqueness.", err)
		}
		if err := createEvent(ctx, tx, model, nil, principal.UserID, reason, map[string]any{"requestType": requestType}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "privacy_request.created", ResourceType: "privacy_request", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"subjectUserId": subjectUserID, "requestType": requestType,
				"dueAt": model.DueAt, "reason": reason, "version": model.Version,
			},
		})
	})
	if err != nil {
		return Request{}, err
	}
	return toRequest(model), nil
}

func (s *Service) Transition(
	ctx context.Context,
	principal identity.Principal,
	tenantID, privacyRequestID uuid.UUID,
	input TransitionInput,
	requestID, ipAddress string,
) (Request, error) {
	model, role, err := s.authorizedRequest(ctx, principal, tenantID, privacyRequestID)
	if err != nil {
		return Request{}, err
	}
	toStatus := strings.ToLower(strings.TrimSpace(input.ToStatus))
	if _, allowed := publicTransitions[model.Status][toStatus]; !allowed {
		return Request{}, problem.New(409, "invalid_privacy_request_transition", "The requested Privacy Request transition is not allowed.")
	}
	manager := authorization.TenantAllows(role, authorization.RetentionManage)
	if toStatus != "cancelled" && !manager {
		return Request{}, problem.New(403, "privacy_request_transition_forbidden", "Tenant privacy administrator access is required.")
	}
	if toStatus == "cancelled" && !manager && model.SubjectUserID != principal.UserID {
		return Request{}, problem.New(403, "privacy_request_transition_forbidden", "Only the subject or a Tenant privacy administrator can cancel this request.")
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Request{}, err
	}
	updated, err := s.transition(ctx, tenantID, privacyRequestID, input.ExpectedVersion, model.Status,
		toStatus, principal.UserID, reason, map[string]any{}, nil, requestID, ipAddress)
	if err != nil {
		return Request{}, err
	}
	return toRequest(updated), nil
}

func (s *Service) transition(
	ctx context.Context,
	tenantID, requestID uuid.UUID,
	expectedVersion int64,
	fromStatus, toStatus string,
	actorUserID uuid.UUID,
	reason string,
	resultSummary map[string]any,
	resultDigest *string,
	httpRequestID, ipAddress string,
) (persistence.PrivacyRequest, error) {
	var model persistence.PrivacyRequest
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var err error
		model, err = transitionLocked(ctx, tx, tenantID, requestID, expectedVersion, fromStatus,
			toStatus, actorUserID, reason, resultSummary, resultDigest)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &actorUserID,
			Action: "privacy_request.transitioned", ResourceType: "privacy_request", ResourceID: &requestID,
			RequestID: httpRequestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"subjectUserId": model.SubjectUserID, "requestType": model.RequestType,
				"fromStatus": fromStatus, "toStatus": toStatus, "reason": reason, "version": model.Version,
			},
		})
	})
	return model, err
}

func transitionLocked(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, requestID uuid.UUID,
	expectedVersion int64,
	fromStatus, toStatus string,
	actorUserID uuid.UUID,
	reason string,
	resultSummary map[string]any,
	resultDigest *string,
) (persistence.PrivacyRequest, error) {
	var model persistence.PrivacyRequest
	loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ?", tenantID, requestID).Take(&model).Error
	if errors.Is(loadErr, gorm.ErrRecordNotFound) {
		return model, problem.New(404, "privacy_request_not_found", "Privacy Request not found.")
	}
	if loadErr != nil {
		return model, problem.Wrap(500, "privacy_request_load_failed", "Privacy Request could not be loaded.", loadErr)
	}
	if expectedVersion < 1 || model.Version != expectedVersion || model.Status != fromStatus {
		return model, problem.New(409, "privacy_request_version_conflict", "Privacy Request changed; reload it before continuing.")
	}
	now := time.Now().UTC()
	if resultSummary == nil {
		resultSummary = map[string]any{}
	}
	var completedAt *time.Time
	if toStatus == "completed" {
		completedAt = &now
	}
	updates := persistence.PrivacyRequest{
		Status: toStatus, Version: model.Version + 1,
		LastTransitionBy: actorUserID, LastTransitionReason: reason,
		ResultSummary: resultSummary, ResultDigestSHA256: resultDigest,
		CompletedAt: completedAt, UpdatedAt: now,
	}
	updated := tx.WithContext(ctx).Model(&persistence.PrivacyRequest{}).
		Where("tenant_id = ? AND id = ? AND status = ? AND version = ?", tenantID, requestID, fromStatus, expectedVersion).
		Select("status", "version", "last_transition_by", "last_transition_reason", "result_summary",
			"result_digest_sha256", "completed_at", "updated_at").Updates(&updates)
	if updated.Error != nil || updated.RowsAffected != 1 {
		return model, problem.Wrap(409, "privacy_request_version_conflict", "Privacy Request changed; reload it before continuing.", updated.Error)
	}
	previous := model.Status
	model.Status = toStatus
	model.Version++
	model.LastTransitionBy = actorUserID
	model.LastTransitionReason = reason
	model.ResultSummary = resultSummary
	model.ResultDigestSHA256 = resultDigest
	model.UpdatedAt = now
	if toStatus == "completed" {
		model.CompletedAt = &now
	} else {
		model.CompletedAt = nil
	}
	if err := createEvent(ctx, tx, model, &previous, actorUserID, reason, resultSummary); err != nil {
		return model, err
	}
	return model, nil
}

func createEvent(
	ctx context.Context,
	tx *gorm.DB,
	request persistence.PrivacyRequest,
	fromStatus *string,
	actorUserID uuid.UUID,
	reason string,
	metadata map[string]any,
) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	event := persistence.PrivacyRequestEvent{
		ID: uuid.New(), TenantID: request.TenantID, PrivacyRequestID: request.ID,
		Version: request.Version, FromStatus: fromStatus, ToStatus: request.Status,
		ActorUserID: actorUserID, Reason: reason, Metadata: metadata, OccurredAt: time.Now().UTC(),
	}
	if err := tx.WithContext(ctx).Create(&event).Error; err != nil {
		return problem.Wrap(409, "privacy_request_event_rejected", "Privacy Request history could not be recorded.", err)
	}
	return nil
}

func (s *Service) authorizedRequest(
	ctx context.Context,
	principal identity.Principal,
	tenantID, requestID uuid.UUID,
) (persistence.PrivacyRequest, string, error) {
	role, err := s.requireTenant(ctx, principal, tenantID)
	if err != nil {
		return persistence.PrivacyRequest{}, "", err
	}
	var model persistence.PrivacyRequest
	err = s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, requestID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model, role, problem.New(404, "privacy_request_not_found", "Privacy Request not found.")
	}
	if err != nil {
		return model, role, problem.Wrap(500, "privacy_request_load_failed", "Privacy Request could not be loaded.", err)
	}
	if model.SubjectUserID != principal.UserID && !authorization.TenantAllows(role, authorization.RetentionManage) {
		return persistence.PrivacyRequest{}, role, problem.New(404, "privacy_request_not_found", "Privacy Request not found.")
	}
	return model, role, nil
}

func (s *Service) requireTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) (string, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return "", err
	}
	return s.authorizer.TenantRole(ctx, principal.UserID, tenantID)
}

func normalizeReason(value string) (string, error) {
	reason := strings.TrimSpace(value)
	if len(reason) < 10 || len(reason) > 2000 {
		return "", problem.New(400, "invalid_privacy_request_reason", "Privacy Request reason must contain between 10 and 2000 characters.")
	}
	return reason, nil
}

func toRequest(model persistence.PrivacyRequest) Request {
	resultSummary := model.ResultSummary
	if resultSummary == nil {
		resultSummary = map[string]any{}
	}
	return Request{
		ID: model.ID, TenantID: model.TenantID, SubjectUserID: model.SubjectUserID,
		RequestType: model.RequestType, Status: model.Status, Version: model.Version,
		RequestedBy: model.RequestedBy, IntakeReason: model.IntakeReason, DueAt: model.DueAt,
		LastTransitionBy: model.LastTransitionBy, LastTransitionReason: model.LastTransitionReason,
		CompletedAt: model.CompletedAt, ResultSummary: resultSummary, ResultDigestSHA256: model.ResultDigestSHA256,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func toEvent(model persistence.PrivacyRequestEvent) Event {
	metadata := model.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return Event{
		ID: model.ID, Version: model.Version, FromStatus: model.FromStatus, ToStatus: model.ToStatus,
		ActorUserID: model.ActorUserID, Reason: model.Reason, Metadata: metadata, OccurredAt: model.OccurredAt,
	}
}

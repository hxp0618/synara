package sessions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	projects   *projects.Service
	targets    *executiontargets.Service
	repository persistence.Repository[persistence.AgentSession]
	events     *eventBroker
}

func NewService(db *gorm.DB, projectService *projects.Service, targetService *executiontargets.Service) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), projects: projectService,
		targets:    targetService,
		repository: persistence.NewRepository[persistence.AgentSession](db),
		events:     newEventBroker(),
	}
}

func ActiveTenant(principal identity.Principal) (uuid.UUID, error) {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID == uuid.Nil {
		return uuid.Nil, problem.New(409, "active_tenant_required", "Select an active tenant before accessing tenant resources.")
	}
	return *principal.ActiveTenantID, nil
}

func toSession(model persistence.AgentSession) Session {
	return Session{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		ProjectID: model.ProjectID, CreatedBy: model.CreatedBy, Title: model.Title,
		Status: model.Status, Visibility: model.Visibility, Provider: model.Provider,
		Model: model.Model, ProviderCredentialID: model.ProviderCredentialID, ExecutionTargetID: model.ExecutionTargetID,
		LastEventSequence: model.LastEventSequence, CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt, ArchivedAt: model.ArchivedAt,
	}
}

func toTurn(model persistence.AgentTurn) Turn {
	return Turn{
		ID: model.ID, TenantID: model.TenantID, SessionID: model.SessionID,
		CreatedBy: model.CreatedBy, Status: model.Status, InputText: model.InputText,
		StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, CreatedAt: model.CreatedAt,
	}
}

func toEvent(model persistence.SessionEvent) Event {
	payload := model.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return Event{
		EventID: model.EventID, EventVersion: model.EventVersion, TenantID: model.TenantID,
		OrganizationID: model.OrganizationID, ProjectID: model.ProjectID, SessionID: model.SessionID,
		ExecutionID: model.ExecutionID, WorkerID: model.WorkerID, Generation: model.Generation,
		Sequence: model.Sequence, EventType: model.EventType, ActorType: model.ActorType,
		ActorID: model.ActorID, Payload: payload, OccurredAt: model.OccurredAt,
	}
}

func normalizeSessionVisibility(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "private"
	}
	if value != "private" && value != "project" && value != "organization" {
		return "", problem.New(400, "invalid_session_visibility", "Session visibility must be private, project, or organization.")
	}
	return value, nil
}

func normalizeProvider(value string) (string, error) {
	return validation.Code(value, "codex", "invalid_provider", "Provider")
}

func normalizeModel(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil, nil
	}
	if len(normalized) > 200 || strings.ContainsAny(normalized, "\r\n\t") {
		return nil, problem.New(400, "invalid_model", "Model is invalid.")
	}
	return &normalized, nil
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	projectID uuid.UUID,
	input CreateSessionInput,
	requestID, ipAddress string,
) (Session, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, err
	}
	project, err := s.projects.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return Session{}, err
	}
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, authorization.SessionCreate); err != nil {
		return Session{}, err
	}
	title, err := validation.Name(input.Title, "invalid_session_title", "Session title", 300)
	if err != nil {
		return Session{}, err
	}
	visibility, err := normalizeSessionVisibility(input.Visibility)
	if err != nil {
		return Session{}, err
	}
	provider, err := normalizeProvider(input.Provider)
	if err != nil {
		return Session{}, err
	}
	modelName, err := normalizeModel(input.Model)
	if err != nil {
		return Session{}, err
	}
	credentialID, err := s.resolveProviderCredential(
		ctx, principal, tenantID, project.OrganizationID, provider, input.ProviderCredentialID,
	)
	if err != nil {
		return Session{}, err
	}

	target, err := s.targets.ResolveForSession(ctx, tenantID, project.OrganizationID, input.ExecutionTargetID)
	if err != nil {
		return Session{}, err
	}

	model := persistence.AgentSession{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: project.OrganizationID,
		ProjectID: project.ID, CreatedBy: principal.UserID, Title: title, Status: "active",
		Visibility: visibility, Provider: provider, Model: modelName,
		ProviderCredentialID: credentialID, ExecutionTargetID: target.ID,
	}
	var createdEvent persistence.SessionEvent
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "session_create_rejected", "Session creation was rejected by a tenant isolation constraint.", err)
		}
		createdEvent, err = appendEvent(ctx, tx, &model, eventInput{
			EventType: "session.created", ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{
				"title": title, "provider": provider, "visibility": visibility,
				"executionTargetId": target.ID, "targetKind": target.Kind,
				"providerCredentialId": credentialID,
			},
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session.created", ResourceType: "agent_session", ResourceID: &model.ID,
			OrganizationID: &project.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"projectId": project.ID, "visibility": visibility, "providerCredentialId": credentialID},
		})
	})
	if err != nil {
		return Session{}, err
	}
	s.events.publish(toEvent(createdEvent))
	return s.Get(ctx, principal, tenantID, model.ID)
}

func (s *Service) resolveProviderCredential(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	provider string,
	credentialID *uuid.UUID,
) (*uuid.UUID, error) {
	if credentialID == nil || *credentialID == uuid.Nil {
		return nil, nil
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, organizationID, authorization.CredentialsUse,
	); err != nil {
		return nil, err
	}
	var credential persistence.ProviderCredential
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "organization_id", "provider", "expires_at", "revoked_at").
		Where("tenant_id = ? AND id = ?", tenantID, *credentialID).
		Take(&credential).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, problem.New(404, "credential_not_found", "Provider Credential not found.")
	}
	if err != nil {
		return nil, problem.Wrap(500, "credential_load_failed", "Provider Credential could not be loaded.", err)
	}
	if credential.OrganizationID != nil && *credential.OrganizationID != organizationID {
		return nil, problem.New(404, "credential_not_found", "Provider Credential not found.")
	}
	if credential.Provider != provider {
		return nil, problem.New(409, "credential_provider_mismatch", "Provider Credential does not match the Agent Session provider.")
	}
	if credential.RevokedAt != nil || (credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now().UTC())) {
		return nil, problem.New(409, "credential_unavailable", "Provider Credential is revoked or expired.")
	}
	value := credential.ID
	return &value, nil
}

func (s *Service) ListByProject(
	ctx context.Context,
	principal identity.Principal,
	projectID uuid.UUID,
) ([]Session, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return nil, err
	}
	project, err := s.projects.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return nil, err
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, authorization.SessionRead)
	if err != nil {
		return nil, err
	}
	query := s.db.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND project_id = ? AND archived_at IS NULL", tenantID, projectID)
	if !authorization.TenantAllows(access.TenantRole, authorization.SessionRead) {
		query = query.Where("visibility <> ? OR created_by = ?", "private", principal.UserID)
	}
	models := make([]persistence.AgentSession, 0)
	if err := query.Order("updated_at DESC, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "sessions_load_failed", "Failed to load sessions.", err)
	}
	items := make([]Session, 0, len(models))
	for _, model := range models {
		items = append(items, toSession(model))
	}
	return items, nil
}

func (s *Service) Get(
	ctx context.Context,
	principal identity.Principal,
	tenantID, sessionID uuid.UUID,
) (Session, error) {
	model, access, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.SessionRead)
	if err != nil {
		return Session{}, err
	}
	if model.Visibility == "private" && model.CreatedBy != principal.UserID && !authorization.TenantAllows(access.TenantRole, authorization.SessionRead) {
		return Session{}, problem.New(404, "session_not_found", "Session not found.")
	}
	return toSession(model), nil
}

func (s *Service) CreateTurn(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input CreateTurnInput,
	requestID, ipAddress string,
) (Turn, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Turn{}, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.ExecutionCreate)
	if err != nil {
		return Turn{}, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Turn{}, problem.New(404, "session_not_found", "Session not found.")
	}
	inputText := strings.TrimSpace(input.InputText)
	if inputText == "" || len(inputText) > 1_000_000 {
		return Turn{}, problem.New(400, "invalid_turn_input", "Turn input must be between 1 and 1000000 characters.")
	}
	queuedAt := time.Now().UTC()
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: tenantID, SessionID: sessionID,
		CreatedBy: principal.UserID, Status: "queued", InputText: inputText,
	}
	var execution persistence.AgentExecution
	var createdEvent persistence.SessionEvent
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id", "status").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if tenant.Status != "active" {
			return problem.New(409, "tenant_suspended", "The tenant is suspended and cannot create new executions.")
		}
		var quota persistence.TenantQuota
		quotaErr := tx.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&quota).Error
		if quotaErr == nil && quota.MaxConcurrentExecutions != nil {
			var activeExecutions int64
			if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND status IN ?", tenantID, []string{"queued", "leased", "running", "recovering"}).
				Count(&activeExecutions).Error; err != nil {
				return problem.Wrap(500, "execution_quota_check_failed", "Failed to check tenant execution quota.", err)
			}
			if activeExecutions >= int64(*quota.MaxConcurrentExecutions) {
				return problem.New(409, "execution_quota_exceeded", "The tenant concurrent execution quota has been reached.")
			}
		} else if quotaErr != nil && !errors.Is(quotaErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "execution_quota_check_failed", "Failed to load tenant execution quota.", quotaErr)
		}
		locked, err := lockActiveSession(ctx, tx, tenantID, sessionID)
		if err != nil {
			return err
		}
		var target persistence.ExecutionTarget
		if err := tx.WithContext(ctx).
			Where("id = ? AND status = ?", locked.ExecutionTargetID, "active").Take(&target).Error; err != nil {
			return problem.Wrap(409, "execution_target_unavailable", "The session execution target is unavailable.", err)
		}
		execution = persistence.AgentExecution{
			ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, TurnID: turn.ID,
			Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
			Generation: 0, RequestedBy: principal.UserID, QueuedAt: queuedAt,
		}
		if err := tx.Create(&turn).Error; err != nil {
			return problem.Wrap(409, "turn_create_rejected", "Turn creation was rejected by a tenant isolation constraint.", err)
		}
		if err := tx.Create(&execution).Error; err != nil {
			return problem.Wrap(409, "execution_create_rejected", "Execution creation was rejected by a tenant isolation constraint.", err)
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "execution.queued", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"executionId": execution.ID, "tenantId": tenantID, "sessionId": sessionID,
				"turnId": turn.ID, "executionTargetId": execution.ExecutionTargetID,
				"targetKind": execution.TargetKind, "attempt": execution.Attempt,
			},
			Headers: map[string]any{"eventVersion": 1}, AvailableAt: queuedAt, CreatedAt: queuedAt,
		}); err != nil {
			return problem.Wrap(409, "execution_outbox_create_rejected", "Execution dispatch could not be queued atomically.", err)
		}
		createdEvent, err = appendEvent(ctx, tx, &locked, eventInput{
			EventType: "turn.created", ActorType: "user", ActorID: &principal.UserID,
			ExecutionID: &execution.ID,
			Payload: map[string]any{
				"turnId": turn.ID, "executionId": execution.ID, "inputText": inputText,
				"status": "queued", "executionTargetId": execution.ExecutionTargetID,
				"targetKind": execution.TargetKind,
			},
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "turn.created", ResourceType: "agent_turn", ResourceID: &turn.ID,
			OrganizationID: &locked.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"sessionId": sessionID, "projectId": locked.ProjectID},
		})
	})
	if err != nil {
		return Turn{}, err
	}
	s.events.publish(toEvent(createdEvent))
	return toTurn(turn), nil
}

func (s *Service) ListEvents(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	afterSequence int64,
	limit int,
) (EventPage, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return EventPage{}, err
	}
	session, err := s.Get(ctx, principal, tenantID, sessionID)
	if err != nil {
		return EventPage{}, err
	}
	if afterSequence < 0 {
		return EventPage{}, problem.New(400, "invalid_event_sequence", "afterSequence must be zero or greater.")
	}
	limit = persistence.NormalizeLimit(limit, 100, 500)
	models := make([]persistence.SessionEvent, 0)
	err = s.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND sequence > ?", tenantID, sessionID, afterSequence).
		Order("sequence").Limit(limit).Find(&models).Error
	if err != nil {
		return EventPage{}, problem.Wrap(500, "session_events_load_failed", "Failed to load session events.", err)
	}
	items := make([]Event, 0, len(models))
	for _, model := range models {
		items = append(items, toEvent(model))
	}
	return EventPage{Items: items, LastSequence: session.LastEventSequence}, nil
}

func (s *Service) Archive(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	requestID, ipAddress string,
) (Session, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.SessionArchive)
	if err != nil {
		return Session{}, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Session{}, problem.New(404, "session_not_found", "Session not found.")
	}
	if current.ArchivedAt != nil {
		return toSession(current), nil
	}
	var archivedEvent persistence.SessionEvent
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		locked, err := lockActiveSession(ctx, tx, tenantID, sessionID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).
			Updates(map[string]any{"status": "archived", "archived_at": now}).Error; err != nil {
			return problem.Wrap(500, "session_archive_failed", "Failed to archive the session.", err)
		}
		locked.Status = "archived"
		locked.ArchivedAt = &now
		archivedEvent, err = appendEvent(ctx, tx, &locked, eventInput{
			EventType: "session.archived", ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{"archivedAt": now},
		})
		if err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "session.archived", MessageKey: sessionID.String(),
			Payload: map[string]any{
				"tenantId": tenantID, "organizationId": locked.OrganizationID,
				"projectId": locked.ProjectID, "sessionId": sessionID, "archivedAt": now,
			},
		}); err != nil {
			return problem.Wrap(500, "session_archive_outbox_failed", "The archived Session event could not be queued.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session.archived", ResourceType: "agent_session", ResourceID: &sessionID,
			OrganizationID: &locked.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Session{}, err
	}
	s.events.publish(toEvent(archivedEvent))
	return s.Get(ctx, principal, tenantID, sessionID)
}

func (s *Service) SubscribeEvents(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) (uuid.UUID, <-chan Event, func(), error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return uuid.Nil, nil, nil, err
	}
	if _, err := s.Get(ctx, principal, tenantID, sessionID); err != nil {
		return uuid.Nil, nil, nil, err
	}
	events, cancel := s.events.subscribe(tenantID, sessionID)
	return tenantID, events, cancel, nil
}

func (s *Service) authorizedModel(
	ctx context.Context,
	principal identity.Principal,
	tenantID, sessionID uuid.UUID,
	permission authorization.Permission,
) (persistence.AgentSession, authorization.OrganizationAccess, error) {
	model, err := s.repository.First(ctx,
		persistence.TenantScope(tenantID),
		func(db *gorm.DB) *gorm.DB { return db.Where("id = ?", sessionID) },
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.AgentSession{}, authorization.OrganizationAccess{}, problem.New(404, "session_not_found", "Session not found.")
	}
	if err != nil {
		return persistence.AgentSession{}, authorization.OrganizationAccess{}, problem.Wrap(500, "session_load_failed", "Failed to load the session.", err)
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, model.OrganizationID, permission)
	if err != nil {
		return persistence.AgentSession{}, authorization.OrganizationAccess{}, err
	}
	return model, access, nil
}

func lockActiveSession(ctx context.Context, tx *gorm.DB, tenantID, sessionID uuid.UUID) (persistence.AgentSession, error) {
	var session persistence.AgentSession
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ? AND status = ? AND archived_at IS NULL", tenantID, sessionID, "active").
		Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.AgentSession{}, problem.New(409, "session_not_active", "Session is not active.")
	}
	if err != nil {
		return persistence.AgentSession{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the session.", err)
	}
	return session, nil
}

type InternalEventInput struct {
	EventID      *uuid.UUID
	EventVersion int
	EventType    string
	ActorType    string
	ActorID      *uuid.UUID
	ExecutionID  *uuid.UUID
	WorkerID     *uuid.UUID
	Generation   *int64
	Payload      map[string]any
	OccurredAt   *time.Time
}

func (s *Service) AppendInternalEvent(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	input InternalEventInput,
) (persistence.SessionEvent, error) {
	var session persistence.AgentSession
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ?", tenantID, sessionID).Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.SessionEvent{}, problem.New(404, "session_not_found", "Session not found.")
	}
	if err != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the session.", err)
	}
	eventID := uuid.New()
	if input.EventID != nil {
		eventID = *input.EventID
	}
	eventVersion := input.EventVersion
	if eventVersion <= 0 {
		eventVersion = 1
	}
	occurredAt := time.Now().UTC()
	if input.OccurredAt != nil {
		occurredAt = input.OccurredAt.UTC()
	}
	return appendEvent(ctx, tx, &session, eventInput{
		EventID: eventID, EventVersion: eventVersion, EventType: input.EventType,
		ActorType: input.ActorType, ActorID: input.ActorID, ExecutionID: input.ExecutionID,
		WorkerID: input.WorkerID, Generation: input.Generation, Payload: input.Payload,
		OccurredAt: occurredAt,
	})
}

func (s *Service) PublishInternalEvent(event persistence.SessionEvent) {
	s.events.publish(toEvent(event))
}

type eventInput struct {
	EventID      uuid.UUID
	EventVersion int
	EventType    string
	ActorType    string
	ActorID      *uuid.UUID
	ExecutionID  *uuid.UUID
	WorkerID     *uuid.UUID
	Generation   *int64
	Payload      map[string]any
	OccurredAt   time.Time
}

func appendEvent(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
	input eventInput,
) (persistence.SessionEvent, error) {
	nextSequence := session.LastEventSequence + 1
	eventID := input.EventID
	if eventID == uuid.Nil {
		eventID = uuid.New()
	}
	eventVersion := input.EventVersion
	if eventVersion <= 0 {
		eventVersion = 1
	}
	occurredAt := input.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	event := persistence.SessionEvent{
		TenantID: session.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: session.ID, Sequence: nextSequence,
		EventID: eventID, EventVersion: eventVersion, EventType: input.EventType,
		ActorType: input.ActorType, ActorID: input.ActorID, ExecutionID: input.ExecutionID,
		WorkerID: input.WorkerID, Generation: input.Generation, Payload: input.Payload,
		OccurredAt: occurredAt,
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	if err := tx.WithContext(ctx).Create(&event).Error; err != nil {
		return persistence.SessionEvent{}, problem.Wrap(409, "session_event_append_rejected", "Session event append was rejected.", err)
	}
	result := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ? AND last_event_sequence = ?", session.TenantID, session.ID, session.LastEventSequence).
		Update("last_event_sequence", nextSequence)
	if result.Error != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "session_event_sequence_update_failed", "Failed to update the session event sequence.", result.Error)
	}
	if result.RowsAffected != 1 {
		return persistence.SessionEvent{}, problem.New(409, "session_event_sequence_conflict", "Session event sequence changed concurrently.")
	}
	session.LastEventSequence = nextSequence
	return event, nil
}

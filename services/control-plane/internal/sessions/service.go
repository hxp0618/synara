package sessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/executionqueue"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/tenantstate"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

var activeSessionExecutionStatuses = []string{"queued", "leased", "running", "waiting-for-approval", "recovering", "suspended"}
var resourceQuotaExecutionStatuses = []string{"queued", "leased", "running", "waiting-for-approval", "recovering"}

const defaultProviderCapabilityHeartbeatTimeout = 90 * time.Second

type ServiceOption func(*Service)

func WithProviderCapabilityHeartbeatTimeout(timeout time.Duration) ServiceOption {
	return func(service *Service) {
		if timeout > 0 {
			service.providerCapabilityHeartbeatTimeout = timeout
		}
	}
}

type LifecyclePolicyResolver interface {
	ResolveForSession(
		context.Context,
		*gorm.DB,
		uuid.UUID,
		uuid.UUID,
		*lifecyclepolicy.Overrides,
	) (lifecyclepolicy.Effective, error)
}

type EventProjector interface {
	ProjectSessionEvent(context.Context, *gorm.DB, persistence.SessionEvent) error
}

func WithEventProjector(projector EventProjector) ServiceOption {
	return func(service *Service) {
		service.eventProjector = projector
	}
}

func WithLifecyclePolicyResolver(resolver LifecyclePolicyResolver) ServiceOption {
	return func(service *Service) {
		service.lifecyclePolicies = resolver
	}
}

type Service struct {
	db                                 *gorm.DB
	authorizer                         *authorization.Authorizer
	projects                           *projects.Service
	targets                            *executiontargets.Service
	repository                         persistence.Repository[persistence.AgentSession]
	events                             *eventBroker
	providerCapabilityHeartbeatTimeout time.Duration
	lifecyclePolicies                  LifecyclePolicyResolver
	eventProjector                     EventProjector
	now                                func() time.Time
	// Package-private deterministic seam for selection-to-commit race tests.
	// Production services leave this nil.
	targetFailoverBeforeCommitValidation func(context.Context, *gorm.DB, routing.SelectRequest, routing.Selection) error
}

func NewService(
	db *gorm.DB,
	projectService *projects.Service,
	targetService *executiontargets.Service,
	options ...ServiceOption,
) *Service {
	service := &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), projects: projectService,
		targets: targetService, repository: persistence.NewRepository[persistence.AgentSession](db),
		events: newEventBroker(), providerCapabilityHeartbeatTimeout: defaultProviderCapabilityHeartbeatTimeout,
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func ActiveTenant(principal identity.Principal) (uuid.UUID, error) {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID == uuid.Nil {
		return uuid.Nil, problem.New(409, "active_tenant_required", "Select an active tenant before accessing tenant resources.")
	}
	return *principal.ActiveTenantID, nil
}

func (s *Service) RequireExecutionQuotaAvailable(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
) error {
	return s.RequireExecutionQuotaAvailableFor(ctx, tx, tenantID, ExecutionQuotaAdmission{QuotaUnits: 1})
}

type ExecutionQuotaAdmission struct {
	ProjectID    uuid.UUID
	SessionID    uuid.UUID
	AutomationID *uuid.UUID
	QuotaUnits   int
}

func (s *Service) RequireExecutionQuotaAvailableFor(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	admission ExecutionQuotaAdmission,
) error {
	if err := s.lockExecutionQuotaAuthority(ctx, tx, tenantID); err != nil {
		return err
	}
	return s.requireExecutionQuotaAdmissionAfterAuthorityLock(ctx, tx, tenantID, admission)
}

func (s *Service) lockExecutionQuotaAuthority(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
) error {
	// Serialize count-based admission on the Tenant row. Locking only the quota
	// row is insufficient because an absent (unlimited) row can be created
	// concurrently, while every admission and quota update already shares this
	// durable Tenant authority.
	var tenant persistence.Tenant
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Select("id").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
		return problem.Wrap(500, "execution_quota_check_failed", "Failed to lock the tenant execution quota authority.", err)
	}
	return nil
}

// requireExecutionQuotaAvailableAfterAuthorityLock performs the count-based
// admission check after lockExecutionQuotaAuthority has established the
// transaction-wide serialization point. Keeping the operations separate lets
// replacement flows lock in the global Tenant-before-Session order and decide
// later whether they consume new capacity.
func (s *Service) requireExecutionQuotaAvailableAfterAuthorityLock(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
) error {
	return s.requireExecutionQuotaAdmissionAfterAuthorityLock(
		ctx, tx, tenantID, ExecutionQuotaAdmission{QuotaUnits: 1},
	)
}

func (s *Service) requireExecutionQuotaAdmissionAfterAuthorityLock(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	admission ExecutionQuotaAdmission,
) error {
	if admission.QuotaUnits == 0 {
		admission.QuotaUnits = 1
	}
	if admission.QuotaUnits < 1 || admission.QuotaUnits > 1_000_000 {
		return problem.New(400, "invalid_execution_quota_units", "Execution quotaUnits must be between 1 and 1000000.")
	}
	var quota persistence.TenantQuota
	quotaErr := tx.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&quota).Error
	if quotaErr != nil && !errors.Is(quotaErr, gorm.ErrRecordNotFound) {
		return problem.Wrap(500, "execution_quota_check_failed", "Failed to load the tenant execution quota.", quotaErr)
	}
	if quotaErr == nil {
		if err := s.enforceExecutionQuotaLimits(
			ctx, tx, tenantID, "tenant", tenantID, admission,
			quota.MaxConcurrentExecutions, quota.MaxQueuedExecutions, quota.MaxConcurrentExecutionUnits,
		); err != nil {
			return err
		}
	}

	scopes := make([]struct {
		kind string
		id   uuid.UUID
	}, 0, 3)
	if admission.ProjectID != uuid.Nil {
		scopes = append(scopes, struct {
			kind string
			id   uuid.UUID
		}{kind: "project", id: admission.ProjectID})
	}
	if admission.SessionID != uuid.Nil {
		scopes = append(scopes, struct {
			kind string
			id   uuid.UUID
		}{kind: "session", id: admission.SessionID})
	}
	if admission.AutomationID != nil && *admission.AutomationID != uuid.Nil {
		scopes = append(scopes, struct {
			kind string
			id   uuid.UUID
		}{kind: "automation", id: *admission.AutomationID})
	}
	for _, scope := range scopes {
		var policy persistence.ExecutionQuotaPolicy
		err := tx.WithContext(ctx).
			Where("tenant_id = ? AND scope_kind = ? AND scope_id = ?", tenantID, scope.kind, scope.id).
			Take(&policy).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return problem.Wrap(500, "execution_quota_check_failed", "Failed to load the scoped execution quota.", err)
		}
		if err := s.enforceExecutionQuotaLimits(
			ctx, tx, tenantID, scope.kind, scope.id, admission,
			policy.MaxConcurrentExecutions, policy.MaxQueuedExecutions, policy.MaxConcurrentExecutionUnits,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) enforceExecutionQuotaLimits(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	scopeKind string,
	scopeID uuid.UUID,
	admission ExecutionQuotaAdmission,
	maxConcurrent *int,
	maxQueued *int,
	maxUnits *int64,
) error {
	if maxConcurrent == nil && maxQueued == nil && maxUnits == nil {
		return nil
	}
	query := tx.WithContext(ctx).Table("agent_executions AS execution").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id").
		Where("execution.tenant_id = ?", tenantID)
	switch scopeKind {
	case "tenant":
	case "project":
		query = query.Where("session.project_id = ?", scopeID)
	case "session":
		query = query.Where("execution.session_id = ?", scopeID)
	case "automation":
		query = query.Where("execution.automation_id = ?", scopeID)
	default:
		return problem.New(500, "execution_quota_scope_invalid", "Execution quota scope is invalid.")
	}
	if maxConcurrent != nil {
		var active int64
		if err := query.Session(&gorm.Session{}).
			Where("execution.status IN ?", resourceQuotaExecutionStatuses).
			Count(&active).Error; err != nil {
			return problem.Wrap(500, "execution_quota_check_failed", "Failed to check concurrent execution quota.", err)
		}
		if active >= int64(*maxConcurrent) {
			apiError := problem.New(409, "execution_quota_exceeded", "The concurrent execution quota has been reached.")
			apiError.Details = map[string]any{"scopeKind": scopeKind, "scopeId": scopeID, "limit": *maxConcurrent}
			return apiError
		}
	}
	if maxQueued != nil {
		var queued int64
		if err := query.Session(&gorm.Session{}).
			Where("execution.status IN ?", []string{"queued", "recovering"}).
			Count(&queued).Error; err != nil {
			return problem.Wrap(500, "execution_queue_quota_check_failed", "Failed to check queued execution quota.", err)
		}
		if queued >= int64(*maxQueued) {
			apiError := problem.New(429, "execution_queue_quota_exceeded", "The queued execution quota has been reached.")
			apiError.Details = map[string]any{"scopeKind": scopeKind, "scopeId": scopeID, "limit": *maxQueued}
			return apiError
		}
	}
	if maxUnits != nil {
		var used struct {
			Units int64 `gorm:"column:units"`
		}
		if err := query.Session(&gorm.Session{}).
			Select("COALESCE(SUM(execution.quota_units), 0) AS units").
			Where("execution.status IN ?", resourceQuotaExecutionStatuses).
			Scan(&used).Error; err != nil {
			return problem.Wrap(500, "execution_unit_quota_check_failed", "Failed to check execution resource-unit quota.", err)
		}
		if used.Units+int64(admission.QuotaUnits) > *maxUnits {
			apiError := problem.New(409, "execution_unit_quota_exceeded", "The concurrent execution resource-unit quota has been reached.")
			apiError.Details = map[string]any{
				"scopeKind": scopeKind, "scopeId": scopeID, "limit": *maxUnits,
				"used": used.Units, "requested": admission.QuotaUnits,
			}
			return apiError
		}
	}
	return nil
}

func toSession(model persistence.AgentSession) Session {
	provider := strings.TrimSpace(model.Provider)
	if strings.EqualFold(provider, "claude") {
		provider = "claudeAgent"
	} else if canonical, valid := executiontargets.CanonicalStage3Provider(provider); valid {
		provider = canonical
	}
	return Session{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		ProjectID: model.ProjectID, CreatedBy: model.CreatedBy, Title: model.Title,
		Status: model.Status, Visibility: model.Visibility, Provider: provider,
		Model: model.Model, ProviderCredentialID: model.ProviderCredentialID, ExecutionTargetID: model.ExecutionTargetID,
		RequestedExecutionTargetID: model.RequestedExecutionTargetID,
		ExecutionTargetGroupID:     model.ExecutionTargetGroupID, RoutingPolicyVersion: model.RoutingPolicyVersion,
		PreferredExecutionRegion: model.PreferredExecutionRegion,
		ForkSourceSessionID:      model.ForkSourceSessionID, ForkSourceTurnID: model.ForkSourceTurnID,
		ForkSourceSequence: model.ForkSourceEventSequence, ForkStrategy: model.ForkStrategy,
		LastEventSequence: model.LastEventSequence, ResourceState: model.ResourceState,
		MeaningfulActivityAt: model.MeaningfulActivityAt,
		ResourceIdleSince:    model.ResourceIdleSince, AbsoluteExpiresAt: model.AbsoluteExpiresAt,
		ResourceLifecyclePolicy: lifecyclepolicy.Effective{
			WaitingKeepAliveSeconds:        model.WaitingKeepAliveSeconds,
			SuspendAfterIdleSeconds:        model.SuspendAfterIdleSeconds,
			AbsoluteSessionLifetimeSeconds: cloneOptionalInt(model.AbsoluteSessionLifetimeSeconds),
			WorkspaceRetentionDays:         model.WorkspaceRetentionDays, WarmPoolMode: model.WarmPoolMode,
		},
		CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt, SettledAt: model.SettledAt, ArchivedAt: model.ArchivedAt,
	}
}

func toTurn(model persistence.AgentTurn) Turn {
	return Turn{
		ID: model.ID, TenantID: model.TenantID, SessionID: model.SessionID,
		CreatedBy: model.CreatedBy, Status: model.Status, InputText: model.InputText,
		TurnKind:    model.TurnKind,
		RuntimeMode: model.RuntimeMode, InteractionMode: model.InteractionMode,
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

func normalizeRuntimeMode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "full-access"
	}
	if value != "approval-required" && value != "full-access" {
		return "", problem.New(400, "invalid_runtime_mode", "runtimeMode must be approval-required or full-access.")
	}
	return value, nil
}

func normalizeInteractionMode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "default"
	}
	if value != "default" && value != "plan" {
		return "", problem.New(400, "invalid_interaction_mode", "interactionMode must be default or plan.")
	}
	return value, nil
}

func normalizeSourceProposedPlan(
	value *SourceProposedPlanReference,
) (*SourceProposedPlanReference, error) {
	if value == nil {
		return nil, nil
	}
	threadID := strings.TrimSpace(value.ThreadID)
	planID := strings.TrimSpace(value.PlanID)
	if threadID == "" || planID == "" || len(threadID) > 300 || len(planID) > 300 ||
		strings.ContainsAny(threadID, "\r\n\t\x00") || strings.ContainsAny(planID, "\r\n\t\x00") {
		return nil, problem.New(
			400,
			"invalid_source_proposed_plan",
			"sourceProposedPlan must contain valid threadId and planId values.",
		)
	}
	return &SourceProposedPlanReference{ThreadID: threadID, PlanID: planID}, nil
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

func normalizePreferredExecutionRegion(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil, nil
	}
	if len(normalized) > 120 || strings.ContainsAny(normalized, "\r\n\t\x00") {
		return nil, problem.New(400, "invalid_preferred_execution_region", "preferredExecutionRegion is invalid.")
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
	item, _, err := s.CreateWithIdempotency(ctx, principal, projectID, input, "", requestID, ipAddress)
	return item, err
}

func (s *Service) CreateWithIdempotency(
	ctx context.Context,
	principal identity.Principal,
	projectID uuid.UUID,
	input CreateSessionInput,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, false, err
	}
	project, err := s.projects.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return Session{}, false, err
	}
	if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, authorization.SessionCreate); err != nil {
		return Session{}, false, err
	}
	title, err := validation.Name(input.Title, "invalid_session_title", "Session title", 300)
	if err != nil {
		return Session{}, false, err
	}
	if identity.IsServiceAccount(principal) && strings.TrimSpace(input.Visibility) == "" {
		input.Visibility = "project"
	}
	visibility, err := normalizeSessionVisibility(input.Visibility)
	if err != nil {
		return Session{}, false, err
	}
	if identity.IsServiceAccount(principal) && visibility == "private" {
		return Session{}, false, problem.New(400, "service_account_private_session_unsupported", "Service Accounts cannot create private Sessions.")
	}
	provider, err := normalizeProvider(input.Provider)
	if err != nil {
		return Session{}, false, err
	}
	modelName, err := normalizeModel(input.Model)
	if err != nil {
		return Session{}, false, err
	}
	requestedCredentialID := normalizeRequestedCredentialID(input.ProviderCredentialID)
	if requestedCredentialID != nil {
		if _, err := s.authorizer.RequireOrganization(
			ctx, principal.UserID, tenantID, project.OrganizationID, authorization.CredentialsUse,
		); err != nil {
			return Session{}, false, err
		}
	}

	var fixedTarget *executiontargets.Binding
	if input.ExecutionTargetGroupID == nil {
		target, err := s.targets.ResolveForSession(ctx, tenantID, project.OrganizationID, input.ExecutionTargetID)
		if err != nil {
			return Session{}, false, err
		}
		fixedTarget = &target
	}
	preferredRegion, err := normalizePreferredExecutionRegion(input.PreferredExecutionRegion)
	if err != nil {
		return Session{}, false, err
	}

	var createdEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session.create", SuccessStatus: 201,
		Request: map[string]any{
			"projectId": projectID, "title": title, "visibility": visibility, "provider": provider,
			"model": modelName, "providerCredentialId": requestedCredentialID,
			"executionTargetId": input.ExecutionTargetID, "executionTargetGroupId": input.ExecutionTargetGroupID,
			"preferredExecutionRegion": preferredRegion,
			"resourceLifecyclePolicy":  input.ResourceLifecyclePolicy,
		},
	}, func(tx *gorm.DB) (Session, error) {
		now := s.now()
		lifecyclePolicy, err := s.resolveLifecyclePolicy(
			ctx, tx, tenantID, project.ID, input.ResourceLifecyclePolicy,
		)
		if err != nil {
			return Session{}, err
		}

		var (
			targetModel      persistence.ExecutionTarget
			globalSelection  *routing.Selection
			launchTargetPlan ExecutionLaunchTarget
		)
		if input.ExecutionTargetGroupID == nil {
			targetErr := tx.WithContext(ctx).
				Where("id = ? AND status = ?", fixedTarget.ID, "active").
				Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, project.OrganizationID).
				Take(&targetModel).Error
			if errors.Is(targetErr, gorm.ErrRecordNotFound) {
				return Session{}, problem.New(409, "execution_target_unavailable", "The selected Execution Target is no longer available.")
			}
			if targetErr != nil {
				return Session{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to reload the selected Execution Target.", targetErr)
			}
		}
		var routeRequest *routing.SelectRequest
		if input.ExecutionTargetGroupID != nil {
			preferredRegions := make([]string, 0, 1)
			if preferredRegion != nil {
				preferredRegions = append(preferredRegions, *preferredRegion)
			}
			request := routing.SelectRequest{
				TenantID: tenantID, OrganizationID: project.OrganizationID,
				TargetGroupID: *input.ExecutionTargetGroupID, Provider: provider,
				PreferredTargetID: input.ExecutionTargetID,
				PreferredRegions:  preferredRegions,
			}
			routeRequest = &request
		}
		launchTargetPlan, err = SelectExecutionLaunchTarget(
			ctx,
			tx,
			func() *persistence.ExecutionTarget {
				if routeRequest != nil {
					return nil
				}
				return &targetModel
			}(),
			routeRequest,
			ExecutionLaunchPolicyScope{
				TenantID: tenantID, OrganizationID: project.OrganizationID, Provider: provider,
			},
			lifecyclePolicy.WarmPoolMode,
			func(
				ctx context.Context,
				tx *gorm.DB,
				target persistence.ExecutionTarget,
				placementSelection placement.Selection,
				selection *routing.Selection,
			) error {
				return s.requireTargetPoolProviderCapabilities(
					ctx,
					tx,
					target,
					placementSelection,
					provider,
					false,
					"start-session",
					"send-turn",
				)
			},
		)
		if err != nil {
			return Session{}, err
		}
		targetModel = launchTargetPlan.Target
		globalSelection = launchTargetPlan.RoutingSelection
		credentialID, err := s.resolveProviderCredentialSelection(
			ctx,
			tx,
			tenantID,
			project.OrganizationID,
			principal.UserID,
			identity.IsServiceAccount(principal),
			provider,
			modelName,
			requestedCredentialID,
		)
		if err != nil {
			return Session{}, err
		}
		var absoluteExpiresAt *time.Time
		if lifecyclePolicy.AbsoluteSessionLifetimeSeconds != nil {
			expiresAt := now.Add(time.Duration(*lifecyclePolicy.AbsoluteSessionLifetimeSeconds) * time.Second)
			absoluteExpiresAt = &expiresAt
		}
		model := persistence.AgentSession{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: project.OrganizationID,
			ProjectID: project.ID, CreatedBy: principal.UserID, Title: title, Status: "active",
			Visibility: visibility, Provider: provider, Model: modelName,
			ProviderCredentialID: credentialID, ExecutionTargetID: targetModel.ID,
			RequestedExecutionTargetID: targetModel.ID, PreferredExecutionRegion: preferredRegion,
			ResourceState: "idle", MeaningfulActivityAt: now, ResourceIdleSince: &now,
			AbsoluteExpiresAt:              absoluteExpiresAt,
			WaitingKeepAliveSeconds:        lifecyclePolicy.WaitingKeepAliveSeconds,
			SuspendAfterIdleSeconds:        lifecyclePolicy.SuspendAfterIdleSeconds,
			AbsoluteSessionLifetimeSeconds: cloneOptionalInt(lifecyclePolicy.AbsoluteSessionLifetimeSeconds),
			WorkspaceRetentionDays:         lifecyclePolicy.WorkspaceRetentionDays,
			WarmPoolMode:                   lifecyclePolicy.WarmPoolMode,
			CreatedAt:                      now,
			UpdatedAt:                      now,
		}
		if globalSelection != nil {
			requestedTargetID := input.ExecutionTargetID
			if requestedTargetID != nil && *requestedTargetID != globalSelection.Target.ID {
				requestedTargetID = nil
			}
			routing.ApplySessionSelection(&model, *globalSelection, requestedTargetID, preferredRegion)
		}
		if err := tx.Create(&model).Error; err != nil {
			return Session{}, problem.Wrap(409, "session_create_rejected", "Session creation was rejected by a tenant isolation constraint.", err)
		}
		if _, err := s.ensureRuntimeResources(ctx, tx, &model); err != nil {
			return Session{}, err
		}
		var selectedRegion any
		var selectedClusterID any
		if globalSelection != nil {
			selectedRegion = globalSelection.Member.Region
			selectedClusterID = globalSelection.Member.ClusterID
		}
		createdEvent, err = s.appendEvent(ctx, tx, &model, eventInput{
			EventType: "session.created", ActorType: "user", ActorID: &principal.UserID,
			Payload: map[string]any{
				"title": title, "provider": provider, "visibility": visibility,
				"executionTargetId": targetModel.ID, "targetKind": targetModel.Kind,
				"executionTargetGroupId": model.ExecutionTargetGroupID,
				"routingPolicyVersion":   model.RoutingPolicyVersion,
				"selectedRegion":         selectedRegion, "selectedClusterId": selectedClusterID,
				"providerCredentialId":    credentialID,
				"resourceLifecyclePolicy": lifecyclePolicy,
			},
		})
		if err != nil {
			return Session{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session.created", ResourceType: "agent_session", ResourceID: &model.ID,
			OrganizationID: &project.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"projectId": project.ID, "visibility": visibility, "providerCredentialId": credentialID},
		}); err != nil {
			return Session{}, err
		}
		return toSession(model), nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if !result.Replayed {
		s.events.publish(toEvent(createdEvent))
	}
	return result.Value, result.Replayed, nil
}

func (s *Service) resolveLifecyclePolicy(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, projectID uuid.UUID,
	override *lifecyclepolicy.Overrides,
) (lifecyclepolicy.Effective, error) {
	if s.lifecyclePolicies != nil {
		return s.lifecyclePolicies.ResolveForSession(ctx, tx, tenantID, projectID, override)
	}
	defaults := lifecyclepolicy.Effective{
		WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800,
		WorkspaceRetentionDays: 30, WarmPoolMode: "disabled",
	}
	if override == nil {
		return defaults, nil
	}
	if override.WaitingKeepAliveSeconds != nil {
		defaults.WaitingKeepAliveSeconds = *override.WaitingKeepAliveSeconds
	}
	if override.SuspendAfterIdleSeconds != nil {
		defaults.SuspendAfterIdleSeconds = *override.SuspendAfterIdleSeconds
	}
	if override.AbsoluteSessionLifetimeSeconds != nil {
		defaults.AbsoluteSessionLifetimeSeconds = cloneOptionalInt(override.AbsoluteSessionLifetimeSeconds)
	}
	if override.WorkspaceRetentionDays != nil {
		defaults.WorkspaceRetentionDays = *override.WorkspaceRetentionDays
	}
	if override.WarmPoolMode != nil {
		defaults.WarmPoolMode = *override.WarmPoolMode
	}
	return defaults, nil
}

func cloneOptionalInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (s *Service) resolveProviderCredentialSelection(
	ctx context.Context,
	db *gorm.DB,
	tenantID, organizationID, sessionOwnerUserID uuid.UUID,
	excludeUserScope bool,
	provider string,
	model *string,
	requestedCredentialID *uuid.UUID,
) (*uuid.UUID, error) {
	selection, err := credentialscope.Resolve(ctx, db, credentialscope.Request{
		TenantID: tenantID, OrganizationID: organizationID, SessionOwnerUserID: sessionOwnerUserID,
		ExcludeUserScope: excludeUserScope, Provider: provider, Model: model,
		ExplicitCredentialID: requestedCredentialID, Now: s.now(),
	})
	if err != nil {
		return nil, err
	}
	if selection == nil {
		return nil, nil
	}
	value := selection.Credential.ID
	return &value, nil
}

func normalizeRequestedCredentialID(value *uuid.UUID) *uuid.UUID {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	normalized := *value
	return &normalized
}

func (s *Service) ListByProject(
	ctx context.Context,
	principal identity.Principal,
	projectID uuid.UUID,
) ([]Session, error) {
	page, err := s.ListByProjectPage(ctx, principal, projectID, SessionListQuery{Limit: 200})
	return page.Items, err
}

func (s *Service) ListByProjectPage(
	ctx context.Context,
	principal identity.Principal,
	projectID uuid.UUID,
	input SessionListQuery,
) (SessionPage, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return SessionPage{}, err
	}
	project, err := s.projects.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return SessionPage{}, err
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, authorization.SessionRead)
	if err != nil {
		return SessionPage{}, err
	}
	limit := input.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return SessionPage{}, problem.New(400, "invalid_session_limit", "Session list limit must be between 1 and 200.")
	}
	var cursor *sessionListCursor
	if strings.TrimSpace(input.Cursor) != "" {
		decoded, decodeErr := decodeSessionListCursor(input.Cursor)
		if decodeErr != nil || decoded.TenantID != tenantID || decoded.ProjectID != projectID {
			return SessionPage{}, problem.New(400, "invalid_session_cursor", "Session list cursor is invalid for this Project.")
		}
		cursor = &decoded
	}
	query := s.db.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND project_id = ? AND archived_at IS NULL", tenantID, projectID)
	if identity.IsServiceAccount(principal) {
		query = query.Where("visibility <> ?", "private")
	} else if !authorization.TenantAllows(access.TenantRole, authorization.SessionRead) {
		query = query.Where("visibility <> ? OR created_by = ?", "private", principal.UserID)
	}
	if cursor != nil {
		query = query.Where("updated_at < ? OR (updated_at = ? AND id < ?)", cursor.UpdatedAt, cursor.UpdatedAt, cursor.ID)
	}
	models := make([]persistence.AgentSession, 0)
	if err := query.Order("updated_at DESC, id DESC").Limit(limit + 1).Find(&models).Error; err != nil {
		return SessionPage{}, problem.Wrap(500, "sessions_load_failed", "Failed to load sessions.", err)
	}
	hasMore := len(models) > limit
	if hasMore {
		models = models[:limit]
	}
	items := make([]Session, 0, len(models))
	for _, model := range models {
		items = append(items, toSession(model))
	}
	page := SessionPage{Items: items}
	if hasMore {
		last := models[len(models)-1]
		encoded := encodeSessionListCursor(sessionListCursor{
			Version: 1, TenantID: tenantID, ProjectID: projectID, UpdatedAt: last.UpdatedAt, ID: last.ID,
		})
		page.NextCursor = &encoded
	}
	return page, nil
}

type sessionListCursor struct {
	Version   int       `json:"v"`
	TenantID  uuid.UUID `json:"tenantId"`
	ProjectID uuid.UUID `json:"projectId"`
	UpdatedAt time.Time `json:"updatedAt"`
	ID        uuid.UUID `json:"id"`
}

func encodeSessionListCursor(cursor sessionListCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeSessionListCursor(value string) (sessionListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return sessionListCursor{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	var cursor sessionListCursor
	if err := decoder.Decode(&cursor); err != nil {
		return sessionListCursor{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return sessionListCursor{}, errors.New("invalid trailing cursor data")
	}
	if cursor.Version != 1 || cursor.TenantID == uuid.Nil || cursor.ProjectID == uuid.Nil ||
		cursor.UpdatedAt.IsZero() || cursor.ID == uuid.Nil {
		return sessionListCursor{}, errors.New("invalid cursor fields")
	}
	return cursor, nil
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
	item, _, err := s.CreateTurnWithIdempotency(ctx, principal, sessionID, input, "", requestID, ipAddress)
	return item, err
}

func (s *Service) CreateTurnWithIdempotency(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input CreateTurnInput,
	idempotencyKey, requestID, ipAddress string,
) (Turn, bool, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Turn{}, false, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.ExecutionCreate)
	if err != nil {
		return Turn{}, false, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Turn{}, false, problem.New(404, "session_not_found", "Session not found.")
	}
	inputText := strings.TrimSpace(input.InputText)
	if inputText == "" || len(inputText) > 1_000_000 {
		return Turn{}, false, problem.New(400, "invalid_turn_input", "Turn input must be between 1 and 1000000 characters.")
	}
	runtimeMode, err := normalizeRuntimeMode(input.RuntimeMode)
	if err != nil {
		return Turn{}, false, err
	}
	interactionMode, err := normalizeInteractionMode(input.InteractionMode)
	if err != nil {
		return Turn{}, false, err
	}
	sourceProposedPlan, err := normalizeSourceProposedPlan(input.SourceProposedPlan)
	if err != nil {
		return Turn{}, false, err
	}
	queueSnapshot, err := executionqueue.Normalize(executionqueue.Snapshot{
		Class: input.QueueClass, Priority: input.QueuePriority,
		QuotaUnits: input.QuotaUnits, AutomationID: input.AutomationID,
	})
	if err != nil {
		return Turn{}, false, problem.New(
			400,
			"invalid_execution_queue_snapshot",
			"queueClass, queuePriority, quotaUnits, and automationId do not form a valid immutable queue snapshot.",
		)
	}
	var execution persistence.AgentExecution
	var createdEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "turn.create", SuccessStatus: 201,
		Request: map[string]any{
			"sessionId": sessionID, "inputText": inputText,
			"runtimeMode": runtimeMode, "interactionMode": interactionMode,
			"sourceProposedPlan": sourceProposedPlan,
			"automationId":       queueSnapshot.AutomationID, "queueClass": queueSnapshot.Class,
			"queuePriority": queueSnapshot.Priority, "quotaUnits": queueSnapshot.QuotaUnits,
		},
	}, func(tx *gorm.DB) (Turn, error) {
		queuedAt := s.now()
		turn := persistence.AgentTurn{
			ID: uuid.New(), TenantID: tenantID, SessionID: sessionID,
			CreatedBy: principal.UserID, Status: "queued", InputText: inputText,
			TurnKind:    "message",
			RuntimeMode: runtimeMode, InteractionMode: interactionMode,
		}
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id", "status", "trial_expires_at").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return Turn{}, problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if !tenantstate.IsOperational(tenant.Status, tenant.TrialExpiresAt, queuedAt) {
			return Turn{}, problem.New(409, "tenant_suspended", "The tenant is not operational and cannot create new executions.")
		}
		locked, err := lockActiveSession(ctx, tx, tenantID, sessionID)
		if err != nil {
			return Turn{}, err
		}
		if err := RequireSessionWithinAbsoluteLifetime(locked, queuedAt); err != nil {
			return Turn{}, err
		}
		var activeSessionExecutions int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID, activeSessionExecutionStatuses).
			Count(&activeSessionExecutions).Error; err != nil {
			return Turn{}, problem.Wrap(500, "session_execution_check_failed", "Failed to inspect the active Session execution.", err)
		}
		if activeSessionExecutions > 0 {
			return Turn{}, problem.New(409, "session_execution_active", "The Session already has an active Turn execution.")
		}
		if queueSnapshot.AutomationID != nil {
			var automation persistence.Automation
			if err := tx.WithContext(ctx).
				Where(
					"tenant_id = ? AND id = ? AND project_id = ? AND status = ? AND archived_at IS NULL",
					tenantID, *queueSnapshot.AutomationID, locked.ProjectID, "active",
				).
				Take(&automation).Error; err != nil {
				return Turn{}, problem.Wrap(
					409,
					"execution_automation_unavailable",
					"The Automation queue authority is unavailable for this Session Project.",
					err,
				)
			}
		}
		if err := s.RequireExecutionQuotaAvailableFor(ctx, tx, tenantID, ExecutionQuotaAdmission{
			ProjectID: locked.ProjectID, SessionID: sessionID,
			AutomationID: queueSnapshot.AutomationID, QuotaUnits: queueSnapshot.QuotaUnits,
		}); err != nil {
			return Turn{}, err
		}
		var target persistence.ExecutionTarget
		if locked.ExecutionTargetGroupID == nil {
			if err := tx.WithContext(ctx).
				Where("id = ? AND status = ?", locked.ExecutionTargetID, "active").Take(&target).Error; err != nil {
				return Turn{}, problem.Wrap(409, "execution_target_unavailable", "The session execution target is unavailable.", err)
			}
		}
		requiredCapabilities := []string{"send-turn"}
		if interactionMode == "plan" {
			requiredCapabilities = append(requiredCapabilities, "plan-mode")
		}
		var routeRequest *routing.SelectRequest
		if locked.ExecutionTargetGroupID != nil {
			request, err := BuildSessionExecutionTargetGroupSelectRequest(ctx, tx, locked)
			if err != nil {
				return Turn{}, err
			}
			routeRequest = &request
		}
		launchTargetPlan, err := SelectExecutionLaunchTarget(
			ctx,
			tx,
			func() *persistence.ExecutionTarget {
				if routeRequest != nil {
					return nil
				}
				return &target
			}(),
			routeRequest,
			ExecutionLaunchPolicyScope{
				TenantID: tenantID, OrganizationID: locked.OrganizationID, Provider: locked.Provider,
			},
			locked.WarmPoolMode,
			func(
				ctx context.Context,
				tx *gorm.DB,
				target persistence.ExecutionTarget,
				placementSelection placement.Selection,
				selection *routing.Selection,
			) error {
				return s.requireTargetPoolProviderCapabilities(
					ctx,
					tx,
					target,
					placementSelection,
					locked.Provider,
					false,
					requiredCapabilities...,
				)
			},
		)
		if err != nil {
			return Turn{}, err
		}
		target = launchTargetPlan.Target
		if launchTargetPlan.RoutingSelection != nil {
			if err := AdvanceSessionExecutionTargetAuthority(ctx, tx, &locked, *launchTargetPlan.RoutingSelection, queuedAt); err != nil {
				return Turn{}, err
			}
		}
		resources, err := s.ensureRuntimeResources(ctx, tx, &locked)
		if err != nil {
			return Turn{}, err
		}
		provider := locked.Provider
		execution = persistence.AgentExecution{
			ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, TurnID: turn.ID,
			AutomationID: queueSnapshot.AutomationID, QueueClass: queueSnapshot.Class,
			QueuePriority: queueSnapshot.Priority, QuotaUnits: queueSnapshot.QuotaUnits,
			Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
			Provider: &provider, ProviderRuntimeBindingID: &resources.BindingID, RemoteWorkspaceID: &resources.WorkspaceID,
			WorkspaceMaterializationID: &resources.MaterializationID,
			RestoreCheckpointID:        resources.RestoreCheckpointID,
			WarmPoolModeSnapshot:       locked.WarmPoolMode,
			Generation:                 0, RequestedBy: principal.UserID, QueuedAt: queuedAt,
		}
		if err := tx.Create(&turn).Error; err != nil {
			return Turn{}, problem.Wrap(409, "turn_create_rejected", "Turn creation was rejected by a tenant isolation constraint.", err)
		}
		scheduled, err := CreateScheduledExecution(ctx, tx, execution, launchTargetPlan, queuedAt)
		if err != nil {
			return Turn{}, err
		}
		execution = scheduled.Execution
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "execution.queued", MessageKey: execution.ID.String(),
			Payload: scheduled.MergeSchedulingEvidencePayload(map[string]any{
				"executionId": execution.ID, "tenantId": tenantID, "sessionId": sessionID,
				"turnId": turn.ID, "executionTargetId": execution.ExecutionTargetID,
				"targetKind": execution.TargetKind, "attempt": execution.Attempt,
				"automationId": execution.AutomationID, "queueClass": execution.QueueClass,
				"queuePriority": execution.QueuePriority, "quotaUnits": execution.QuotaUnits,
				"provider":                              provider,
				"providerRuntimeBindingId":              resources.BindingID,
				"remoteWorkspaceId":                     resources.WorkspaceID,
				"workspaceMaterializationId":            resources.MaterializationID,
				"workspaceMaterializationIncarnationId": resources.IncarnationID,
				"workspaceLayoutVersion":                resources.LayoutVersion,
				"restoreCheckpointId":                   resources.RestoreCheckpointID,
			}),
			Headers: map[string]any{"eventVersion": 1}, AvailableAt: queuedAt, CreatedAt: queuedAt,
		}); err != nil {
			return Turn{}, problem.Wrap(409, "execution_outbox_create_rejected", "Execution dispatch could not be queued atomically.", err)
		}
		turnCreatedPayload := scheduled.MergeSchedulingEvidencePayload(map[string]any{
			"turnId": turn.ID, "executionId": execution.ID, "inputText": inputText,
			"status": "queued", "executionTargetId": execution.ExecutionTargetID,
			"targetKind":                 execution.TargetKind,
			"automationId":               execution.AutomationID,
			"queueClass":                 execution.QueueClass,
			"queuePriority":              execution.QueuePriority,
			"quotaUnits":                 execution.QuotaUnits,
			"workspaceMaterializationId": resources.MaterializationID,
			"runtimeMode":                runtimeMode, "interactionMode": interactionMode,
		})
		if sourceProposedPlan != nil {
			turnCreatedPayload["sourceProposedPlan"] = sourceProposedPlan
		}
		createdEvent, err = s.appendEvent(ctx, tx, &locked, eventInput{
			EventType: "turn.created", ActorType: "user", ActorID: &principal.UserID,
			ExecutionID: &execution.ID,
			Payload:     turnCreatedPayload,
		})
		if err != nil {
			return Turn{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "turn.created", ResourceType: "agent_turn", ResourceID: &turn.ID,
			OrganizationID: &locked.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"sessionId": sessionID, "projectId": locked.ProjectID,
				"runtimeMode": runtimeMode, "interactionMode": interactionMode,
			},
		}); err != nil {
			return Turn{}, err
		}
		return toTurn(turn), nil
	})
	if err != nil {
		return Turn{}, false, err
	}
	if !result.Replayed {
		s.events.publish(toEvent(createdEvent))
	}
	return result.Value, result.Replayed, nil
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
	session, eventAccess, err := s.authorizedEventAccess(ctx, principal, tenantID, sessionID)
	if err != nil {
		return EventPage{}, err
	}
	if afterSequence < 0 {
		return EventPage{}, problem.New(400, "invalid_event_sequence", "afterSequence must be zero or greater.")
	}
	limit = persistence.NormalizeLimit(limit, 100, 500)
	logical, err := LoadLogicalEventsPage(
		ctx, s.db, tenantID, sessionID, afterSequence, session.LastEventSequence, limit,
	)
	if err != nil {
		return EventPage{}, problem.Wrap(500, "session_events_load_failed", "Failed to load session events.", err)
	}
	models := make([]persistence.SessionEvent, 0, len(logical))
	for _, item := range logical {
		model := item.Event
		model.SessionID = sessionID
		model.OrganizationID = session.OrganizationID
		model.ProjectID = session.ProjectID
		models = append(models, model)
	}
	items := make([]Event, 0, len(models))
	for _, model := range models {
		items = append(items, SanitizeEventForAccess(toEvent(model), eventAccess))
	}
	return EventPage{Items: items, LastSequence: session.LastEventSequence}, nil
}

func (s *Service) Archive(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	requestID, ipAddress string,
) (Session, error) {
	item, _, err := s.ArchiveWithIdempotency(ctx, principal, sessionID, "", requestID, ipAddress)
	return item, err
}

func (s *Service) SetSettledWithIdempotency(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input SetSessionSettledInput,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, false, err
	}
	current, _, err := s.authorizedModel(
		ctx,
		principal,
		tenantID,
		sessionID,
		authorization.SessionSettle,
	)
	if err != nil {
		return Session{}, false, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Session{}, false, problem.New(404, "session_not_found", "Session not found.")
	}
	operation := "session.unsettle"
	eventType := "session.unsettled"
	if input.Settled {
		operation = "session.settle"
		eventType = "session.settled"
	}
	var settledEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID:      tenantID,
		ActorID:       principal.UserID,
		Key:           idempotencyKey,
		Operation:     operation,
		SuccessStatus: 200,
		Request:       map[string]any{"sessionId": sessionID, "settled": input.Settled},
	}, func(tx *gorm.DB) (Session, error) {
		var locked persistence.AgentSession
		lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).
			Take(&locked).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return Session{}, problem.New(404, "session_not_found", "Session not found.")
		}
		if lookupErr != nil {
			return Session{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the session.", lookupErr)
		}
		if locked.ArchivedAt != nil || locked.Status == "archived" {
			return Session{}, problem.New(409, "session_archived", "Archived Sessions cannot be marked done.")
		}
		currentlySettled := locked.SettledAt != nil
		if currentlySettled == input.Settled {
			return toSession(locked), nil
		}

		now := time.Now().UTC()
		var settledAt *time.Time
		if input.Settled {
			settledAt = &now
		}
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).
			Update("settled_at", settledAt).Error; err != nil {
			return Session{}, problem.Wrap(500, "session_settle_failed", "Failed to update the Session done state.", err)
		}
		locked.SettledAt = settledAt
		payload := map[string]any{"settled": input.Settled}
		if settledAt != nil {
			payload["settledAt"] = *settledAt
		}
		settledEvent, err = appendEvent(ctx, tx, &locked, eventInput{
			EventType: eventType,
			ActorType: "user",
			ActorID:   &principal.UserID,
			Payload:   payload,
		})
		if err != nil {
			return Session{}, err
		}
		outboxPayload := map[string]any{
			"tenantId":       tenantID,
			"organizationId": locked.OrganizationID,
			"projectId":      locked.ProjectID,
			"sessionId":      sessionID,
			"settled":        input.Settled,
		}
		if settledAt != nil {
			outboxPayload["settledAt"] = *settledAt
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID:   &tenantID,
			Topic:      eventType,
			MessageKey: sessionID.String(),
			Payload:    outboxPayload,
		}); err != nil {
			return Session{}, problem.Wrap(500, "session_settle_outbox_failed", "The Session done event could not be queued.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID:       tenantID,
			ActorType:      "user",
			ActorID:        &principal.UserID,
			Action:         eventType,
			ResourceType:   "agent_session",
			ResourceID:     &sessionID,
			OrganizationID: &locked.OrganizationID,
			RequestID:      requestID,
			IPAddress:      ipAddress,
		}); err != nil {
			return Session{}, err
		}
		return toSession(locked), nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if !result.Replayed && settledEvent.EventID != uuid.Nil {
		s.events.publish(toEvent(settledEvent))
	}
	return result.Value, result.Replayed, nil
}

func (s *Service) ArchiveWithIdempotency(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
) (Session, bool, error) {
	tenantID, err := ActiveTenant(principal)
	if err != nil {
		return Session{}, false, err
	}
	current, _, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.SessionArchive)
	if err != nil {
		return Session{}, false, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return Session{}, false, problem.New(404, "session_not_found", "Session not found.")
	}
	actorType, actorID := identity.ActorType(principal), identity.ActorID(principal)
	var archivedEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: actorID, Key: idempotencyKey,
		Operation: "session.archive", SuccessStatus: 200,
		Request: map[string]any{"sessionId": sessionID},
	}, func(tx *gorm.DB) (Session, error) {
		var locked persistence.AgentSession
		lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).Take(&locked).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return Session{}, problem.New(404, "session_not_found", "Session not found.")
		}
		if lookupErr != nil {
			return Session{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the session.", lookupErr)
		}
		if locked.ArchivedAt != nil {
			return toSession(locked), nil
		}
		if locked.Status != "active" {
			return Session{}, problem.New(409, "session_not_active", "Session is not active.")
		}
		now := time.Now().UTC()
		cleanupAfterDays, err := loadWorkspaceCleanupAfterDays(ctx, tx, tenantID)
		if err != nil {
			return Session{}, err
		}
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).
			Updates(map[string]any{"status": "archived", "archived_at": now}).Error; err != nil {
			return Session{}, problem.Wrap(500, "session_archive_failed", "Failed to archive the session.", err)
		}
		locked.Status = "archived"
		locked.ArchivedAt = &now
		if err := scheduleArchivedWorkspaceCleanup(
			ctx, tx, tenantID, sessionID, now, cleanupAfterDays, "session-archive",
		); err != nil {
			return Session{}, err
		}
		archivedEvent, err = s.appendEvent(ctx, tx, &locked, eventInput{
			EventType: "session.archived", ActorType: actorType, ActorID: &actorID,
			Payload: map[string]any{"archivedAt": now},
		})
		if err != nil {
			return Session{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "session.archived", MessageKey: sessionID.String(),
			Payload: map[string]any{
				"tenantId": tenantID, "organizationId": locked.OrganizationID,
				"projectId": locked.ProjectID, "sessionId": sessionID, "archivedAt": now,
			},
		}); err != nil {
			return Session{}, problem.Wrap(500, "session_archive_outbox_failed", "The archived Session event could not be queued.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: actorType, ActorID: &actorID,
			Action: "session.archived", ResourceType: "agent_session", ResourceID: &sessionID,
			OrganizationID: &locked.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		}); err != nil {
			return Session{}, err
		}
		return toSession(locked), nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if !result.Replayed && archivedEvent.EventID != uuid.Nil {
		s.events.publish(toEvent(archivedEvent))
	}
	return result.Value, result.Replayed, nil
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
	if _, _, err := s.authorizedEventAccess(ctx, principal, tenantID, sessionID); err != nil {
		return uuid.Nil, nil, nil, err
	}
	events, cancel := s.events.subscribe(tenantID, sessionID)
	return tenantID, events, cancel, nil
}

func (s *Service) SanitizeSubscribedEvent(
	ctx context.Context,
	principal identity.Principal,
	event Event,
) (Event, error) {
	if err := identity.RequireActiveTenant(principal, event.TenantID); err != nil {
		return Event{}, err
	}
	if !isInteractionLifecycleEvent(event.EventType) {
		return event, nil
	}
	_, access, err := s.authorizedEventAccess(ctx, principal, event.TenantID, event.SessionID)
	if err != nil {
		return Event{}, err
	}
	return SanitizeEventForAccess(event, access), nil
}

func (s *Service) authorizedEventAccess(
	ctx context.Context,
	principal identity.Principal,
	tenantID, sessionID uuid.UUID,
) (Session, EventAccess, error) {
	model, access, err := s.authorizedModel(ctx, principal, tenantID, sessionID, authorization.SessionRead)
	if err != nil {
		return Session{}, EventAccess{}, err
	}
	if model.Visibility == "private" && model.CreatedBy != principal.UserID && !authorization.TenantAllows(access.TenantRole, authorization.SessionRead) {
		return Session{}, EventAccess{}, problem.New(404, "session_not_found", "Session not found.")
	}
	return toSession(model), eventAccessForOrganization(access), nil
}

func eventAccessForOrganization(access authorization.OrganizationAccess) EventAccess {
	return EventAccess{
		CanReadInteractionDetails: authorization.TenantAllows(access.TenantRole, authorization.ExecutionApprove) ||
			authorization.OrganizationAllows(access.OrganizationRole, authorization.ExecutionApprove),
	}
}

func SanitizeEventForAccess(event Event, access EventAccess) Event {
	if access.CanReadInteractionDetails || !isInteractionLifecycleEvent(event.EventType) {
		return event
	}
	event.EventVersion = 1
	event.EventType = "session.event.redacted"
	event.ExecutionID = nil
	event.WorkerID = nil
	event.Generation = nil
	event.ActorType = "system"
	event.ActorID = nil
	event.Payload = map[string]any{}
	return event
}

func isInteractionLifecycleEvent(eventType string) bool {
	switch eventType {
	case "approval.requested", "approval.resolved", "request.opened", "request.resolved",
		"user-input.requested", "user-input.resolved":
		return true
	default:
		return false
	}
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
	if identity.IsServiceAccount(principal) && model.Visibility == "private" {
		return persistence.AgentSession{}, authorization.OrganizationAccess{}, problem.New(404, "session_not_found", "Session not found.")
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
	// MeaningfulActivity is set only by trusted semantic runtime ingestion.
	// Worker lifecycle/checkpoint bookkeeping must leave it false.
	MeaningfulActivity bool
	ExecutionID        *uuid.UUID
	WorkerID           *uuid.UUID
	Generation         *int64
	Payload            map[string]any
	OccurredAt         *time.Time
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
	return s.appendEvent(ctx, tx, &session, eventInput{
		EventID: eventID, EventVersion: eventVersion, EventType: input.EventType,
		ActorType: input.ActorType, ActorID: input.ActorID, MeaningfulActivity: input.MeaningfulActivity,
		ExecutionID: input.ExecutionID,
		WorkerID:    input.WorkerID, Generation: input.Generation, Payload: input.Payload,
		OccurredAt: occurredAt,
	})
}

func (s *Service) PublishInternalEvent(event persistence.SessionEvent) {
	s.events.publish(toEvent(event))
}

type eventInput struct {
	EventID            uuid.UUID
	EventVersion       int
	EventType          string
	ActorType          string
	ActorID            *uuid.UUID
	MeaningfulActivity bool
	ExecutionID        *uuid.UUID
	WorkerID           *uuid.UUID
	Generation         *int64
	Payload            map[string]any
	OccurredAt         time.Time
}

func appendEvent(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
	input eventInput,
) (persistence.SessionEvent, error) {
	if machine, ok := authorization.MachinePrincipalFromContext(ctx); ok && input.ActorType == "user" {
		input.ActorType = "service_account"
		input.ActorID = &machine.ActorID
	}
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
	updates := map[string]any{"last_event_sequence": nextSequence}
	observedAt := time.Now().UTC()
	// User actions are semantic by definition. Worker events renew activity only
	// when the trusted runtime-ingestion path marks them explicitly; lifecycle,
	// checkpoint, Workspace, and recovery bookkeeping must never slide activity.
	if input.ActorType == "user" || input.ActorType == "service_account" || input.MeaningfulActivity {
		if session.MeaningfulActivityAt.After(observedAt) {
			observedAt = session.MeaningfulActivityAt
		}
		updates["meaningful_activity_at"] = observedAt
		updates["meaningful_activity_sequence"] = nextSequence
		session.MeaningfulActivityAt = observedAt
		session.MeaningfulActivitySequence = nextSequence
	}
	if input.ExecutionID != nil {
		updates["resource_idle_since"] = nil
		session.ResourceIdleSince = nil
	}
	if input.ExecutionID != nil {
		if resourceState, changed := eventResourceState(input.EventType, input.Payload); changed {
			updates["resource_state"] = resourceState
			session.ResourceState = resourceState
		}
	}
	if input.ExecutionID != nil && terminalExecutionEvent(input.EventType) {
		updates["resource_idle_since"] = observedAt
		session.ResourceIdleSince = &observedAt
	}
	if input.ExecutionID != nil && input.EventType == "execution.suspended" {
		updates["resource_idle_since"] = observedAt
		session.ResourceIdleSince = &observedAt
	}
	result := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ? AND last_event_sequence = ?", session.TenantID, session.ID, session.LastEventSequence).
		Updates(updates)
	if result.Error != nil {
		return persistence.SessionEvent{}, problem.Wrap(500, "session_event_sequence_update_failed", "Failed to update the session event sequence.", result.Error)
	}
	if result.RowsAffected != 1 {
		return persistence.SessionEvent{}, problem.New(409, "session_event_sequence_conflict", "Session event sequence changed concurrently.")
	}
	session.LastEventSequence = nextSequence
	return event, nil
}

func (s *Service) appendEvent(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
	input eventInput,
) (persistence.SessionEvent, error) {
	event, err := appendEvent(ctx, tx, session, input)
	if err != nil || s.eventProjector == nil {
		return event, err
	}
	if err := s.eventProjector.ProjectSessionEvent(ctx, tx, event); err != nil {
		return persistence.SessionEvent{}, problem.Wrap(
			500, "session_event_projection_failed", "Session Event delivery projections could not be persisted.", err,
		)
	}
	return event, nil
}

func terminalExecutionEvent(eventType string) bool {
	switch eventType {
	case "execution.completed", "execution.failed", "execution.cancelled", "execution.interrupted":
		return true
	default:
		return false
	}
}

func eventResourceState(eventType string, payload map[string]any) (string, bool) {
	switch eventType {
	case "turn.created":
		return "provisioning", true
	case "execution.leased", "execution.started", "approval.resolved", "request.resolved", "user-input.resolved":
		return "active", true
	case "approval.requested", "request.opened", "user-input.requested":
		return "waiting", true
	case "execution.suspend-checkpointing":
		return "checkpointing", true
	case "execution.suspend-aborted":
		if reason, _ := payload["reason"].(string); reason == "active-idle-timeout" {
			return "active", true
		}
		return "waiting", true
	case "execution.suspended":
		return "suspended", true
	case "execution.recovering":
		return "restoring", true
	case "execution.completed", "execution.failed", "execution.cancelled", "execution.interrupted":
		return "idle", true
	default:
		return "", false
	}
}

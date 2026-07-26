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
	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
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
	if err := s.lockExecutionQuotaAuthority(ctx, tx, tenantID); err != nil {
		return err
	}
	return s.requireExecutionQuotaAvailableAfterAuthorityLock(ctx, tx, tenantID)
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
	var quota persistence.TenantQuota
	quotaErr := tx.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&quota).Error
	if errors.Is(quotaErr, gorm.ErrRecordNotFound) {
		return nil
	}
	if quotaErr != nil {
		return problem.Wrap(500, "execution_quota_check_failed", "Failed to load the tenant execution quota.", quotaErr)
	}
	if quota.MaxConcurrentExecutions == nil {
		return nil
	}
	var activeExecutions int64
	if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND status IN ?", tenantID, resourceQuotaExecutionStatuses).
		Count(&activeExecutions).Error; err != nil {
		return problem.Wrap(500, "execution_quota_check_failed", "Failed to check tenant execution quota.", err)
	}
	if activeExecutions >= int64(*quota.MaxConcurrentExecutions) {
		return problem.New(409, "execution_quota_exceeded", "The tenant concurrent execution quota has been reached.")
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
		UpdatedAt: model.UpdatedAt, ArchivedAt: model.ArchivedAt,
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
	visibility, err := normalizeSessionVisibility(input.Visibility)
	if err != nil {
		return Session{}, false, err
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
		createdEvent, err = appendEvent(ctx, tx, &model, eventInput{
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
	provider string,
	model *string,
	requestedCredentialID *uuid.UUID,
) (*uuid.UUID, error) {
	selection, err := credentialscope.Resolve(ctx, db, credentialscope.Request{
		TenantID: tenantID, OrganizationID: organizationID, SessionOwnerUserID: sessionOwnerUserID,
		Provider: provider, Model: model, ExplicitCredentialID: requestedCredentialID, Now: s.now(),
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
	var execution persistence.AgentExecution
	var createdEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "turn.create", SuccessStatus: 201,
		Request: map[string]any{
			"sessionId": sessionID, "inputText": inputText,
			"runtimeMode": runtimeMode, "interactionMode": interactionMode,
			"sourceProposedPlan": sourceProposedPlan,
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
			Select("id", "status").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return Turn{}, problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if tenant.Status != "active" {
			return Turn{}, problem.New(409, "tenant_suspended", "The tenant is suspended and cannot create new executions.")
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
		if err := s.RequireExecutionQuotaAvailable(ctx, tx, tenantID); err != nil {
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
		decision := scheduled.Decision
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "execution.queued", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"executionId": execution.ID, "tenantId": tenantID, "sessionId": sessionID,
				"turnId": turn.ID, "executionTargetId": execution.ExecutionTargetID,
				"targetKind": execution.TargetKind, "attempt": execution.Attempt,
				"workerReleaseRevisionId":             execution.WorkerReleaseRevisionID,
				"workerReleaseChannel":                execution.WorkerReleaseChannel,
				"workerPoolId":                        execution.WorkerPoolID,
				"workerPoolVersion":                   execution.WorkerPoolVersion,
				"capacityClass":                       execution.CapacityClass,
				"placementPolicyVersion":              execution.PlacementPolicyVersion,
				"placementRegion":                     execution.PlacementRegion,
				"placementClusterId":                  execution.PlacementClusterID,
				"tenantSchedulingPolicyVersion":       execution.TenantSchedulingPolicyVersion,
				"tenantSchedulingPolicyDigest":        execution.TenantSchedulingPolicyDigest,
				"organizationSchedulingPolicyVersion": execution.OrganizationSchedulingPolicyVersion,
				"organizationSchedulingPolicyDigest":  execution.OrganizationSchedulingPolicyDigest,
				"targetGroupId":                       execution.TargetGroupID,
				"targetGroupVersion":                  execution.TargetGroupVersion,
				"targetGroupMemberVersion":            execution.TargetGroupMemberVersion,
				"selectedRegion":                      execution.SelectedRegion,
				"selectedClusterId":                   execution.SelectedClusterID,
				"routingReason":                       execution.RoutingReason,
				"schedulingDecisionId":                decision.ID,
				"schedulingAlgorithmVersion":          decision.AlgorithmVersion,
				"schedulingEvidenceCompleteness":      decision.EvidenceCompleteness,
				"schedulingCandidateSetSha256":        decision.CandidateSetSHA256,
				"provider":                            provider, "providerRuntimeBindingId": resources.BindingID,
				"remoteWorkspaceId":                     resources.WorkspaceID,
				"workspaceMaterializationId":            resources.MaterializationID,
				"workspaceMaterializationIncarnationId": resources.IncarnationID,
				"workspaceLayoutVersion":                resources.LayoutVersion,
				"restoreCheckpointId":                   resources.RestoreCheckpointID,
			},
			Headers: map[string]any{"eventVersion": 1}, AvailableAt: queuedAt, CreatedAt: queuedAt,
		}); err != nil {
			return Turn{}, problem.Wrap(409, "execution_outbox_create_rejected", "Execution dispatch could not be queued atomically.", err)
		}
		turnCreatedPayload := map[string]any{
			"turnId": turn.ID, "executionId": execution.ID, "inputText": inputText,
			"status": "queued", "executionTargetId": execution.ExecutionTargetID,
			"targetKind":                          execution.TargetKind,
			"workerReleaseRevisionId":             execution.WorkerReleaseRevisionID,
			"workerReleaseChannel":                execution.WorkerReleaseChannel,
			"workerPoolId":                        execution.WorkerPoolID,
			"workerPoolVersion":                   execution.WorkerPoolVersion,
			"capacityClass":                       execution.CapacityClass,
			"placementPolicyVersion":              execution.PlacementPolicyVersion,
			"placementRegion":                     execution.PlacementRegion,
			"placementClusterId":                  execution.PlacementClusterID,
			"tenantSchedulingPolicyVersion":       execution.TenantSchedulingPolicyVersion,
			"tenantSchedulingPolicyDigest":        execution.TenantSchedulingPolicyDigest,
			"organizationSchedulingPolicyVersion": execution.OrganizationSchedulingPolicyVersion,
			"organizationSchedulingPolicyDigest":  execution.OrganizationSchedulingPolicyDigest,
			"targetGroupId":                       execution.TargetGroupID,
			"targetGroupVersion":                  execution.TargetGroupVersion,
			"targetGroupMemberVersion":            execution.TargetGroupMemberVersion,
			"selectedRegion":                      execution.SelectedRegion,
			"selectedClusterId":                   execution.SelectedClusterID,
			"routingReason":                       execution.RoutingReason,
			"schedulingDecisionId":                decision.ID,
			"schedulingAlgorithmVersion":          decision.AlgorithmVersion,
			"schedulingEvidenceCompleteness":      decision.EvidenceCompleteness,
			"schedulingCandidateSetSha256":        decision.CandidateSetSHA256,
			"workspaceMaterializationId":          resources.MaterializationID,
			"runtimeMode":                         runtimeMode, "interactionMode": interactionMode,
		}
		if sourceProposedPlan != nil {
			turnCreatedPayload["sourceProposedPlan"] = sourceProposedPlan
		}
		createdEvent, err = appendEvent(ctx, tx, &locked, eventInput{
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
	var archivedEvent persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
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
		archivedEvent, err = appendEvent(ctx, tx, &locked, eventInput{
			EventType: "session.archived", ActorType: "user", ActorID: &principal.UserID,
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
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
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
	return appendEvent(ctx, tx, &session, eventInput{
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
	if input.ActorType == "user" || input.MeaningfulActivity {
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

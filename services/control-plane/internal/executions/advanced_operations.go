package executions

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
)

type primaryOperationRequest struct {
	Type                      string
	TurnKind                  string
	CommandType               string
	CommandPrefix             string
	CapabilityID              string
	RuntimeMode               string
	ExpectedLastEventSequence int64
	Payload                   map[string]any
}

func (s *Service) RequestCompact(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input CompactSessionInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[QueuedSessionOperation], error) {
	expected, err := requiredExpectedLastEventSequence(input.ExpectedLastEventSequence)
	if err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}
	return s.requestPrimaryOperation(ctx, principal, sessionID, idempotencyKey, requestID, ipAddress,
		primaryOperationRequest{
			Type: "compact", TurnKind: "compact", CommandType: "CompactSession",
			CommandPrefix: "compact", CapabilityID: "compact", ExpectedLastEventSequence: expected,
			RuntimeMode: "full-access",
			Payload:     map[string]any{"expectedLastEventSequence": expected},
		})
}

func (s *Service) RequestReview(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input StartReviewInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[QueuedSessionOperation], error) {
	expected, err := requiredExpectedLastEventSequence(input.ExpectedLastEventSequence)
	if err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}
	target, err := normalizeReviewTarget(input.Target)
	if err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}
	runtimeMode, err := normalizeReviewRuntimeMode(input.RuntimeMode)
	if err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}
	return s.requestPrimaryOperation(ctx, principal, sessionID, idempotencyKey, requestID, ipAddress,
		primaryOperationRequest{
			Type: "review", TurnKind: "review", CommandType: "StartReview",
			CommandPrefix: "review", CapabilityID: "review", ExpectedLastEventSequence: expected,
			RuntimeMode: runtimeMode,
			Payload: map[string]any{
				"expectedLastEventSequence": expected, "target": target, "runtimeMode": runtimeMode,
			},
		})
}

func requiredExpectedLastEventSequence(value *int64) (int64, error) {
	if value == nil || *value < 0 {
		return 0, problem.New(400, "expected_session_sequence_required", "expectedLastEventSequence must be provided and cannot be negative.")
	}
	return *value, nil
}

func normalizeReviewTarget(input ReviewTarget) (map[string]any, error) {
	targetType := strings.TrimSpace(input.Type)
	switch targetType {
	case "uncommittedChanges":
		if input.Branch != nil && strings.TrimSpace(*input.Branch) != "" {
			return nil, problem.New(400, "invalid_review_target", "uncommittedChanges does not accept a branch.")
		}
		return map[string]any{"type": targetType}, nil
	case "baseBranch":
		if input.Branch == nil {
			return map[string]any{"type": targetType}, nil
		}
		branch := strings.TrimSpace(*input.Branch)
		if branch == "" || len(branch) > 255 || strings.ContainsAny(branch, "\r\n\x00") {
			return nil, problem.New(400, "invalid_review_target", "Review branch must contain between 1 and 255 safe characters.")
		}
		return map[string]any{"type": targetType, "branch": branch}, nil
	default:
		return nil, problem.New(400, "invalid_review_target", "Review target type must be uncommittedChanges or baseBranch.")
	}
}

func normalizeReviewRuntimeMode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "approval-required"
	}
	if value != "approval-required" && value != "full-access" {
		return "", problem.New(400, "invalid_runtime_mode", "runtimeMode must be approval-required or full-access.")
	}
	return value, nil
}

func (s *Service) requestPrimaryOperation(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	idempotencyKey, requestID, ipAddress string,
	request primaryOperationRequest,
) (OperationResult[QueuedSessionOperation], error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return OperationResult[QueuedSessionOperation]{}, problem.New(400, "idempotency_key_required", "Idempotency-Key is required for Session operations.")
	}
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}
	current, err := s.sessions.Get(ctx, principal, tenantID, sessionID)
	if err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}
	if current.Visibility == "private" && current.CreatedBy != principal.UserID {
		return OperationResult[QueuedSessionOperation]{}, problem.New(404, "session_not_found", "Session not found.")
	}
	if _, err := s.authorizer.RequireOrganization(
		ctx, principal.UserID, tenantID, current.OrganizationID, authorization.ExecutionCreate,
	); err != nil {
		return OperationResult[QueuedSessionOperation]{}, err
	}

	var appended persistence.SessionEvent
	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "session." + request.Type, SuccessStatus: 202,
		Request: map[string]any{
			"sessionId": sessionID, "expectedLastEventSequence": request.ExpectedLastEventSequence,
			"payload": request.Payload,
		},
	}, func(tx *gorm.DB) (QueuedSessionOperation, error) {
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id", "status").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return QueuedSessionOperation{}, problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if tenant.Status != "active" {
			return QueuedSessionOperation{}, problem.New(409, "tenant_suspended", "The tenant is suspended and cannot create new executions.")
		}
		var session persistence.AgentSession
		sessionErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ? AND status = ? AND archived_at IS NULL", tenantID, sessionID, "active").
			Take(&session).Error
		if errors.Is(sessionErr, gorm.ErrRecordNotFound) {
			return QueuedSessionOperation{}, problem.New(409, "session_not_active", "Session is not active.")
		}
		if sessionErr != nil {
			return QueuedSessionOperation{}, problem.Wrap(500, "session_lock_failed", "Failed to lock the Session.", sessionErr)
		}
		if session.LastEventSequence != request.ExpectedLastEventSequence {
			apiError := problem.New(409, "stale_session_sequence", "The Session changed after it was loaded.")
			apiError.Details = map[string]any{"expectedLastEventSequence": request.ExpectedLastEventSequence, "actualLastEventSequence": session.LastEventSequence}
			return QueuedSessionOperation{}, apiError
		}
		if request.Type == "compact" &&
			(session.ProviderResumeCursorState != "usable" || len(session.ProviderResumeCursorEncrypted) == 0) {
			return QueuedSessionOperation{}, problem.New(
				409,
				providercapabilities.ReasonProviderCursorRequired,
				"Codex Session compaction requires a usable native Provider Cursor from a completed Turn.",
			)
		}
		if request.Type == "review" {
			target, _ := request.Payload["target"].(map[string]any)
			if target["type"] == "baseBranch" {
				if branch, _ := target["branch"].(string); strings.TrimSpace(branch) == "" {
					var project persistence.Project
					if err := tx.WithContext(ctx).Select("default_branch").
						Where("tenant_id = ? AND id = ?", tenantID, session.ProjectID).Take(&project).Error; err != nil {
						return QueuedSessionOperation{}, problem.Wrap(500, "review_project_load_failed", "The Review Project could not be loaded.", err)
					}
					target["branch"] = project.DefaultBranch
				}
			}
		}
		var active int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID,
				[]string{"queued", "leased", "running", "waiting-for-approval", "recovering"}).
			Count(&active).Error; err != nil {
			return QueuedSessionOperation{}, problem.Wrap(500, "session_execution_check_failed", "Failed to inspect the active Session execution.", err)
		}
		if active != 0 {
			return QueuedSessionOperation{}, problem.New(409, "session_execution_active", "The Session already has an active Turn execution.")
		}
		if err := s.sessions.RequireExecutionQuotaAvailable(ctx, tx, tenantID); err != nil {
			return QueuedSessionOperation{}, err
		}
		var target persistence.ExecutionTarget
		if err := tx.WithContext(ctx).
			Where("id = ? AND status = ?", session.ExecutionTargetID, "active").
			Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, session.OrganizationID).
			Take(&target).Error; err != nil {
			return QueuedSessionOperation{}, problem.Wrap(409, "execution_target_unavailable", "The Session Execution Target is unavailable.", err)
		}
		projection, err := s.projectIdleSessionProviderCapabilities(ctx, tx, target, sessions.Session{
			ID: session.ID, TenantID: session.TenantID, Provider: session.Provider,
		})
		if err != nil {
			return QueuedSessionOperation{}, err
		}
		decision := providercapabilities.Check(projection, session.Provider, request.CapabilityID)
		if err := sessions.EnforceProviderCapabilityDecision(target, decision, true); err != nil {
			return QueuedSessionOperation{}, err
		}
		resources, err := s.sessions.EnsureRuntimeResources(ctx, tx, &session)
		if err != nil {
			return QueuedSessionOperation{}, err
		}
		now := s.now()
		turn := persistence.AgentTurn{
			ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, CreatedBy: principal.UserID,
			Status: "queued", InputText: "", TurnKind: request.TurnKind,
			RuntimeMode: request.RuntimeMode, InteractionMode: "default", CreatedAt: now,
		}
		provider := session.Provider
		execution := persistence.AgentExecution{
			ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, TurnID: turn.ID,
			Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
			Provider: &provider, ProviderRuntimeBindingID: &resources.BindingID,
			RemoteWorkspaceID: &resources.WorkspaceID, WorkspaceMaterializationID: &resources.MaterializationID,
			RestoreCheckpointID: resources.RestoreCheckpointID, RequestedBy: principal.UserID, QueuedAt: now,
		}
		releaseSelection, err := workerreleases.SelectExecution(ctx, tx, target.ID, execution.ID)
		if err != nil {
			return QueuedSessionOperation{}, err
		}
		if releaseSelection != nil {
			execution.WorkerReleaseRevisionID = &releaseSelection.RevisionID
			execution.WorkerReleaseChannel = &releaseSelection.Channel
		}
		commandID := uuid.New()
		payload := make(map[string]any, len(request.Payload)+1)
		for key, value := range request.Payload {
			payload[key] = value
		}
		payload["turnId"] = turn.ID.String()
		command := persistence.ExecutionControlCommand{
			ID: commandID, TenantID: tenantID, ExecutionID: execution.ID, SessionID: sessionID,
			TurnID: turn.ID, Provider: provider, CommandType: request.CommandType,
			CommandID: request.CommandPrefix + ":" + commandID.String(), Payload: payload,
			Status: "pending", RequestedBy: principal.UserID, RequestedAt: now, DeliveryAvailableAt: now,
		}
		if err := tx.WithContext(ctx).Create(&turn).Error; err != nil {
			return QueuedSessionOperation{}, problem.Wrap(409, "turn_create_rejected", "The operation Turn could not be created.", err)
		}
		if err := tx.WithContext(ctx).Create(&execution).Error; err != nil {
			return QueuedSessionOperation{}, problem.Wrap(409, "execution_create_rejected", "The operation Execution could not be created.", err)
		}
		if err := tx.WithContext(ctx).Create(&command).Error; err != nil {
			return QueuedSessionOperation{}, problem.Wrap(409, "control_command_conflict", "The primary Control command conflicts with another operation.", err)
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, tenantID, sessionID, sessions.InternalEventInput{
			EventType: "turn.created", ActorType: "user", ActorID: &principal.UserID, ExecutionID: &execution.ID,
			Payload: map[string]any{
				"turnId": turn.ID, "executionId": execution.ID, "status": "queued",
				"turnKind": request.TurnKind, "controlCommandId": command.ID,
				"executionTargetId": target.ID, "targetKind": target.Kind,
				"workerReleaseRevisionId":    execution.WorkerReleaseRevisionID,
				"workerReleaseChannel":       execution.WorkerReleaseChannel,
				"workspaceMaterializationId": resources.MaterializationID,
				"runtimeMode":                turn.RuntimeMode, "interactionMode": turn.InteractionMode,
				"operation": request.Payload,
			},
		})
		if err != nil {
			return QueuedSessionOperation{}, err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "execution.queued", MessageKey: execution.ID.String(),
			Payload: map[string]any{
				"executionId": execution.ID, "tenantId": tenantID, "sessionId": sessionID,
				"turnId": turn.ID, "turnKind": request.TurnKind, "controlCommandId": command.ID,
				"executionTargetId": target.ID, "targetKind": target.Kind, "attempt": execution.Attempt,
				"workerReleaseRevisionId": execution.WorkerReleaseRevisionID,
				"workerReleaseChannel":    execution.WorkerReleaseChannel,
				"provider":                provider, "providerRuntimeBindingId": resources.BindingID,
				"remoteWorkspaceId":                     resources.WorkspaceID,
				"workspaceMaterializationId":            resources.MaterializationID,
				"workspaceMaterializationIncarnationId": resources.IncarnationID,
				"workspaceLayoutVersion":                resources.LayoutVersion,
				"restoreCheckpointId":                   resources.RestoreCheckpointID,
			}, Headers: map[string]any{"eventVersion": 1}, AvailableAt: now, CreatedAt: now,
		}); err != nil {
			return QueuedSessionOperation{}, problem.Wrap(500, "execution_outbox_create_rejected", "Execution dispatch could not be queued atomically.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "session." + request.Type + "_requested", ResourceType: "agent_session", ResourceID: &session.ID,
			OrganizationID: &session.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"turnId": turn.ID, "executionId": execution.ID, "controlCommandId": command.ID},
		}); err != nil {
			return QueuedSessionOperation{}, err
		}
		return QueuedSessionOperation{
			Type: request.Type,
			Turn: sessions.Turn{
				ID: turn.ID, TenantID: turn.TenantID, SessionID: turn.SessionID, CreatedBy: turn.CreatedBy,
				Status: turn.Status, InputText: turn.InputText, TurnKind: turn.TurnKind,
				RuntimeMode: turn.RuntimeMode, InteractionMode: turn.InteractionMode,
				StartedAt: turn.StartedAt, CompletedAt: turn.CompletedAt, CreatedAt: turn.CreatedAt,
			},
			ExecutionID: execution.ID, ControlCommand: toControlCommand(command),
		}, nil
	})
	if err == nil && !result.Replayed && appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	return OperationResult[QueuedSessionOperation]{
		Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode,
	}, err
}

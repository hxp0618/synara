package workerreleases

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	apiidempotency "github.com/synara-ai/synara/services/control-plane/internal/idempotency"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

// Queued executions can be reassigned atomically before they acquire runtime
// lineage. Recovering executions cannot: they retain prior work and interaction
// identity until their replacement Generation is ready.
var releaseTransitionBlockingExecutionStatuses = []string{
	"leased",
	"running",
	"waiting-for-approval",
	"recovering",
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) List(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
) (Overview, error) {
	if err := requireTenant(principal, tenantID); err != nil {
		return Overview{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return Overview{}, err
	}
	if _, err := loadTenantTarget(ctx, s.db, tenantID, targetID, false); err != nil {
		return Overview{}, err
	}

	models := make([]persistence.WorkerReleaseRevision, 0)
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_target_id = ?", tenantID, targetID).
		Order("revision DESC, id").Find(&models).Error; err != nil {
		return Overview{}, problem.Wrap(500, "worker_releases_load_failed", "Failed to load Worker release revisions.", err)
	}
	manifestIDs := make([]uuid.UUID, 0, len(models))
	for _, model := range models {
		manifestIDs = append(manifestIDs, model.WorkerManifestID)
	}
	manifests := make([]persistence.WorkerManifest, 0, len(manifestIDs))
	if len(manifestIDs) > 0 {
		if err := s.db.WithContext(ctx).Where("id IN ?", manifestIDs).Find(&manifests).Error; err != nil {
			return Overview{}, problem.Wrap(500, "worker_release_manifests_load_failed", "Failed to load Worker release manifests.", err)
		}
	}
	manifestByID := make(map[uuid.UUID]persistence.WorkerManifest, len(manifests))
	for _, manifest := range manifests {
		manifestByID[manifest.ID] = manifest
	}
	revisions := make([]Revision, 0, len(models))
	for _, model := range models {
		manifest, found := manifestByID[model.WorkerManifestID]
		if !found {
			return Overview{}, problem.New(500, "worker_release_manifest_missing", "Worker release manifest is missing.")
		}
		revisions = append(revisions, projectRevisionWithManifest(model, manifest))
	}

	var policyView *Policy
	var policy persistence.WorkerReleasePolicy
	policyErr := s.db.WithContext(ctx).Where("execution_target_id = ?", targetID).Take(&policy).Error
	if policyErr == nil {
		projected := projectPolicy(policy)
		policyView = &projected
	} else if !errors.Is(policyErr, gorm.ErrRecordNotFound) {
		return Overview{}, problem.Wrap(500, "worker_release_policy_lookup_failed", "Failed to load the Worker release policy.", policyErr)
	}

	transitionModels := make([]persistence.WorkerReleaseTransition, 0)
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_target_id = ?", tenantID, targetID).
		Order("policy_version DESC, id").Limit(200).Find(&transitionModels).Error; err != nil {
		return Overview{}, problem.Wrap(500, "worker_release_transitions_load_failed", "Failed to load Worker release history.", err)
	}
	transitions := make([]Transition, 0, len(transitionModels))
	for _, model := range transitionModels {
		transitions = append(transitions, projectTransition(model))
	}
	return Overview{Policy: policyView, Revisions: revisions, Transitions: transitions}, nil
}

func (s *Service) CreateRevision(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	input CreateRevisionInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[Revision], error) {
	if err := requireTenant(principal, tenantID); err != nil {
		return OperationResult[Revision]{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return OperationResult[Revision]{}, err
	}
	if err := requireIdempotencyKey(idempotencyKey); err != nil {
		return OperationResult[Revision]{}, err
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return OperationResult[Revision]{}, err
	}
	if input.WorkerManifestID == uuid.Nil {
		return OperationResult[Revision]{}, problem.New(400, "worker_manifest_required", "workerManifestId is required.")
	}
	input.Description = strings.TrimSpace(input.Description)
	if len(input.Description) > 2000 {
		return OperationResult[Revision]{}, problem.New(400, "invalid_worker_release_description", "description must not exceed 2000 characters.")
	}

	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "worker-release.revision.create", SuccessStatus: 201,
		Request: map[string]any{
			"executionTargetId": targetID, "workerManifestId": input.WorkerManifestID,
			"description": input.Description,
		},
	}, func(tx *gorm.DB) (Revision, error) {
		target, err := loadTenantTarget(ctx, tx, tenantID, targetID, true)
		if err != nil {
			return Revision{}, err
		}
		var observed int64
		observationQuery := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("worker_instances.current_manifest_id = ?", input.WorkerManifestID)
		if target.Kind == "docker" || target.Kind == "kubernetes" {
			observationQuery = observationQuery.
				Joins("JOIN execution_targets AS observed_target ON observed_target.id = worker_instances.execution_target_id").
				Where("observed_target.tenant_id = ?", tenantID)
		} else {
			observationQuery = observationQuery.Where("worker_instances.execution_target_id = ?", targetID)
		}
		if err := observationQuery.Count(&observed).Error; err != nil {
			return Revision{}, problem.Wrap(500, "worker_release_observation_failed", "Failed to inspect Worker manifest observations.", err)
		}
		if observed == 0 {
			return Revision{}, problem.New(404, "worker_manifest_not_found", "Worker manifest not found.")
		}
		var manifest persistence.WorkerManifest
		if err := tx.WithContext(ctx).Where("id = ?", input.WorkerManifestID).Take(&manifest).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return Revision{}, problem.New(404, "worker_manifest_not_found", "Worker manifest not found.")
		} else if err != nil {
			return Revision{}, problem.Wrap(500, "worker_manifest_lookup_failed", "Failed to load the Worker manifest.", err)
		}
		if (target.Kind == "docker" || target.Kind == "kubernetes") && manifest.ImageDigest == nil {
			return Revision{}, problem.New(409, "worker_release_image_digest_required", "Managed Docker and Kubernetes Worker releases require an immutable image digest.")
		}
		var existing persistence.WorkerReleaseRevision
		existingErr := tx.WithContext(ctx).
			Where("execution_target_id = ? AND worker_manifest_id = ?", targetID, input.WorkerManifestID).
			Take(&existing).Error
		if existingErr == nil {
			conflict := problem.New(409, "worker_release_manifest_already_registered", "Worker manifest already has an immutable release revision on this Execution Target.")
			conflict.Details = map[string]any{"releaseRevisionId": existing.ID, "revision": existing.Revision}
			return Revision{}, conflict
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return Revision{}, problem.Wrap(500, "worker_release_revision_lookup_failed", "Failed to inspect existing Worker release revisions.", existingErr)
		}
		var highest int64
		if err := tx.WithContext(ctx).Model(&persistence.WorkerReleaseRevision{}).
			Where("execution_target_id = ?", targetID).Select("COALESCE(MAX(revision), 0)").Scan(&highest).Error; err != nil {
			return Revision{}, problem.Wrap(500, "worker_release_revision_allocate_failed", "Failed to allocate the next Worker release revision.", err)
		}
		now := s.now()
		model := persistence.WorkerReleaseRevision{
			ID: uuid.New(), TenantID: tenantID, ExecutionTargetID: targetID,
			Revision: highest + 1, WorkerManifestID: input.WorkerManifestID,
			Description: input.Description, CreatedBy: principal.UserID, CreatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return Revision{}, problem.Wrap(409, "worker_release_revision_conflict", "Worker release revision creation conflicted with another request.", err)
		}
		metadata := map[string]any{
			"executionTargetId": targetID, "revision": model.Revision,
			"workerManifestId": input.WorkerManifestID, "description": input.Description,
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "worker_release.revision_created", ResourceType: "worker_release_revision", ResourceID: &model.ID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress, Metadata: metadata,
		}); err != nil {
			return Revision{}, problem.Wrap(500, "worker_release_audit_failed", "Worker release audit record could not be persisted.", err)
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "worker.release.revision-created", MessageKey: model.ID.String(),
			Payload: map[string]any{
				"tenantId": tenantID, "organizationId": target.OrganizationID,
				"executionTargetId": targetID, "releaseRevisionId": model.ID,
				"revision": model.Revision, "workerManifestId": input.WorkerManifestID,
				"createdBy": principal.UserID, "createdAt": now,
			},
		}); err != nil {
			return Revision{}, problem.Wrap(500, "worker_release_outbox_failed", "Worker release event could not be queued.", err)
		}
		return projectRevisionWithManifest(model, manifest), nil
	})
	if err != nil {
		return OperationResult[Revision]{}, err
	}
	return OperationResult[Revision]{Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode}, nil
}

func (s *Service) Promote(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, revisionID uuid.UUID,
	input PolicyChangeInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[Policy], error) {
	return s.changePolicy(ctx, principal, tenantID, targetID, revisionID, input, idempotencyKey, requestID, ipAddress, "promote")
}

func (s *Service) StartCanary(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, revisionID uuid.UUID,
	input PolicyChangeInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[Policy], error) {
	return s.changePolicy(ctx, principal, tenantID, targetID, revisionID, input, idempotencyKey, requestID, ipAddress, "canary")
}

func (s *Service) Rollback(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, revisionID uuid.UUID,
	input PolicyChangeInput,
	idempotencyKey, requestID, ipAddress string,
) (OperationResult[Policy], error) {
	return s.changePolicy(ctx, principal, tenantID, targetID, revisionID, input, idempotencyKey, requestID, ipAddress, "rollback")
}

func (s *Service) changePolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID, revisionID uuid.UUID,
	input PolicyChangeInput,
	idempotencyKey, requestID, ipAddress, action string,
) (OperationResult[Policy], error) {
	if err := requireTenant(principal, tenantID); err != nil {
		return OperationResult[Policy]{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return OperationResult[Policy]{}, err
	}
	if err := requireIdempotencyKey(idempotencyKey); err != nil {
		return OperationResult[Policy]{}, err
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return OperationResult[Policy]{}, err
	}
	if revisionID == uuid.Nil {
		return OperationResult[Policy]{}, problem.New(400, "worker_release_revision_required", "Worker release revision is required.")
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if len(input.Reason) == 0 || len(input.Reason) > 2000 {
		return OperationResult[Policy]{}, problem.New(400, "invalid_worker_release_reason", "reason must contain between 1 and 2000 characters.")
	}
	if input.ExpectedPolicyVersion < 0 {
		return OperationResult[Policy]{}, problem.New(400, "invalid_worker_release_policy_version", "expectedPolicyVersion must be zero or greater.")
	}
	if action == "canary" && (input.CanaryPercent < 1 || input.CanaryPercent > 100) {
		return OperationResult[Policy]{}, problem.New(400, "invalid_worker_release_canary_percent", "canaryPercent must be between 1 and 100.")
	}
	if action != "canary" && input.CanaryPercent != 0 {
		return OperationResult[Policy]{}, problem.New(400, "invalid_worker_release_canary_percent", "canaryPercent is only valid for a canary transition.")
	}

	result, err := apiidempotency.Execute(ctx, s.db, apiidempotency.Scope{
		TenantID: tenantID, ActorID: principal.UserID, Key: idempotencyKey,
		Operation: "worker-release.policy." + action, SuccessStatus: 200,
		Request: map[string]any{
			"executionTargetId": targetID, "releaseRevisionId": revisionID,
			"expectedPolicyVersion": input.ExpectedPolicyVersion,
			"reason":                input.Reason, "canaryPercent": input.CanaryPercent,
		},
	}, func(tx *gorm.DB) (Policy, error) {
		target, err := loadTenantTarget(ctx, tx, tenantID, targetID, true)
		if err != nil {
			return Policy{}, err
		}
		revision, err := loadTargetRevision(ctx, tx, tenantID, targetID, revisionID)
		if err != nil {
			return Policy{}, err
		}

		var current persistence.WorkerReleasePolicy
		policyErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_target_id = ?", targetID).Take(&current).Error
		policyFound := policyErr == nil
		if !policyFound && !errors.Is(policyErr, gorm.ErrRecordNotFound) {
			return Policy{}, problem.Wrap(500, "worker_release_policy_lock_failed", "Failed to lock the Worker release policy.", policyErr)
		}
		currentVersion := int64(0)
		if policyFound {
			currentVersion = current.PolicyVersion
		}
		if input.ExpectedPolicyVersion != currentVersion {
			conflict := problem.New(409, "worker_release_policy_version_conflict", "Worker release policy changed before this transition could be committed.")
			conflict.Details = map[string]any{
				"expectedPolicyVersion": input.ExpectedPolicyVersion, "currentPolicyVersion": currentVersion,
			}
			return Policy{}, conflict
		}

		var fromPromoted, fromCanary *uuid.UUID
		if policyFound {
			fromPromoted = uuidPointer(current.PromotedRevisionID)
			fromCanary = current.CanaryRevisionID
		}
		next := persistence.WorkerReleasePolicy{
			TenantID: tenantID, ExecutionTargetID: targetID, PolicyVersion: currentVersion + 1,
			UpdatedBy: principal.UserID, UpdatedAt: s.now(),
		}
		transitionAction := action
		switch action {
		case "promote":
			if policyFound {
				if current.CanaryRevisionID == nil || *current.CanaryRevisionID != revision.ID {
					return Policy{}, problem.New(409, "worker_release_revision_not_canary", "Only the active canary revision can be promoted.")
				}
			}
			next.PromotedRevisionID = revision.ID
		case "canary":
			if !policyFound {
				return Policy{}, problem.New(409, "worker_release_promoted_revision_required", "Promote an initial Worker release before starting a canary.")
			}
			if current.PromotedRevisionID == revision.ID {
				return Policy{}, problem.New(409, "worker_release_canary_matches_promoted", "Canary revision must differ from the promoted revision.")
			}
			promoted, err := loadTargetRevision(ctx, tx, tenantID, targetID, current.PromotedRevisionID)
			if err != nil {
				return Policy{}, err
			}
			if revision.Revision <= promoted.Revision {
				return Policy{}, problem.New(409, "worker_release_canary_not_newer", "Canary revision must be newer than the promoted revision.")
			}
			next.PromotedRevisionID = current.PromotedRevisionID
			next.CanaryRevisionID = &revision.ID
			next.CanaryPercent = input.CanaryPercent
		case "rollback":
			if !policyFound {
				return Policy{}, problem.New(409, "worker_release_promoted_revision_required", "No promoted Worker release exists to roll back.")
			}
			if current.CanaryRevisionID != nil && revision.ID == current.PromotedRevisionID {
				// Rolling back to the still-promoted revision is the explicit
				// abort-canary operation. It removes the candidate without
				// pretending a new image was promoted or mutating an immutable
				// revision.
				next.PromotedRevisionID = current.PromotedRevisionID
				transitionAction = "abort-canary"
				break
			}
			promoted, err := loadTargetRevision(ctx, tx, tenantID, targetID, current.PromotedRevisionID)
			if err != nil {
				return Policy{}, err
			}
			if revision.Revision >= promoted.Revision {
				return Policy{}, problem.New(409, "worker_release_rollback_not_older", "Rollback revision must be older than the promoted revision.")
			}
			next.PromotedRevisionID = revision.ID
		default:
			return Policy{}, problem.New(500, "worker_release_transition_invalid", "Worker release transition is invalid.")
		}
		if err := requireReleaseRevisionReady(ctx, tx, target, revision); err != nil {
			return Policy{}, err
		}
		if err := requireNoRetiredReleaseExecutions(ctx, tx, current, policyFound, next); err != nil {
			return Policy{}, err
		}

		if !policyFound {
			if err := tx.WithContext(ctx).Create(&next).Error; err != nil {
				return Policy{}, problem.Wrap(409, "worker_release_policy_conflict", "Worker release policy was created concurrently.", err)
			}
		} else {
			updated := tx.WithContext(ctx).Model(&persistence.WorkerReleasePolicy{}).
				Where("execution_target_id = ? AND policy_version = ?", targetID, currentVersion).
				Updates(map[string]any{
					"policy_version":       next.PolicyVersion,
					"promoted_revision_id": next.PromotedRevisionID,
					"canary_revision_id":   next.CanaryRevisionID,
					"canary_percent":       next.CanaryPercent,
					"updated_by":           next.UpdatedBy,
					"updated_at":           next.UpdatedAt,
				})
			if updated.Error != nil {
				return Policy{}, problem.Wrap(409, "worker_release_policy_conflict", "Worker release policy transition conflicted with another request.", updated.Error)
			}
			if updated.RowsAffected != 1 {
				return Policy{}, problem.New(409, "worker_release_policy_version_conflict", "Worker release policy changed before this transition could be committed.")
			}
		}

		if err := reassignUnleasedExecutions(ctx, tx, current, policyFound, next, action); err != nil {
			return Policy{}, err
		}
		if err := synchronizeTargetWorkers(ctx, tx, next, s.now()); err != nil {
			return Policy{}, err
		}
		requestIDPointer := optionalString(requestID)
		transition := persistence.WorkerReleaseTransition{
			ID: uuid.New(), TenantID: tenantID, ExecutionTargetID: targetID,
			PolicyVersion: next.PolicyVersion, Action: action,
			FromPromotedRevisionID: fromPromoted, FromCanaryRevisionID: fromCanary,
			ToPromotedRevisionID: next.PromotedRevisionID, ToCanaryRevisionID: next.CanaryRevisionID,
			CanaryPercent: next.CanaryPercent, Reason: input.Reason,
			ActorID: principal.UserID, RequestID: requestIDPointer, OccurredAt: next.UpdatedAt,
		}
		if err := tx.WithContext(ctx).Create(&transition).Error; err != nil {
			return Policy{}, problem.Wrap(500, "worker_release_transition_store_failed", "Worker release transition history could not be persisted.", err)
		}
		metadata := map[string]any{
			"executionTargetId": targetID, "policyVersion": next.PolicyVersion, "action": transitionAction,
			"fromPromotedRevisionId": fromPromoted, "fromCanaryRevisionId": fromCanary,
			"toPromotedRevisionId": next.PromotedRevisionID, "toCanaryRevisionId": next.CanaryRevisionID,
			"canaryPercent": next.CanaryPercent, "reason": input.Reason,
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action:       "worker_release." + transitionAuditAction(transitionAction),
			ResourceType: "execution_target", ResourceID: &targetID,
			OrganizationID: target.OrganizationID, RequestID: requestID, IPAddress: ipAddress, Metadata: metadata,
		}); err != nil {
			return Policy{}, problem.Wrap(500, "worker_release_audit_failed", "Worker release audit record could not be persisted.", err)
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "worker.release." + transitionOutboxTopic(transitionAction),
			MessageKey: targetID.String() + ":" + formatVersion(next.PolicyVersion),
			Payload: map[string]any{
				"tenantId": tenantID, "organizationId": target.OrganizationID,
				"executionTargetId": targetID, "policyVersion": next.PolicyVersion,
				"action": transitionAction, "fromPromotedRevisionId": fromPromoted,
				"fromCanaryRevisionId": fromCanary, "toPromotedRevisionId": next.PromotedRevisionID,
				"toCanaryRevisionId": next.CanaryRevisionID, "canaryPercent": next.CanaryPercent,
				"reason": input.Reason, "actorId": principal.UserID, "occurredAt": next.UpdatedAt,
			},
		}); err != nil {
			return Policy{}, problem.Wrap(500, "worker_release_outbox_failed", "Worker release event could not be queued.", err)
		}
		return projectPolicy(next), nil
	})
	if err != nil {
		return OperationResult[Policy]{}, err
	}
	return OperationResult[Policy]{Value: result.Value, Replayed: result.Replayed, StatusCode: result.StatusCode}, nil
}

func reassignUnleasedExecutions(
	ctx context.Context,
	tx *gorm.DB,
	current persistence.WorkerReleasePolicy,
	policyFound bool,
	next persistence.WorkerReleasePolicy,
	action string,
) error {
	query := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ? AND status IN ?", next.ExecutionTargetID, []string{"queued", "recovering"})
	updates := map[string]any{}
	switch action {
	case "promote", "rollback":
		updates["worker_release_revision_id"] = next.PromotedRevisionID
		updates["worker_release_channel"] = ChannelPromoted
	case "canary":
		if !policyFound || current.CanaryRevisionID == nil ||
			(next.CanaryRevisionID != nil && *current.CanaryRevisionID == *next.CanaryRevisionID) {
			return nil
		}
		query = query.Where("worker_release_revision_id = ? AND worker_release_channel = ?", *current.CanaryRevisionID, ChannelCanary)
		updates["worker_release_revision_id"] = next.PromotedRevisionID
		updates["worker_release_channel"] = ChannelPromoted
	default:
		return problem.New(500, "worker_release_transition_invalid", "Worker release transition is invalid.")
	}
	if err := query.Updates(updates).Error; err != nil {
		return problem.Wrap(500, "worker_release_execution_reassignment_failed", "Queued Executions could not be reassigned to the active Worker release.", err)
	}
	return nil
}

func requireNoRetiredReleaseExecutions(
	ctx context.Context,
	tx *gorm.DB,
	current persistence.WorkerReleasePolicy,
	policyFound bool,
	next persistence.WorkerReleasePolicy,
) error {
	if !policyFound {
		var count int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where(
				"execution_target_id = ? AND worker_release_revision_id IS NULL AND worker_release_channel IS NULL AND status IN ?",
				next.ExecutionTargetID, releaseTransitionBlockingExecutionStatuses,
			).Count(&count).Error; err != nil {
			return problem.Wrap(500, "worker_release_active_execution_probe_failed", "Failed to inspect active unmanaged Executions before the initial Worker release.", err)
		}
		if count > 0 {
			conflict := problem.New(
				409,
				"worker_release_active_executions",
				"Drain or finish active unmanaged Executions before establishing the initial Worker release policy.",
			)
			conflict.Details = map[string]any{
				"releaseChannel":   "unmanaged",
				"activeExecutions": count,
			}
			return conflict
		}
		return nil
	}
	type releasePair struct {
		revisionID uuid.UUID
		channel    string
	}
	currentPairs := []releasePair{{revisionID: current.PromotedRevisionID, channel: ChannelPromoted}}
	if current.CanaryRevisionID != nil {
		currentPairs = append(currentPairs, releasePair{revisionID: *current.CanaryRevisionID, channel: ChannelCanary})
	}
	nextPairs := map[releasePair]struct{}{
		{revisionID: next.PromotedRevisionID, channel: ChannelPromoted}: {},
	}
	if next.CanaryRevisionID != nil {
		nextPairs[releasePair{revisionID: *next.CanaryRevisionID, channel: ChannelCanary}] = struct{}{}
	}
	for _, pair := range currentPairs {
		if _, retained := nextPairs[pair]; retained {
			continue
		}
		var count int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where(
				"execution_target_id = ? AND worker_release_revision_id = ? AND worker_release_channel = ? AND status IN ?",
				next.ExecutionTargetID, pair.revisionID, pair.channel,
				releaseTransitionBlockingExecutionStatuses,
			).Count(&count).Error; err != nil {
			return problem.Wrap(500, "worker_release_active_execution_probe_failed", "Failed to inspect active Executions on the retiring Worker release.", err)
		}
		if count > 0 {
			conflict := problem.New(
				409,
				"worker_release_active_executions",
				"Drain or finish active Executions on the retiring Worker release before changing the release policy.",
			)
			conflict.Details = map[string]any{
				"releaseRevisionId": pair.revisionID,
				"releaseChannel":    pair.channel,
				"activeExecutions":  count,
			}
			return conflict
		}
	}
	return nil
}

func synchronizeTargetWorkers(
	ctx context.Context,
	tx *gorm.DB,
	policy persistence.WorkerReleasePolicy,
	now time.Time,
) error {
	revisions := make([]persistence.WorkerReleaseRevision, 0)
	if err := tx.WithContext(ctx).Where("execution_target_id = ?", policy.ExecutionTargetID).Find(&revisions).Error; err != nil {
		return problem.Wrap(500, "worker_release_revisions_load_failed", "Failed to load Worker release revisions.", err)
	}
	byManifest := make(map[uuid.UUID]persistence.WorkerReleaseRevision, len(revisions))
	for _, revision := range revisions {
		byManifest[revision.WorkerManifestID] = revision
	}
	workers := make([]persistence.WorkerInstance, 0)
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("execution_target_id = ?", policy.ExecutionTargetID).Order("id").Find(&workers).Error; err != nil {
		return problem.Wrap(500, "worker_release_workers_lock_failed", "Failed to lock Workers for release policy synchronization.", err)
	}
	checkedAt := now.UTC()
	for index := range workers {
		worker := &workers[index]
		var revision *persistence.WorkerReleaseRevision
		if worker.CurrentManifestID != nil {
			if matched, found := byManifest[*worker.CurrentManifestID]; found {
				revision = &matched
			}
		}
		if revision != nil && revision.ID == policy.PromotedRevisionID {
			channel := ChannelPromoted
			if err := updateWorkerReleaseState(ctx, tx, worker, &revision.ID, &channel, "active", nil, &checkedAt); err != nil {
				return err
			}
			continue
		}
		if revision != nil && policy.CanaryRevisionID != nil && revision.ID == *policy.CanaryRevisionID {
			channel := ChannelCanary
			if err := updateWorkerReleaseState(ctx, tx, worker, &revision.ID, &channel, "active", nil, &checkedAt); err != nil {
				return err
			}
			continue
		}
		reason := "Worker manifest is not selected by the active promoted or canary release policy."
		var revisionID *uuid.UUID
		if revision != nil {
			revisionID = &revision.ID
		}
		if err := updateWorkerReleaseState(ctx, tx, worker, revisionID, nil, "inactive", &reason, &checkedAt); err != nil {
			return err
		}
	}
	return nil
}

func requireOnlineReleaseWorker(ctx context.Context, tx *gorm.DB, targetID, manifestID uuid.UUID) error {
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where(
			"execution_target_id = ? AND current_manifest_id = ? AND status = ? AND administrative_status = ? AND compatibility_status = ?",
			targetID, manifestID, "online", "active", "compatible",
		).Count(&count).Error; err != nil {
		return problem.Wrap(500, "worker_release_pool_probe_failed", "Failed to inspect the Worker release pool.", err)
	}
	if count == 0 {
		return problem.New(409, "worker_release_no_online_workers", "Worker release has no online, compatible Worker on this Execution Target.")
	}
	return nil
}

func requireReleaseRevisionReady(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	revision persistence.WorkerReleaseRevision,
) error {
	if target.Kind != "docker" && target.Kind != "kubernetes" {
		return requireOnlineReleaseWorker(ctx, tx, target.ID, revision.WorkerManifestID)
	}
	var manifest persistence.WorkerManifest
	if err := tx.WithContext(ctx).Select("id", "image_digest").Where("id = ?", revision.WorkerManifestID).Take(&manifest).Error; err != nil {
		return problem.Wrap(500, "worker_release_manifest_lookup_failed", "Failed to load the Worker release manifest.", err)
	}
	if manifest.ImageDigest == nil || strings.TrimSpace(*manifest.ImageDigest) == "" {
		return problem.New(409, "worker_release_image_digest_required", "Managed Docker and Kubernetes Worker releases require an immutable image digest.")
	}
	return nil
}

func loadTenantTarget(
	ctx context.Context,
	db *gorm.DB,
	tenantID, targetID uuid.UUID,
	lock bool,
) (persistence.ExecutionTarget, error) {
	query := db.WithContext(ctx)
	if lock {
		query = persistence.WithLocking(query, "UPDATE", "")
	}
	var target persistence.ExecutionTarget
	err := query.Where("id = ? AND tenant_id = ?", targetID, tenantID).Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return target, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	if err != nil {
		return target, problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the Execution Target.", err)
	}
	if target.Status != "active" {
		return target, problem.New(409, "execution_target_unavailable", "Execution Target must be active for Worker release management.")
	}
	return target, nil
}

func loadTargetRevision(
	ctx context.Context,
	db *gorm.DB,
	tenantID, targetID, revisionID uuid.UUID,
) (persistence.WorkerReleaseRevision, error) {
	var revision persistence.WorkerReleaseRevision
	err := db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND execution_target_id = ?", revisionID, tenantID, targetID).
		Take(&revision).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return revision, problem.New(404, "worker_release_revision_not_found", "Worker release revision not found.")
	}
	if err != nil {
		return revision, problem.Wrap(500, "worker_release_revision_lookup_failed", "Failed to load the Worker release revision.", err)
	}
	return revision, nil
}

func projectRevisionWithManifest(model persistence.WorkerReleaseRevision, manifest persistence.WorkerManifest) Revision {
	return Revision{
		ID: model.ID, TenantID: model.TenantID, ExecutionTargetID: model.ExecutionTargetID,
		Revision: model.Revision, WorkerManifestID: model.WorkerManifestID,
		WorkerBuildVersion: manifest.WorkerBuildVersion, WorkerBuildGitSHA: manifest.WorkerBuildGitSHA,
		ImageDigest: manifest.ImageDigest, Description: model.Description,
		CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt,
	}
}

func projectPolicy(model persistence.WorkerReleasePolicy) Policy {
	return Policy{
		TenantID: model.TenantID, ExecutionTargetID: model.ExecutionTargetID,
		PolicyVersion: model.PolicyVersion, PromotedRevisionID: model.PromotedRevisionID,
		CanaryRevisionID: model.CanaryRevisionID, CanaryPercent: model.CanaryPercent,
		UpdatedBy: model.UpdatedBy, UpdatedAt: model.UpdatedAt,
	}
}

func projectTransition(model persistence.WorkerReleaseTransition) Transition {
	return Transition{
		ID: model.ID, TenantID: model.TenantID, ExecutionTargetID: model.ExecutionTargetID,
		PolicyVersion: model.PolicyVersion, Action: projectedTransitionAction(model),
		FromPromotedRevisionID: model.FromPromotedRevisionID, FromCanaryRevisionID: model.FromCanaryRevisionID,
		ToPromotedRevisionID: model.ToPromotedRevisionID, ToCanaryRevisionID: model.ToCanaryRevisionID,
		CanaryPercent: model.CanaryPercent, Reason: model.Reason, ActorID: model.ActorID,
		RequestID: model.RequestID, OccurredAt: model.OccurredAt,
	}
}

func projectedTransitionAction(model persistence.WorkerReleaseTransition) string {
	if model.Action == "rollback" && model.FromCanaryRevisionID != nil && model.FromPromotedRevisionID != nil &&
		*model.FromPromotedRevisionID == model.ToPromotedRevisionID && model.ToCanaryRevisionID == nil {
		return "abort-canary"
	}
	return model.Action
}

func requireTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

func requireIdempotencyKey(value string) error {
	if strings.TrimSpace(value) == "" {
		return problem.New(400, "idempotency_key_required", "Idempotency-Key is required for Worker release mutations.")
	}
	return nil
}

func normalizeRequestID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 160 {
		return "", problem.New(400, "invalid_request_id", "X-Request-ID must not exceed 160 characters.")
	}
	return value, nil
}

func transitionAuditAction(action string) string {
	switch action {
	case "promote":
		return "promoted"
	case "canary":
		return "canary_started"
	case "rollback":
		return "rolled_back"
	case "abort-canary":
		return "canary_aborted"
	default:
		return action
	}
}

func transitionOutboxTopic(action string) string {
	switch action {
	case "promote":
		return "promoted"
	case "canary":
		return "canary-started"
	case "rollback":
		return "rolled-back"
	case "abort-canary":
		return "canary-aborted"
	default:
		return action
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func uuidPointer(value uuid.UUID) *uuid.UUID { return &value }

func formatVersion(value int64) string {
	return strconv.FormatInt(value, 10)
}

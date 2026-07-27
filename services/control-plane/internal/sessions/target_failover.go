package sessions

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/leadership"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

const (
	FailoverReasonTargetUnreachable = "target-unreachable"
	FailoverReasonTargetDraining    = "target-draining"
	FailoverReasonRegionFailover    = "region-failover"
	FailoverReasonOperatorRequest   = "operator-request"
)

type TargetFailoverResult struct {
	FailoverAttemptID            uuid.UUID `json:"failoverAttemptId"`
	SourceExecutionID            uuid.UUID `json:"sourceExecutionId"`
	DestinationExecutionID       uuid.UUID `json:"destinationExecutionId"`
	SourceExecutionTargetID      uuid.UUID `json:"sourceExecutionTargetId"`
	DestinationExecutionTargetID uuid.UUID `json:"destinationExecutionTargetId"`
	DestinationRegion            string    `json:"destinationRegion"`
	DestinationClusterID         string    `json:"destinationClusterId"`
	Attempt                      int       `json:"attempt"`
}

type TargetFailoverFence struct {
	LeaseName    string
	HolderID     string
	FencingToken int64
}

type TargetFailoverSweep struct {
	Candidates int            `json:"candidates"`
	Committed  int            `json:"committed"`
	Rejected   int            `json:"rejected"`
	Failures   map[string]int `json:"failures"`
}

// FailoverExecution creates a new immutable Execution attempt on another
// Target. It never mutates the source placement and only accepts lease-free
// states whose replay boundary can be proven from an immutable bundle.
func (s *Service) FailoverExecution(
	ctx context.Context,
	tenantID, sourceExecutionID uuid.UUID,
	leaderFence TargetFailoverFence,
	reason string,
) (TargetFailoverResult, error) {
	if tenantID == uuid.Nil || sourceExecutionID == uuid.Nil || leaderFence.FencingToken <= 0 {
		return TargetFailoverResult{}, problem.New(400, "invalid_target_failover_scope", "Tenant, source Execution, and leader fencing token are required.")
	}
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason != FailoverReasonTargetUnreachable && reason != FailoverReasonTargetDraining &&
		reason != FailoverReasonRegionFailover && reason != FailoverReasonOperatorRequest {
		return TargetFailoverResult{}, problem.New(400, "invalid_target_failover_reason", "Target failover reason is invalid.")
	}
	var result TargetFailoverResult
	var appended persistence.SessionEvent
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if _, err := leadership.AssertFenceInTransaction(
			ctx, tx, leaderFence.LeaseName, leaderFence.HolderID, leaderFence.FencingToken,
		); err != nil {
			if errors.Is(err, leadership.ErrFenceInactive) {
				return problem.New(409, "target_failover_leadership_lost", "The target-failover leadership epoch is no longer active.")
			}
			return problem.Wrap(500, "target_failover_leadership_check_failed", "The target-failover leadership epoch could not be verified.", err)
		}
		// Lock the shared admission authority before any Session row so failover,
		// turn creation, resume, and quota updates use one deadlock-safe order.
		// Queued/recovering failover is a capacity-neutral replacement; a
		// suspended source is checked later because it had released its slot.
		if err := s.lockExecutionQuotaAuthority(ctx, tx, tenantID); err != nil {
			return err
		}
		var source persistence.AgentExecution
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, sourceExecutionID).Take(&source).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "target_failover_source_not_found", "Source Execution not found.")
		}
		if err != nil {
			return problem.Wrap(500, "target_failover_source_load_failed", "Source Execution could not be loaded.", err)
		}
		var session persistence.AgentSession
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, source.SessionID).
			Take(&session).Error; err != nil {
			return problem.Wrap(409, "target_failover_session_unavailable", "Source Session is not active.", err)
		}
		if session.ExecutionTargetGroupID == nil {
			return problem.New(409, "target_failover_group_required", "Session is pinned to one Execution Target and cannot fail over globally.")
		}
		if source.ExecutionTargetID != session.ExecutionTargetID {
			return problem.New(409, "target_failover_source_stale", "Source Execution is no longer attached to the Session Target authority.")
		}
		var group persistence.ExecutionTargetGroup
		if err := tx.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, *session.ExecutionTargetGroupID).
			Take(&group).Error; err != nil {
			return problem.Wrap(409, "target_failover_group_unavailable", "Execution Target Group is unavailable.", err)
		}
		if err := requireFailoverSafeSource(ctx, tx, source); err != nil {
			return err
		}
		var leaseCount int64
		if err := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
			Where("tenant_id = ? AND execution_id = ?", tenantID, source.ID).Count(&leaseCount).Error; err != nil {
			return problem.Wrap(500, "target_failover_lease_probe_failed", "Source Worker Lease could not be inspected.", err)
		}
		if leaseCount != 0 || source.WorkerID != nil {
			return problem.New(409, "target_failover_source_not_fenced", "Source Execution still has a Worker identity or Lease.")
		}
		if source.Status == "suspended" {
			if err := s.requireExecutionQuotaAdmissionAfterAuthorityLock(ctx, tx, tenantID, ExecutionQuotaAdmission{
				ProjectID: session.ProjectID, SessionID: session.ID,
				AutomationID: source.AutomationID, QuotaUnits: source.QuotaUnits,
			}); err != nil {
				return err
			}
		}

		var sourceBundle *persistence.ExecutionRecoveryBundle
		var bundle persistence.ExecutionRecoveryBundle
		bundleErr := tx.WithContext(ctx).
			Where("tenant_id = ? AND execution_id = ?", tenantID, source.ID).
			Order("generation DESC").Take(&bundle).Error
		if bundleErr == nil {
			sourceBundle = &bundle
		} else if !errors.Is(bundleErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "target_failover_bundle_load_failed", "Source Recovery Bundle could not be loaded.", bundleErr)
		}
		if source.Generation > 0 && sourceBundle == nil {
			return problem.New(409, "target_failover_bundle_required", "A previously claimed Execution requires an immutable Recovery Bundle before cross-Target failover.")
		}
		failoverAuthority, err := loadFailoverReplicationAuthority(ctx, tx, tenantID, session, source, sourceBundle)
		if err != nil {
			return err
		}

		preferredRegions := make([]string, 0, 1)
		if session.PreferredExecutionRegion != nil {
			preferredRegions = append(preferredRegions, *session.PreferredExecutionRegion)
		}
		sourceRegion := ""
		if source.SelectedRegion != nil {
			sourceRegion = *source.SelectedRegion
		}
		routeRequest := routing.SelectRequest{
			TenantID: tenantID, OrganizationID: session.OrganizationID,
			TargetGroupID: *session.ExecutionTargetGroupID, Provider: session.Provider, PreferredRegions: preferredRegions,
			ExcludedTargetIDs:   []uuid.UUID{source.ExecutionTargetID},
			SourceRegion:        sourceRegion,
			SourceClusterID:     valueOrEmpty(source.SelectedClusterID),
			SourceDRDomain:      failoverAuthority.SourceDRDomain,
			ReplicatedThroughAt: failoverAuthority.ReplicatedThroughAt,
			RequiredDRStores:    failoverAuthority.RequiredStores,
			DisasterRecovery:    true,
		}
		launchTargetPlan, err := SelectExecutionLaunchTarget(
			ctx,
			tx,
			nil,
			&routeRequest,
			ExecutionLaunchPolicyScope{
				TenantID: tenantID, OrganizationID: session.OrganizationID, Provider: session.Provider,
			},
			session.WarmPoolMode,
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
					session.Provider,
					false,
					"start-session",
					"send-turn",
				)
			},
		)
		if err != nil {
			return err
		}
		selection := *launchTargetPlan.RoutingSelection
		if s.targetFailoverBeforeCommitValidation != nil {
			if err := s.targetFailoverBeforeCommitValidation(ctx, tx, routeRequest, selection); err != nil {
				return err
			}
		}
		selection, err = routing.NewService(s.db).LockSelectionForCommit(ctx, tx, routeRequest, selection)
		if err != nil {
			return err
		}
		launchTargetPlan.Target = selection.Target
		launchTargetPlan.RoutingSelection = &selection
		launchTargetPlan.SchedulingPolicySnapshot = selection.SchedulingPolicySnapshot
		var committed int64
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionFailoverAttempt{}).
			Where("tenant_id = ? AND session_id = ? AND turn_id = ? AND status = ?", tenantID, source.SessionID, source.TurnID, "committed").
			Count(&committed).Error; err != nil {
			return problem.Wrap(500, "target_failover_count_failed", "Previous failovers could not be counted.", err)
		}
		if committed >= int64(selection.Group.MaxFailoverAttempts) {
			return problem.New(409, "target_failover_limit_reached", "The Target Group failover limit has been reached for this Turn.")
		}

		now := s.now()
		failover := persistence.ExecutionFailoverAttempt{
			ID: uuid.New(), TenantID: tenantID, SessionID: source.SessionID, TurnID: source.TurnID,
			TargetGroupID: selection.Group.ID, SourceExecutionID: source.ID,
			SourceExecutionTargetID:      source.ExecutionTargetID,
			DestinationExecutionTargetID: selection.Target.ID, SourceGeneration: source.Generation,
			LeaderFencingToken: leaderFence.FencingToken, Reason: reason, Status: "planned", RequestedAt: now,
		}
		if sourceBundle != nil {
			bundleID := sourceBundle.ID
			failover.SourceRecoveryBundleID = &bundleID
		}
		if err := tx.WithContext(ctx).Create(&failover).Error; err != nil {
			return problem.Wrap(409, "target_failover_plan_conflict", "Another failover already owns the source Execution.", err)
		}

		failureCode := "target_failover"
		failureMessage := "Execution was fenced and superseded by a cross-Target disaster-recovery attempt."
		sourceUpdate := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND status = ? AND generation = ?", tenantID, source.ID, source.Status, source.Generation).
			Updates(map[string]any{
				"status": "interrupted", "next_recovery_reason": nil, "finished_at": now,
				"failure_code": failureCode, "failure_message": failureMessage,
			})
		if sourceUpdate.Error != nil || sourceUpdate.RowsAffected != 1 {
			return problem.Wrap(409, "target_failover_source_conflict", "Source Execution changed while it was being fenced.", sourceUpdate.Error)
		}

		if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ? AND execution_target_id = ?", tenantID, session.ID, source.ExecutionTargetID).
			Updates(map[string]any{
				"execution_target_id": selection.Target.ID, "routing_policy_version": selection.Group.Version,
				"resource_state": "restoring", "resource_idle_since": nil, "updated_at": now,
			}).Error; err != nil {
			return problem.Wrap(409, "target_failover_session_update_failed", "Session Target authority could not be moved.", err)
		}
		session.ExecutionTargetID = selection.Target.ID
		session.ResourceState = "restoring"
		session.ResourceIdleSince = nil
		version := selection.Group.Version
		session.RoutingPolicyVersion = &version
		resources, err := s.ensureRuntimeResources(ctx, tx, &session)
		if err != nil {
			return err
		}

		provider := session.Provider
		destination := persistence.AgentExecution{
			ID: uuid.New(), TenantID: tenantID, SessionID: source.SessionID, TurnID: source.TurnID,
			AutomationID: source.AutomationID, QueueClass: source.QueueClass,
			QueuePriority: source.QueuePriority, QuotaUnits: source.QuotaUnits,
			Attempt: source.Attempt + 1, Status: "queued", ExecutionTargetID: selection.Target.ID,
			TargetKind: selection.Target.Kind, Provider: &provider,
			ProviderRuntimeBindingID: &resources.BindingID, RemoteWorkspaceID: &resources.WorkspaceID,
			WorkspaceMaterializationID: &resources.MaterializationID, RestoreCheckpointID: resources.RestoreCheckpointID,
			WarmPoolModeSnapshot: session.WarmPoolMode, Generation: 0, RequestedBy: source.RequestedBy,
			QueuedAt: now, PredecessorExecutionID: &source.ID,
		}
		if sourceBundle != nil {
			destination.Status = "recovering"
			recoveryReason := "disaster-recovery"
			destination.NextRecoveryReason = &recoveryReason
		}
		scheduled, err := CreateScheduledExecution(ctx, tx, destination, launchTargetPlan, now)
		if err != nil {
			return err
		}
		destination = scheduled.Execution
		decision := scheduled.Decision

		interactionMove := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND execution_id = ? AND delivery_status IN ?", tenantID, source.ID, []string{"not-ready", "resume-recorded"}).
			Update("execution_id", destination.ID)
		if interactionMove.Error != nil {
			return problem.Wrap(409, "target_failover_interaction_move_failed", "Unbound Interaction state could not be moved to the successor.", interactionMove.Error)
		}
		if err := tx.WithContext(ctx).Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND session_id = ? AND id = ?", tenantID, source.SessionID, source.TurnID).
			Updates(map[string]any{"status": "queued", "completed_at": nil}).Error; err != nil {
			return problem.Wrap(409, "target_failover_turn_update_failed", "Turn could not be queued on the successor.", err)
		}

		failover.DestinationExecutionID = &destination.ID
		failover.Status = "committed"
		failover.CompletedAt = &now
		commit := tx.WithContext(ctx).Model(&persistence.ExecutionFailoverAttempt{}).
			Where("tenant_id = ? AND id = ? AND status = ?", tenantID, failover.ID, "planned").
			Updates(map[string]any{
				"destination_execution_id": destination.ID, "status": "committed", "completed_at": now,
			})
		if commit.Error != nil || commit.RowsAffected != 1 {
			return problem.Wrap(409, "target_failover_commit_conflict", "Failover audit could not be committed.", commit.Error)
		}

		eventPayload := map[string]any{
			"turnId": source.TurnID, "sourceExecutionId": source.ID,
			"destinationExecutionId":       destination.ID,
			"sourceExecutionTargetId":      source.ExecutionTargetID,
			"destinationExecutionTargetId": destination.ExecutionTargetID,
			"destinationRegion":            selection.Member.Region, "destinationClusterId": selection.Member.ClusterID,
			"placementRegion": destination.PlacementRegion, "placementClusterId": destination.PlacementClusterID,
			"tenantSchedulingPolicyVersion":       destination.TenantSchedulingPolicyVersion,
			"tenantSchedulingPolicyDigest":        destination.TenantSchedulingPolicyDigest,
			"organizationSchedulingPolicyVersion": destination.OrganizationSchedulingPolicyVersion,
			"organizationSchedulingPolicyDigest":  destination.OrganizationSchedulingPolicyDigest,
			"schedulingDecisionId":                decision.ID,
			"schedulingAlgorithmVersion":          decision.AlgorithmVersion,
			"schedulingEvidenceCompleteness":      decision.EvidenceCompleteness,
			"schedulingCandidateSetSha256":        decision.CandidateSetSHA256,
			"leaderFencingToken":                  leaderFence.FencingToken, "reason": reason,
			"sourceRecoveryBundleId": failover.SourceRecoveryBundleID,
			"sourceDrDomain":         failoverAuthority.SourceDRDomain,
		}
		outboxPayload := map[string]any{
			"tenantId": tenantID, "sessionId": source.SessionID, "turnId": source.TurnID,
			"sourceExecutionId": source.ID, "executionId": destination.ID,
			"sourceExecutionTargetId": source.ExecutionTargetID,
			"executionTargetId":       destination.ExecutionTargetID,
			"targetGroupId":           selection.Group.ID, "targetGroupVersion": selection.Group.Version,
			"targetGroupMemberVersion": selection.Member.Version,
			"selectedRegion":           selection.Member.Region, "selectedClusterId": selection.Member.ClusterID,
			"placementRegion": destination.PlacementRegion, "placementClusterId": destination.PlacementClusterID,
			"tenantSchedulingPolicyVersion":       destination.TenantSchedulingPolicyVersion,
			"tenantSchedulingPolicyDigest":        destination.TenantSchedulingPolicyDigest,
			"organizationSchedulingPolicyVersion": destination.OrganizationSchedulingPolicyVersion,
			"organizationSchedulingPolicyDigest":  destination.OrganizationSchedulingPolicyDigest,
			"schedulingDecisionId":                decision.ID,
			"schedulingAlgorithmVersion":          decision.AlgorithmVersion,
			"schedulingEvidenceCompleteness":      decision.EvidenceCompleteness,
			"schedulingCandidateSetSha256":        decision.CandidateSetSHA256,
			"leaderFencingToken":                  leaderFence.FencingToken, "reason": reason,
			"sourceDrDomain": failoverAuthority.SourceDRDomain,
		}
		eventPayload = scheduled.MergeSchedulingEvidencePayload(eventPayload)
		outboxPayload = scheduled.MergeSchedulingEvidencePayload(outboxPayload)
		if !failoverAuthority.ReplicatedThroughAt.IsZero() {
			eventPayload["replicatedThroughAt"] = failoverAuthority.ReplicatedThroughAt
			outboxPayload["replicatedThroughAt"] = failoverAuthority.ReplicatedThroughAt
		}
		if requiredStores := failoverAuthority.RequiredStores.Names(); len(requiredStores) != 0 {
			eventPayload["requiredStores"] = requiredStores
			outboxPayload["requiredStores"] = requiredStores
		}
		if selection.DRReadiness != nil {
			authority := map[string]any{
				"version":             selection.DRReadiness.Version,
				"sourceDrDomain":      selection.DRReadiness.SourceDRDomain,
				"drDomain":            selection.DRReadiness.DRDomain,
				"replicatedThroughAt": selection.DRReadiness.ReplicatedThroughAt,
				"artifactsReady":      selection.DRReadiness.ArtifactsReady,
				"checkpointsReady":    selection.DRReadiness.CheckpointsReady,
				"memoryReady":         selection.DRReadiness.MemoryReady,
				"publisherIdentity":   selection.DRReadiness.PublisherIdentity,
				"observedAt":          selection.DRReadiness.ObservedAt,
				"expiresAt":           selection.DRReadiness.ExpiresAt,
			}
			eventPayload["destinationDrAuthority"] = authority
			outboxPayload["destinationDrAuthority"] = authority
		}
		appended, err = appendEvent(ctx, tx, &session, eventInput{
			EventType: "execution.failover-committed", ActorType: "system", ExecutionID: &destination.ID,
			Payload: eventPayload,
		})
		if err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "execution.failover-committed", MessageKey: destination.ID.String(),
			Payload: outboxPayload,
			Headers: map[string]any{"eventVersion": 1}, AvailableAt: now, CreatedAt: now,
		}); err != nil {
			return problem.Wrap(409, "target_failover_outbox_failed", "Destination dispatch could not be queued atomically.", err)
		}

		result = TargetFailoverResult{
			FailoverAttemptID: failover.ID, SourceExecutionID: source.ID, DestinationExecutionID: destination.ID,
			SourceExecutionTargetID:      source.ExecutionTargetID,
			DestinationExecutionTargetID: destination.ExecutionTargetID,
			DestinationRegion:            selection.Member.Region, DestinationClusterID: selection.Member.ClusterID,
			Attempt: destination.Attempt,
		}
		return nil
	})
	if err != nil {
		return TargetFailoverResult{}, err
	}
	s.events.publish(toEvent(appended))
	return result, nil
}

// ReconcileTargetFailovers finds only already lease-free recovery boundaries.
// Active Workers are fenced by their own Lease recovery before this controller
// is allowed to create a cross-Target successor.
func (s *Service) ReconcileTargetFailovers(
	ctx context.Context,
	leaderFence TargetFailoverFence,
	limit int,
) (TargetFailoverSweep, error) {
	if leaderFence.FencingToken <= 0 {
		return TargetFailoverSweep{}, problem.New(400, "target_failover_leader_required", "A current leader fencing token is required.")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	now := s.now()
	var candidates []struct {
		TenantID    uuid.UUID
		ExecutionID uuid.UUID
		Reason      string
	}
	err := s.db.WithContext(ctx).Raw(`
		SELECT execution.tenant_id, execution.id AS execution_id,
		  CASE
		    WHEN EXISTS (
		      SELECT 1
		      FROM execution_location_outages AS outage
		      WHERE outage.tenant_id = execution.tenant_id
		        AND outage.region = execution.selected_region
		        AND (outage.cluster_id = '' OR outage.cluster_id = execution.selected_cluster_id)
		        AND outage.observed_at <= ?
		        AND outage.expires_at > ?
		    ) THEN ?
		    WHEN target.status <> 'active' THEN ?
		    ELSE ?
		  END AS reason
		FROM agent_executions AS execution
		JOIN agent_sessions AS session
		  ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id
		JOIN execution_targets AS target ON target.id = execution.execution_target_id
		LEFT JOIN execution_target_health AS health ON health.execution_target_id = target.id
		WHERE session.execution_target_group_id IS NOT NULL
		  AND session.archived_at IS NULL
		  AND execution.status IN ('queued', 'recovering', 'suspended')
		  AND execution.worker_id IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM worker_leases AS lease
		    WHERE lease.tenant_id = execution.tenant_id AND lease.execution_id = execution.id
		  )
		  AND (
		    EXISTS (
		      SELECT 1
		      FROM execution_location_outages AS outage
		      WHERE outage.tenant_id = execution.tenant_id
		        AND outage.region = execution.selected_region
		        AND (outage.cluster_id = '' OR outage.cluster_id = execution.selected_cluster_id)
		        AND outage.observed_at <= ?
		        AND outage.expires_at > ?
		    )
		    OR
		    target.status <> 'active' OR health.execution_target_id IS NULL
		    OR health.status IN ('unreachable', 'unknown') OR health.expires_at <= ?
		  )
		ORDER BY execution.queued_at, execution.id
		LIMIT ?`, now, now, FailoverReasonRegionFailover, FailoverReasonTargetDraining, FailoverReasonTargetUnreachable, now, now, now, limit).Scan(&candidates).Error
	if err != nil {
		return TargetFailoverSweep{}, problem.Wrap(500, "target_failover_candidates_load_failed", "Failover candidates could not be loaded.", err)
	}
	summary := TargetFailoverSweep{Candidates: len(candidates), Failures: make(map[string]int)}
	for _, candidate := range candidates {
		if _, err := s.FailoverExecution(ctx, candidate.TenantID, candidate.ExecutionID, leaderFence, candidate.Reason); err != nil {
			summary.Rejected++
			code := problemCode(err)
			if code == "" {
				code = "unknown"
			}
			summary.Failures[code]++
			continue
		}
		summary.Committed++
	}
	return summary, nil
}

func requireFailoverSafeSource(ctx context.Context, tx *gorm.DB, source persistence.AgentExecution) error {
	switch source.Status {
	case "queued":
		if source.Generation != 0 {
			return problem.New(409, "target_failover_queued_generation_invalid", "Only an unclaimed queued Execution can move without a Recovery Bundle.")
		}
	case "suspended":
		if source.NextRecoveryReason == nil || *source.NextRecoveryReason != "suspend-resume" {
			return problem.New(409, "target_failover_suspend_boundary_missing", "Suspended Execution lacks an exact resume boundary.")
		}
	case "recovering":
		if source.ProviderResumeStrategySnapshot != "authoritative-history" &&
			(source.NextRecoveryReason == nil || *source.NextRecoveryReason != "suspend-resume") {
			return problem.New(409, "target_failover_resume_strategy_unsafe", "Recovering Execution does not have a portable authoritative recovery strategy.")
		}
	default:
		return problem.New(409, "target_failover_source_active", "Source Execution must reach a lease-free queued, recovering, or suspended boundary first.")
	}
	var boundInteractions int64
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND execution_id = ? AND delivery_status IN ?", source.TenantID, source.ID,
			[]string{"pending", "delivered", "failed", "resume-bound", "outcome-unknown"}).
		Count(&boundInteractions).Error; err != nil {
		return problem.Wrap(500, "target_failover_interaction_probe_failed", "Interaction recovery boundary could not be inspected.", err)
	}
	if boundInteractions > 0 {
		return problem.New(409, "target_failover_interaction_outcome_unresolved", "Delivered or recovery-bound Interaction state cannot move to another Execution attempt.")
	}
	var activeCommands int64
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ? AND status IN ?", source.TenantID, source.ID,
			[]string{"pending", "delivered"}).Count(&activeCommands).Error; err != nil {
		return problem.Wrap(500, "target_failover_command_probe_failed", "Control Command recovery boundary could not be inspected.", err)
	}
	if activeCommands > 0 {
		return problem.New(409, "target_failover_command_outcome_unresolved", "Delivered or pending Control Commands must reach a terminal outcome before failover.")
	}
	return nil
}

func problemCode(err error) string {
	var item *problem.Error
	if errors.As(err, &item) {
		return item.Code
	}
	return ""
}

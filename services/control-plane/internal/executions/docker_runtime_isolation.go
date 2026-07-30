package executions

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Service) bindDockerRuntimeIsolationDecisionLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	observedAt time.Time,
) error {
	if execution.TargetKind != "docker" || execution.Generation <= 0 {
		return nil
	}
	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "organization_id", "updated_at").
		Where("id = ? AND tenant_id = ? AND kind = ?", execution.ExecutionTargetID, execution.TenantID, "docker").
		Take(&target).Error; err != nil {
		return problem.Wrap(500, "runtime_isolation_target_load_failed", "The Docker runtime isolation Target could not be loaded.", err)
	}
	var observation persistence.ExecutionTargetRuntimeIsolationObservation
	err := tx.WithContext(ctx).
		Where("execution_target_id = ?", execution.ExecutionTargetID).
		Take(&observation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(503, "runtime_isolation_observation_unavailable", "The Docker Target has no current runtime isolation observation.")
	}
	if err != nil {
		return problem.Wrap(500, "runtime_isolation_observation_load_failed", "The Docker runtime isolation observation could not be loaded.", err)
	}
	if !observation.ExpiresAt.After(observedAt) || observation.UpdatedAt.Before(target.UpdatedAt) {
		return problem.New(503, "runtime_isolation_observation_stale", "The Docker Target runtime isolation observation is stale.")
	}
	var releasePolicy persistence.WorkerReleasePolicy
	err = tx.WithContext(ctx).
		Select("updated_at").
		Where("execution_target_id = ?", execution.ExecutionTargetID).
		Take(&releasePolicy).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Wrap(500, "worker_release_policy_lookup_failed", "Worker release runtime compatibility could not be loaded.", err)
	}
	if err == nil && observation.UpdatedAt.Before(releasePolicy.UpdatedAt) {
		return problem.New(503, "runtime_isolation_observation_stale", "The Docker Target runtime isolation observation predates its active Worker release.")
	}
	if (observation.State != "available" && observation.State != "degraded") ||
		(observation.Decision != "selected" && observation.Decision != "fallback") ||
		observation.RequestedProfile != "single-tenant-trusted-v1" ||
		observation.EffectiveRuntime == nil ||
		(*observation.EffectiveRuntime != "runc" && *observation.EffectiveRuntime != "gvisor") ||
		observation.EffectiveProfile == nil || *observation.EffectiveProfile != "single-tenant-trusted-v1" ||
		!validRuntimeIsolationPolicySource(observation.PolicySource) ||
		!validDockerRequestedRuntime(observation.RequestedRuntime) {
		return problem.New(503, "runtime_isolation_observation_invalid", "The Docker Target runtime isolation observation is incomplete.")
	}

	row := persistence.ExecutionRuntimeIsolationDecision{
		TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: execution.Generation,
		ExecutionTargetID: execution.ExecutionTargetID, AllocationBackend: "docker-engine",
		RequestedRuntime: observation.RequestedRuntime, RequestedProfile: observation.RequestedProfile,
		EffectiveRuntime: dockerIsolationStringPointer(*observation.EffectiveRuntime),
		EffectiveProfile: dockerIsolationStringPointer(*observation.EffectiveProfile),
		PolicySource:     observation.PolicySource, Decision: observation.Decision,
		CreatedAt: observedAt.UTC(),
	}
	result := tx.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{
			{Name: "tenant_id"}, {Name: "execution_id"}, {Name: "generation"},
		}, DoNothing: true}).
		Create(&row)
	if result.Error != nil {
		return problem.Wrap(500, "runtime_isolation_decision_persist_failed", "The Docker Generation runtime isolation decision could not be persisted.", result.Error)
	}
	if result.RowsAffected == 1 {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: execution.TenantID, ActorType: "system",
			Action: "execution.runtime_isolation_decided", ResourceType: "execution", ResourceID: &execution.ID,
			OrganizationID: target.OrganizationID,
			RequestID:      "runtime-isolation:" + execution.ID.String() + ":" + strconv.FormatInt(execution.Generation, 10),
			Metadata: map[string]any{
				"generation": execution.Generation, "executionTargetId": execution.ExecutionTargetID,
				"allocationBackend": row.AllocationBackend, "requestedRuntime": row.RequestedRuntime,
				"requestedProfile": row.RequestedProfile, "effectiveRuntime": *row.EffectiveRuntime,
				"effectiveProfile": *row.EffectiveProfile, "policySource": row.PolicySource, "decision": row.Decision,
			},
		}); err != nil {
			return err
		}
		return nil
	}
	var stored persistence.ExecutionRuntimeIsolationDecision
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, execution.Generation).
		Take(&stored).Error; err != nil {
		return problem.Wrap(500, "runtime_isolation_decision_load_failed", "The Docker Generation runtime isolation decision could not be loaded.", err)
	}
	if stored.ExecutionTargetID != row.ExecutionTargetID || stored.AllocationBackend != row.AllocationBackend ||
		stored.RequestedRuntime != row.RequestedRuntime || stored.RequestedProfile != row.RequestedProfile ||
		stored.EffectiveRuntime == nil || *stored.EffectiveRuntime != *row.EffectiveRuntime ||
		stored.EffectiveProfile == nil || *stored.EffectiveProfile != *row.EffectiveProfile ||
		stored.PolicySource != row.PolicySource || stored.Decision != row.Decision {
		return problem.New(409, "runtime_isolation_decision_conflict", "The Docker Execution Generation is already bound to a different runtime isolation decision.")
	}
	return nil
}

func validRuntimeIsolationPolicySource(value string) bool {
	switch strings.TrimSpace(value) {
	case "legacy-native", "target-explicit", "target-auto", "lane-policy":
		return true
	default:
		return false
	}
}

func validDockerRequestedRuntime(value string) bool {
	switch strings.TrimSpace(value) {
	case "auto", "runc", "gvisor":
		return true
	default:
		return false
	}
}

func dockerIsolationStringPointer(value string) *string {
	copy := value
	return &copy
}

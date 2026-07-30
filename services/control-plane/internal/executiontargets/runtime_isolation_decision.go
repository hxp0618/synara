package executiontargets

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (r *KubernetesReconciler) persistRuntimeIsolationDecision(
	ctx context.Context,
	target persistence.ExecutionTarget,
	execution kubernetesExecution,
	configuration kubernetesTargetConfiguration,
) error {
	if target.TenantID == nil || configuration.RuntimeIsolationDecision == nil {
		return problem.New(
			500,
			"runtime_isolation_decision_missing",
			"The runtime isolation decision was not available before materialization.",
		)
	}
	decision := configuration.RuntimeIsolationDecision
	generation := execution.Generation + 1
	row := persistence.ExecutionRuntimeIsolationDecision{
		TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: generation,
		ExecutionTargetID: target.ID, AllocationBackend: configuration.AllocationBackend,
		RequestedRuntime: decision.RequestedRuntime, RequestedProfile: string(decision.RequestedProfile),
		PolicySource: decision.PolicySource, Decision: decision.Decision,
		CreatedAt: r.now(),
	}
	if decision.EffectiveRuntime != "" {
		row.EffectiveRuntime = runtimeIsolationStringPointer(decision.EffectiveRuntime)
		row.EffectiveProfile = runtimeIsolationStringPointer(string(decision.EffectiveProfile))
	}
	if decision.DecisionReasonCode != "" {
		row.DecisionReasonCode = runtimeIsolationStringPointer(decision.DecisionReasonCode)
	}
	if decision.RuntimeClassName != "" {
		row.RuntimeClassName = runtimeIsolationStringPointer(decision.RuntimeClassName)
	}
	if decision.AttestationDigest != "" {
		row.AttestationDigest = runtimeIsolationStringPointer(decision.AttestationDigest)
	}
	row.AttestedAt = cloneTimePointer(decision.AttestedAt)
	row.AttestationExpiresAt = cloneTimePointer(decision.AttestationExpiresAt)

	if err := validateRuntimeIsolationDecisionRow(row); err != nil {
		return err
	}
	if err := persistence.InTransaction(ctx, r.targets.db, func(tx *gorm.DB) error {
		result := tx.WithContext(ctx).
			Clauses(clause.OnConflict{Columns: []clause.Column{
				{Name: "tenant_id"}, {Name: "execution_id"}, {Name: "generation"},
			}, DoNothing: true}).
			Create(&row)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: row.TenantID, ActorType: "system",
			Action: "execution.runtime_isolation_decided", ResourceType: "execution", ResourceID: &row.ExecutionID,
			OrganizationID: target.OrganizationID,
			RequestID:      "runtime-isolation:" + row.ExecutionID.String() + ":" + strconv.FormatInt(row.Generation, 10),
			Metadata: map[string]any{
				"generation": row.Generation, "executionTargetId": row.ExecutionTargetID,
				"allocationBackend": row.AllocationBackend, "requestedRuntime": row.RequestedRuntime,
				"requestedProfile": row.RequestedProfile, "effectiveRuntime": row.EffectiveRuntime,
				"effectiveProfile": row.EffectiveProfile, "policySource": row.PolicySource, "decision": row.Decision,
			},
		})
	}); err != nil {
		return problem.Wrap(
			500,
			"runtime_isolation_decision_persist_failed",
			"The runtime isolation decision could not be persisted before materialization.",
			err,
		)
	}
	var stored persistence.ExecutionRuntimeIsolationDecision
	if err := r.targets.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, generation).
		Take(&stored).Error; err != nil {
		return problem.Wrap(
			500,
			"runtime_isolation_decision_load_failed",
			"The persisted runtime isolation decision could not be loaded.",
			err,
		)
	}
	if !sameRuntimeIsolationDecision(stored, row) {
		return problem.New(
			409,
			"runtime_isolation_decision_conflict",
			"The Execution Generation is already bound to a different runtime isolation decision.",
		)
	}
	return nil
}

func validateRuntimeIsolationDecisionRow(row persistence.ExecutionRuntimeIsolationDecision) error {
	if row.TenantID == uuid.Nil || row.ExecutionID == uuid.Nil || row.ExecutionTargetID == uuid.Nil || row.Generation <= 0 ||
		strings.TrimSpace(row.AllocationBackend) == "" || strings.TrimSpace(row.RequestedRuntime) == "" ||
		strings.TrimSpace(row.RequestedProfile) == "" || strings.TrimSpace(row.PolicySource) == "" {
		return problem.New(500, "runtime_isolation_decision_invalid", "The runtime isolation decision is incomplete.")
	}
	if row.Decision == "rejected" {
		if row.EffectiveRuntime != nil || row.EffectiveProfile != nil || row.DecisionReasonCode == nil ||
			row.RuntimeClassName != nil || row.AttestationDigest != nil || row.AttestedAt != nil || row.AttestationExpiresAt != nil {
			return problem.New(500, "runtime_isolation_decision_invalid", "The rejected runtime isolation decision has an invalid shape.")
		}
		return nil
	}
	if row.Decision != "selected" && row.Decision != "fallback" {
		return problem.New(500, "runtime_isolation_decision_invalid", "The runtime isolation decision has an unsupported outcome.")
	}
	if row.EffectiveRuntime == nil || row.EffectiveProfile == nil || row.DecisionReasonCode != nil {
		return problem.New(500, "runtime_isolation_decision_invalid", "The accepted runtime isolation decision has an invalid shape.")
	}
	if row.AllocationBackend == "docker-engine" {
		if (*row.EffectiveRuntime != runtimeIsolationRunc && *row.EffectiveRuntime != runtimeIsolationGVisor) ||
			*row.EffectiveProfile != string(platform.IsolationSingleTenantTrusted) || row.RuntimeClassName != nil ||
			row.AttestationDigest != nil || row.AttestedAt != nil || row.AttestationExpiresAt != nil {
			return problem.New(500, "runtime_isolation_decision_invalid", "The Docker runtime decision exceeded the trusted compatibility boundary.")
		}
		return nil
	}
	if *row.EffectiveRuntime == runtimeIsolationGVisor {
		if *row.EffectiveProfile != string(platform.IsolationGVisorSandboxed) || row.RuntimeClassName == nil ||
			row.AttestationDigest == nil || !isLowerHexSHA256(*row.AttestationDigest) ||
			row.AttestedAt == nil || row.AttestationExpiresAt == nil || !row.AttestationExpiresAt.After(*row.AttestedAt) {
			return problem.New(500, "runtime_isolation_decision_invalid", "The gVisor runtime decision is not fully attested.")
		}
	} else if row.RuntimeClassName != nil {
		return problem.New(500, "runtime_isolation_decision_invalid", "Only gVisor decisions can carry a RuntimeClass.")
	}
	if *row.EffectiveRuntime == runtimeIsolationFirecracker &&
		(row.AttestationDigest == nil || !isLowerHexSHA256(*row.AttestationDigest) ||
			row.AttestedAt == nil || row.AttestationExpiresAt == nil || !row.AttestationExpiresAt.After(*row.AttestedAt)) {
		return problem.New(500, "runtime_isolation_decision_invalid", "The Firecracker runtime decision is not fully attested.")
	}
	return nil
}

func sameRuntimeIsolationDecision(
	left persistence.ExecutionRuntimeIsolationDecision,
	right persistence.ExecutionRuntimeIsolationDecision,
) bool {
	return left.TenantID == right.TenantID && left.ExecutionID == right.ExecutionID &&
		left.Generation == right.Generation && left.ExecutionTargetID == right.ExecutionTargetID &&
		left.AllocationBackend == right.AllocationBackend && left.RequestedRuntime == right.RequestedRuntime &&
		left.RequestedProfile == right.RequestedProfile && equalOptionalString(left.EffectiveRuntime, right.EffectiveRuntime) &&
		equalOptionalString(left.EffectiveProfile, right.EffectiveProfile) && left.PolicySource == right.PolicySource &&
		left.Decision == right.Decision && equalOptionalString(left.DecisionReasonCode, right.DecisionReasonCode) &&
		equalOptionalString(left.RuntimeClassName, right.RuntimeClassName) &&
		equalOptionalString(left.AttestationDigest, right.AttestationDigest)
}

func equalOptionalString(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func runtimeIsolationStringPointer(value string) *string {
	copy := value
	return &copy
}

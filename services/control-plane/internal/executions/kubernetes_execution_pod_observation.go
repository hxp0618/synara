package executions

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var kubernetesPodFailureReasonCodePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ObserveKubernetesExecutionPod records provisioning evidence before agentd
// registration exists. Generation facts own the low-cardinality timeline;
// each failure class is retained independently so a later successful Pod does
// not erase an earlier apply, scheduling, image, eviction, or OOM failure.
func (s *Service) ObserveKubernetesExecutionPod(
	ctx context.Context,
	observation executiontargets.KubernetesExecutionPodObservation,
) error {
	normalized, err := normalizeKubernetesExecutionPodObservation(observation)
	if err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var fact persistence.ExecutionGenerationFact
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"tenant_id = ? AND execution_id = ? AND generation = ?",
				normalized.TenantID,
				normalized.ExecutionID,
				normalized.Generation,
			).
			Take(&fact).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.New(
				409,
				"kubernetes_execution_generation_fact_missing",
				"The Kubernetes Pod observation did not match an authoritative Execution generation.",
			)
		}
		if loadErr != nil {
			return problem.Wrap(
				500,
				"kubernetes_execution_generation_fact_load_failed",
				"The Kubernetes Execution generation fact could not be loaded.",
				loadErr,
			)
		}
		if fact.ExecutionTargetID != normalized.ExecutionTargetID || fact.TargetKind != "kubernetes" {
			return problem.New(
				409,
				"kubernetes_execution_pod_scope_mismatch",
				"The Kubernetes Pod observation did not match the Execution generation target.",
			)
		}

		observedAt := normalized.ObservedAt
		if fact.DispatchRequestedAt != nil && observedAt.Before(*fact.DispatchRequestedAt) {
			observedAt = *fact.DispatchRequestedAt
		}
		updates := map[string]any{
			"pod_provisioning_started_at": gorm.Expr("COALESCE(pod_provisioning_started_at, ?)", observedAt),
			"pod_last_observed_at": gorm.Expr(
				"CASE WHEN pod_last_observed_at IS NULL OR pod_last_observed_at < ? THEN ? ELSE pod_last_observed_at END",
				observedAt,
				observedAt,
			),
			"updated_at": gorm.Expr(
				"CASE WHEN updated_at < ? THEN ? ELSE updated_at END",
				observedAt,
				observedAt,
			),
		}
		pendingSince := fact.PodPendingSinceAt
		pendingAgeStartedAt := pendingSince
		if normalized.Phase == "Pending" {
			updates["pod_pending_since_at"] = gorm.Expr("COALESCE(pod_pending_since_at, ?)", observedAt)
			if pendingSince == nil {
				pendingCopy := observedAt
				pendingSince = &pendingCopy
			}
			pendingAgeStartedAt = pendingSince
			if !normalized.PodCreatedAt.IsZero() {
				createdAt := normalized.PodCreatedAt
				if fact.DispatchRequestedAt != nil && createdAt.Before(*fact.DispatchRequestedAt) {
					createdAt = *fact.DispatchRequestedAt
				}
				pendingAgeStartedAt = &createdAt
			}
		}
		if normalized.Phase == "Running" {
			updates["pod_running_at"] = gorm.Expr("COALESCE(pod_running_at, ?)", observedAt)
		}
		if err := updateGenerationFactRow(
			ctx,
			tx,
			fact.TenantID,
			fact.ExecutionID,
			fact.Generation,
			updates,
			"kubernetes_execution_pod_timeline_update_failed",
			"The Kubernetes Execution Pod provisioning timeline could not be updated.",
		); err != nil {
			return err
		}

		if normalized.FailureClass != "" {
			if err := recordKubernetesExecutionPodFailureFact(ctx, tx, fact, normalized, observedAt); err != nil {
				return err
			}
		}
		if normalized.Phase == "Pending" && pendingAgeStartedAt != nil &&
			observedAt.Sub(*pendingAgeStartedAt) >= normalized.PendingFailureThreshold {
			timeout := normalized
			timeout.FailureClass = executiontargets.KubernetesPodFailurePendingTimeout
			timeout.FailureReasonCode = "pending-threshold-exceeded"
			if err := recordKubernetesExecutionPodFailureFact(ctx, tx, fact, timeout, observedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func recordKubernetesExecutionPodFailureFact(
	ctx context.Context,
	tx *gorm.DB,
	generation persistence.ExecutionGenerationFact,
	observation executiontargets.KubernetesExecutionPodObservation,
	observedAt time.Time,
) error {
	var podUID *string
	if value := strings.TrimSpace(observation.PodUID); value != "" {
		podUID = &value
	}
	fact := persistence.ExecutionGenerationPodFailureFact{
		TenantID: generation.TenantID, ExecutionID: generation.ExecutionID, Generation: generation.Generation,
		FailureClass: observation.FailureClass, ExecutionTargetID: generation.ExecutionTargetID,
		Namespace: strings.TrimSpace(observation.Namespace), PodName: strings.TrimSpace(observation.PodName),
		PodUID: podUID, ReasonCode: observation.FailureReasonCode,
		FirstObservedAt: observedAt, LastObservedAt: observedAt, CreatedAt: observedAt, UpdatedAt: observedAt,
	}
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "tenant_id"}, {Name: "execution_id"}, {Name: "generation"}, {Name: "failure_class"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"last_observed_at": gorm.Expr(
				"CASE WHEN execution_generation_pod_failure_facts.last_observed_at < excluded.last_observed_at THEN excluded.last_observed_at ELSE execution_generation_pod_failure_facts.last_observed_at END",
			),
			"updated_at": gorm.Expr(
				"CASE WHEN execution_generation_pod_failure_facts.updated_at < excluded.updated_at THEN excluded.updated_at ELSE execution_generation_pod_failure_facts.updated_at END",
			),
		}),
	}).Create(&fact)
	if result.Error != nil {
		return problem.Wrap(
			500,
			"kubernetes_execution_pod_failure_fact_write_failed",
			"The Kubernetes Execution Pod failure fact could not be recorded.",
			result.Error,
		)
	}
	return nil
}

func normalizeKubernetesExecutionPodObservation(
	observation executiontargets.KubernetesExecutionPodObservation,
) (executiontargets.KubernetesExecutionPodObservation, error) {
	observation.Namespace = strings.TrimSpace(observation.Namespace)
	observation.PodName = strings.TrimSpace(observation.PodName)
	observation.PodUID = strings.TrimSpace(observation.PodUID)
	observation.Phase = strings.TrimSpace(observation.Phase)
	observation.FailureClass = strings.TrimSpace(observation.FailureClass)
	observation.FailureReasonCode = strings.TrimSpace(observation.FailureReasonCode)
	observation.PodCreatedAt = observation.PodCreatedAt.UTC()
	observation.ObservedAt = observation.ObservedAt.UTC()
	if observation.TenantID == uuid.Nil || observation.ExecutionTargetID == uuid.Nil || observation.ExecutionID == uuid.Nil ||
		observation.Generation <= 0 || observation.Namespace == "" || len(observation.Namespace) > 253 ||
		observation.PodName == "" || len(observation.PodName) > 253 || observation.ObservedAt.IsZero() {
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"The Kubernetes Execution Pod observation identity is incomplete.",
		)
	}
	switch observation.Phase {
	case "Applied", "ApplyFailed", "Pending", "Running", "Succeeded", "Failed", "Unknown":
	default:
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"The Kubernetes Execution Pod observation phase is unsupported.",
		)
	}
	if observation.PendingFailureThreshold <= 0 || observation.PendingFailureThreshold > 24*time.Hour {
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"The Kubernetes Execution Pod Pending failure threshold is invalid.",
		)
	}
	if len(observation.PodUID) > 160 {
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"The Kubernetes Execution Pod UID is invalid.",
		)
	}
	if !observation.PodCreatedAt.IsZero() && observation.PodCreatedAt.After(observation.ObservedAt) {
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"The Kubernetes Execution Pod creation time is after its observation.",
		)
	}
	validFailureClass := false
	for _, candidate := range []string{
		executiontargets.KubernetesPodFailureApplyFailed,
		executiontargets.KubernetesPodFailurePendingTimeout,
		executiontargets.KubernetesPodFailureUnschedulable,
		executiontargets.KubernetesPodFailureImagePull,
		executiontargets.KubernetesPodFailureContainerStart,
		executiontargets.KubernetesPodFailureEvicted,
		executiontargets.KubernetesPodFailureOOMKilled,
		executiontargets.KubernetesPodFailureGeneric,
	} {
		if observation.FailureClass == candidate {
			validFailureClass = true
			break
		}
	}
	if observation.FailureClass == "" {
		if observation.FailureReasonCode != "" || observation.Phase == "ApplyFailed" {
			return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
				400,
				"invalid_kubernetes_execution_pod_observation",
				"The Kubernetes Execution Pod failure classification is incomplete.",
			)
		}
		return observation, nil
	}
	if !validFailureClass || !kubernetesPodFailureReasonCodePattern.MatchString(observation.FailureReasonCode) ||
		len(observation.FailureReasonCode) > 160 {
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"The Kubernetes Execution Pod failure classification is invalid.",
		)
	}
	if observation.FailureClass == executiontargets.KubernetesPodFailureApplyFailed {
		if observation.Phase != "ApplyFailed" || observation.PodUID != "" {
			return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
				400,
				"invalid_kubernetes_execution_pod_observation",
				"A Kubernetes Pod apply failure must precede Pod UID assignment.",
			)
		}
	} else if observation.PodUID == "" || observation.Phase == "ApplyFailed" {
		return executiontargets.KubernetesExecutionPodObservation{}, problem.New(
			400,
			"invalid_kubernetes_execution_pod_observation",
			"A Kubernetes Pod runtime failure requires an observed Pod UID.",
		)
	}
	return observation, nil
}

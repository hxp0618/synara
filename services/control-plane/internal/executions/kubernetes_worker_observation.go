package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// ObserveKubernetesWorkerPod records an exact Pod-UID lifecycle observation
// from the target reconciler. A delete request only drains the Worker; it is
// not physical termination proof. Kubelet terminal phases and a separately
// confirmed missing observation close the immutable incarnation fact.
func (s *Service) ObserveKubernetesWorkerPod(
	ctx context.Context,
	observation executiontargets.KubernetesWorkerPodObservation,
) error {
	observedAt, terminalReason, draining, err := normalizeKubernetesWorkerPodObservation(observation)
	if err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var worker persistence.WorkerInstance
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"execution_target_id = ? AND target_kind = ? AND namespace = ? AND pod_name = ? AND instance_uid = ?",
				observation.ExecutionTargetID,
				"kubernetes",
				strings.TrimSpace(observation.Namespace),
				strings.TrimSpace(observation.PodName),
				strings.TrimSpace(observation.PodUID),
			).
			Take(&worker).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			// A Pod can terminate before agentd registers. There is no physical
			// Worker incarnation fact to close in that case.
			return nil
		}
		if loadErr != nil {
			return problem.Wrap(
				500,
				"kubernetes_worker_observation_load_failed",
				"The observed Kubernetes Worker incarnation could not be loaded.",
				loadErr,
			)
		}
		if worker.Status == "terminated" {
			return nil
		}
		if terminalReason != "" {
			return terminalizeWorkerIncarnationFromLeaseLocked(
				ctx,
				tx,
				persistence.WorkerLease{
					WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation,
					WorkerInstanceUID: worker.InstanceUID,
				},
				observedAt,
				terminalReason,
			)
		}
		if !draining {
			return nil
		}
		if worker.Status == "online" {
			result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
				Where(
					"id = ? AND incarnation = ? AND instance_uid = ? AND status = ?",
					worker.ID, worker.Incarnation, worker.InstanceUID, "online",
				).
				Updates(map[string]any{"status": "draining", "draining_at": observedAt})
			if result.Error != nil {
				return problem.Wrap(500, "kubernetes_worker_drain_failed", "The observed Kubernetes Worker could not enter Drain.", result.Error)
			}
			if result.RowsAffected != 1 {
				return problem.New(409, "kubernetes_worker_observation_conflict", "The Kubernetes Worker lifecycle changed while its Pod observation was recorded.")
			}
			worker.Status = "draining"
			worker.DrainingAt = &observedAt
		}
		return transitionWorkerIncarnationFactLocked(
			ctx, tx, worker, observedAt, workerFactStateDraining, false, "",
		)
	})
}

func normalizeKubernetesWorkerPodObservation(
	observation executiontargets.KubernetesWorkerPodObservation,
) (observedAt time.Time, terminalReason string, draining bool, err error) {
	if observation.ExecutionTargetID == uuid.Nil ||
		strings.TrimSpace(observation.Namespace) == "" ||
		strings.TrimSpace(observation.PodName) == "" {
		return time.Time{}, "", false, problem.New(400, "invalid_kubernetes_worker_observation", "The Kubernetes Worker Pod observation identity is incomplete.")
	}
	podUID := strings.TrimSpace(observation.PodUID)
	parsedUID, parseErr := uuid.Parse(podUID)
	if parseErr != nil || parsedUID.String() != podUID {
		return time.Time{}, "", false, problem.New(400, "invalid_kubernetes_worker_observation", "The Kubernetes Worker Pod observation UID is invalid.")
	}
	observedAt = observation.ObservedAt.UTC()
	if observedAt.IsZero() {
		return time.Time{}, "", false, problem.New(400, "invalid_kubernetes_worker_observation", "The Kubernetes Worker Pod observation time is required.")
	}
	phase := strings.TrimSpace(observation.Phase)
	reason := strings.TrimSpace(observation.Reason)
	switch {
	case phase == "Succeeded":
		return observedAt, "kubernetes-pod-succeeded", false, nil
	case phase == "Failed":
		return observedAt, "kubernetes-pod-failed", false, nil
	case strings.HasPrefix(reason, "confirmed-missing:"):
		return observedAt, "kubernetes-pod-confirmed-missing", false, nil
	case strings.HasPrefix(reason, "delete-requested:"):
		return observedAt, "", true, nil
	default:
		return time.Time{}, "", false, problem.New(400, "invalid_kubernetes_worker_observation", "The Kubernetes Worker Pod lifecycle observation is unsupported.")
	}
}

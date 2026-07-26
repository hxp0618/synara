package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateKubernetesPodDeletionFenceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_kubernetes_pod_deletion_fences_shape_insert`,
		`CREATE TRIGGER trg_kubernetes_pod_deletion_fences_shape_insert
		 BEFORE INSERT ON kubernetes_pod_deletion_fences
		 WHEN length(trim(ifnull(NEW.namespace, ''))) NOT BETWEEN 1 AND 253
		   OR length(trim(ifnull(NEW.pod_name, ''))) NOT BETWEEN 1 AND 253
		   OR length(ifnull(NEW.pod_uid, '')) <> 36
		   OR NEW.pod_uid <> trim(NEW.pod_uid)
		   OR NEW.pod_uid <> lower(NEW.pod_uid)
		   OR substr(NEW.pod_uid, 9, 1) <> '-'
		   OR substr(NEW.pod_uid, 14, 1) <> '-'
		   OR substr(NEW.pod_uid, 19, 1) <> '-'
		   OR substr(NEW.pod_uid, 24, 1) <> '-'
		   OR length(replace(NEW.pod_uid, '-', '')) <> 32
		   OR replace(NEW.pod_uid, '-', '') GLOB '*[^0-9a-f]*'
		   OR NEW.pod_uid = '00000000-0000-0000-0000-000000000000'
		   OR length(trim(ifnull(NEW.reason, ''))) NOT BETWEEN 1 AND 2000
		   OR NEW.requested_at IS NULL
		   OR NOT EXISTS (
		     SELECT 1
		     FROM execution_targets AS target
		     WHERE target.id = NEW.execution_target_id
		       AND target.kind = 'kubernetes'
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Kubernetes Pod deletion fence');
		 END`,
		`DROP TRIGGER IF EXISTS trg_kubernetes_pod_deletion_fences_immutable_update`,
		`CREATE TRIGGER trg_kubernetes_pod_deletion_fences_immutable_update
		 BEFORE UPDATE ON kubernetes_pod_deletion_fences
		 BEGIN
		   SELECT RAISE(ABORT, 'Kubernetes Pod deletion fence is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_kubernetes_pod_deletion_fences_immutable_delete`,
		`CREATE TRIGGER trg_kubernetes_pod_deletion_fences_immutable_delete
		 BEFORE DELETE ON kubernetes_pod_deletion_fences
		 WHEN EXISTS (
		   SELECT 1 FROM execution_targets AS target
		   WHERE target.id = OLD.execution_target_id
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Kubernetes Pod deletion fence is immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_kubernetes_pod_deletion_fences_cascade`,
		`CREATE TRIGGER trg_execution_targets_kubernetes_pod_deletion_fences_cascade
		 AFTER DELETE ON execution_targets
		 BEGIN
		   DELETE FROM kubernetes_pod_deletion_fences
		   WHERE execution_target_id = OLD.id;
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_pod_deletion_fence_insert`,
		`CREATE TRIGGER trg_worker_instances_pod_deletion_fence_insert
		 BEFORE INSERT ON worker_instances
		 WHEN NEW.target_kind = 'kubernetes'
		   AND NEW.registration_trust_mode = 'kubernetes-pod-bound-v1'
		   AND (
		     NEW.cluster_id <> 'kubernetes'
		     OR EXISTS (
		       SELECT 1
		       FROM kubernetes_pod_deletion_fences AS fence
		       WHERE fence.execution_target_id = NEW.execution_target_id
		         AND fence.namespace = NEW.namespace
		         AND fence.pod_name = NEW.pod_name
		         AND fence.pod_uid = NEW.instance_uid
		     )
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Kubernetes Pod UID is deletion fenced');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_pod_deletion_fence_update`,
		`CREATE TRIGGER trg_worker_instances_pod_deletion_fence_update
		 BEFORE UPDATE OF execution_target_id, target_kind, cluster_id, namespace, pod_name, instance_uid, registration_trust_mode
		 ON worker_instances
		 WHEN NEW.target_kind = 'kubernetes'
		   AND NEW.registration_trust_mode = 'kubernetes-pod-bound-v1'
		   AND (
		     NEW.cluster_id <> 'kubernetes'
		     OR EXISTS (
		       SELECT 1
		       FROM kubernetes_pod_deletion_fences AS fence
		       WHERE fence.execution_target_id = NEW.execution_target_id
		         AND fence.namespace = NEW.namespace
		         AND fence.pod_name = NEW.pod_name
		         AND fence.pod_uid = NEW.instance_uid
		     )
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Kubernetes Pod UID is deletion fenced');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_instances_fenced_reactivation`,
		`CREATE TRIGGER trg_worker_instances_fenced_reactivation
		 BEFORE UPDATE OF status, administrative_status ON worker_instances
		 WHEN NEW.target_kind = 'kubernetes'
		   AND NEW.registration_trust_mode = 'kubernetes-pod-bound-v1'
		   AND (
		     (NEW.status = 'online' AND NEW.status IS NOT OLD.status)
		     OR (
		       NEW.administrative_status = 'active'
		       AND NEW.administrative_status IS NOT OLD.administrative_status
		     )
		   )
		   AND EXISTS (
		     SELECT 1
		     FROM kubernetes_pod_deletion_fences AS fence
		     WHERE fence.execution_target_id = NEW.execution_target_id
		       AND fence.namespace = NEW.namespace
		       AND fence.pod_name = NEW.pod_name
		       AND fence.pod_uid = NEW.instance_uid
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'Kubernetes Pod UID is deletion fenced');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Kubernetes Pod deletion fence migration: %w", err)
		}
	}
	return nil
}

package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteKubernetesPodDeletionFenceBlocksDirectWorkerWritesAndAllowsTargetCascade(t *testing.T) {
	ctx := context.Background()
	store, domain := openSQLiteWorkerRevocationStore(t, ctx, true)
	now := time.Now().UTC().Truncate(time.Second)
	targetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "sqlite-pod-fence", Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	invalidClusterWorker := sqliteRevocationWorker(targetID, "invalid-cluster-pod", now)
	invalidClusterWorker.TargetKind = "kubernetes"
	invalidClusterWorker.ClusterID = "cluster-a"
	invalidClusterWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	if err := store.DB().Create(&invalidClusterWorker).Error; err == nil {
		t.Fatal("SQLite allowed a direct pod-bound Kubernetes Worker INSERT with a noncanonical cluster identity")
	}
	podName := "fenced-pod"
	fencedUID := uuid.NewString()
	fence := persistence.KubernetesPodDeletionFence{
		ExecutionTargetID: targetID,
		Namespace:         "default",
		PodName:           podName,
		PodUID:            fencedUID,
		RequestedAt:       now,
		Reason:            "sqlite-direct-write-test",
	}
	if err := store.DB().Create(&fence).Error; err != nil {
		t.Fatal(err)
	}

	fencedWorker := sqliteRevocationWorker(targetID, podName, now)
	fencedWorker.TargetKind = "kubernetes"
	fencedWorker.ClusterID = "kubernetes"
	fencedWorker.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	fencedWorker.InstanceUID = fencedUID
	if err := store.DB().Create(&fencedWorker).Error; err == nil {
		t.Fatal("SQLite allowed a direct Worker INSERT for a deletion-fenced Pod UID")
	}

	replacement := sqliteRevocationWorker(targetID, podName, now.Add(time.Second))
	replacement.TargetKind = "kubernetes"
	replacement.ClusterID = "kubernetes"
	replacement.RegistrationTrustMode = "kubernetes-pod-bound-v1"
	if err := store.DB().Create(&replacement).Error; err != nil {
		t.Fatalf("SQLite rejected a replacement Pod UID: %v", err)
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", replacement.ID).
		Update("cluster_id", "cluster-a").Error; err == nil {
		t.Fatal("SQLite allowed a direct pod-bound Kubernetes Worker cluster identity UPDATE")
	}
	if err := store.DB().Model(&persistence.WorkerInstance{}).
		Where("id = ?", replacement.ID).
		Update("instance_uid", fencedUID).Error; err == nil {
		t.Fatal("SQLite allowed a direct Worker instance_uid UPDATE to a deletion-fenced Pod UID")
	}
	if err := store.DB().Model(&persistence.KubernetesPodDeletionFence{}).
		Where(
			"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
			targetID, fence.Namespace, fence.PodName, fence.PodUID,
		).
		Update("reason", "mutated").Error; err == nil {
		t.Fatal("SQLite allowed a Kubernetes Pod deletion fence to mutate")
	}
	if err := store.DB().Delete(&persistence.KubernetesPodDeletionFence{},
		"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
		targetID, fence.Namespace, fence.PodName, fence.PodUID,
	).Error; err == nil {
		t.Fatal("SQLite allowed a Kubernetes Pod deletion fence to be deleted directly")
	}

	if err := store.DB().Delete(&persistence.WorkerInstance{}, "id = ?", replacement.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Delete(&persistence.ExecutionTarget{}, "id = ?", targetID).Error; err != nil {
		t.Fatalf("delete fenced execution target: %v", err)
	}
	var fenceCount int64
	if err := store.DB().Model(&persistence.KubernetesPodDeletionFence{}).
		Where("execution_target_id = ?", targetID).
		Count(&fenceCount).Error; err != nil {
		t.Fatal(err)
	}
	if fenceCount != 0 {
		t.Fatalf("target cascade retained %d Kubernetes Pod deletion fences", fenceCount)
	}
}

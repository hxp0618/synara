package podlifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const KubernetesClusterID = "kubernetes"

var ErrLogicalIdentityLockUnavailable = errors.New("Kubernetes Pod logical identity lock is unavailable")

const logicalIdentityLockUnavailablePostgresMessage = "Worker logical identity lock is unavailable"

type ExactPodIdentity struct {
	ExecutionTargetID uuid.UUID
	Namespace         string
	PodName           string
	PodUID            string
}

func NewExactPodIdentity(
	executionTargetID uuid.UUID,
	namespace string,
	podName string,
	podUID string,
) (ExactPodIdentity, error) {
	namespace = strings.TrimSpace(namespace)
	podName = strings.TrimSpace(podName)
	podUID = strings.TrimSpace(podUID)
	parsedPodUID, err := uuid.Parse(podUID)
	if executionTargetID == uuid.Nil ||
		namespace == "" || len(namespace) > 253 || strings.ContainsAny(namespace, "\r\n\t") ||
		podName == "" || len(podName) > 253 || strings.ContainsAny(podName, "\r\n\t") ||
		err != nil || parsedPodUID == uuid.Nil || parsedPodUID.String() != podUID {
		return ExactPodIdentity{}, errors.New("invalid exact Kubernetes Pod identity")
	}
	return ExactPodIdentity{
		ExecutionTargetID: executionTargetID,
		Namespace:         namespace,
		PodName:           podName,
		PodUID:            podUID,
	}, nil
}

func TryTransactionLogicalIdentityLock(
	ctx context.Context,
	tx *gorm.DB,
	identity ExactPodIdentity,
) (bool, error) {
	if tx.Dialector.Name() != "postgres" {
		// Personal SQLite is deliberately single-connection. Its transaction is
		// the serialization boundary shared by registration and reconciliation.
		return true, nil
	}
	var acquired bool
	if err := tx.WithContext(ctx).
		Raw(
			"SELECT try_lock_worker_logical_identity(?, ?, ?, ?) AS acquired",
			identity.ExecutionTargetID,
			KubernetesClusterID,
			identity.Namespace,
			identity.PodName,
		).
		Scan(&acquired).Error; err != nil {
		return false, fmt.Errorf("acquire Kubernetes Pod logical identity transaction lock: %w", err)
	}
	return acquired, nil
}

func IsLogicalIdentityLockUnavailable(err error) bool {
	if errors.Is(err, ErrLogicalIdentityLockUnavailable) {
		return true
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) &&
		postgresError.Code == "40001" &&
		postgresError.Message == logicalIdentityLockUnavailablePostgresMessage
}

func IsDeletionFenced(ctx context.Context, tx *gorm.DB, identity ExactPodIdentity) (bool, error) {
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.KubernetesPodDeletionFence{}).
		Where(
			"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
			identity.ExecutionTargetID,
			identity.Namespace,
			identity.PodName,
			identity.PodUID,
		).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("inspect Kubernetes Pod deletion fence: %w", err)
	}
	return count > 0, nil
}

func EnsureDeletionFence(
	ctx context.Context,
	tx *gorm.DB,
	identity ExactPodIdentity,
	requestedAt time.Time,
	reason string,
) error {
	reason = strings.TrimSpace(reason)
	if requestedAt.IsZero() || reason == "" || len(reason) > 2000 {
		return errors.New("invalid Kubernetes Pod deletion fence")
	}
	fence := persistence.KubernetesPodDeletionFence{
		ExecutionTargetID: identity.ExecutionTargetID,
		Namespace:         identity.Namespace,
		PodName:           identity.PodName,
		PodUID:            identity.PodUID,
		RequestedAt:       requestedAt.UTC(),
		Reason:            reason,
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "execution_target_id"},
			{Name: "namespace"},
			{Name: "pod_name"},
			{Name: "pod_uid"},
		},
		DoNothing: true,
	}).Create(&fence).Error; err != nil {
		return fmt.Errorf("persist Kubernetes Pod deletion fence: %w", err)
	}
	return nil
}

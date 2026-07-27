package executiontargets

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/warmcapacity"
)

const managedKubernetesWarmCapacityPublisherPrefix = "managed-kubernetes-warm-capacity-publisher:"

type ManagedKubernetesWarmCapacityObservation struct {
	TenantID                uuid.UUID
	ExecutionTargetID       uuid.UUID
	WorkerPoolID            uuid.UUID
	WorkerPoolVersion       int64
	WarmSupported           bool
	WorkerReleaseRevisionID *uuid.UUID
	WorkerReleaseChannel    *string
	MinIdleUnits            int
	DesiredTotalUnits       int
	ClaimedUnits            int
	ReadyIdleUnits          int
	Reason                  *string
	ObservedAt              time.Time
}

type ManagedKubernetesWarmCapacityObserver func(context.Context, ManagedKubernetesWarmCapacityObservation) error

type ManagedKubernetesWarmCapacityPublisherConfig struct {
	PublisherIdentity string
	ObservationTTL    time.Duration
}

type ManagedKubernetesWarmCapacityPublisher struct {
	warmCapacity      *warmcapacity.Service
	publisherIdentity string
	observationTTL    time.Duration
	now               func() time.Time
}

func NewManagedKubernetesWarmCapacityPublisher(
	targets *Service,
	config ManagedKubernetesWarmCapacityPublisherConfig,
) *ManagedKubernetesWarmCapacityPublisher {
	ttl := config.ObservationTTL
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	return &ManagedKubernetesWarmCapacityPublisher{
		warmCapacity:      warmcapacity.NewService(targets.db),
		publisherIdentity: normalizeManagedKubernetesWarmCapacityPublisherIdentity(config.PublisherIdentity),
		observationTTL:    ttl,
		now:               func() time.Time { return time.Now().UTC() },
	}
}

func (p *ManagedKubernetesWarmCapacityPublisher) PublishReconcile(
	ctx context.Context,
	observation ManagedKubernetesWarmCapacityObservation,
) error {
	observedAt := observation.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = p.now()
	}
	_, err := p.warmCapacity.Observe(ctx, warmcapacity.Observation{
		TenantID:                observation.TenantID,
		ExecutionTargetID:       observation.ExecutionTargetID,
		WorkerPoolID:            observation.WorkerPoolID,
		WorkerPoolVersion:       observation.WorkerPoolVersion,
		WarmSupported:           observation.WarmSupported,
		WorkerReleaseRevisionID: observation.WorkerReleaseRevisionID,
		WorkerReleaseChannel:    observation.WorkerReleaseChannel,
		MinIdleUnits:            observation.MinIdleUnits,
		DesiredTotalUnits:       observation.DesiredTotalUnits,
		ClaimedUnits:            observation.ClaimedUnits,
		ReadyIdleUnits:          observation.ReadyIdleUnits,
		Source:                  p.publisherIdentity,
		Reason:                  observation.Reason,
		ObservedAt:              observedAt,
		TTL:                     p.observationTTL,
	})
	return err
}

func normalizeManagedKubernetesWarmCapacityPublisherIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		identity = managedKubernetesWarmCapacityPublisherPrefix + "default"
	}
	if len(identity) > 160 {
		identity = identity[:160]
	}
	return identity
}

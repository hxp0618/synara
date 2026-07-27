package executiontargets

import (
	"context"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/targetcapacity"
)

type ManagedKubernetesTargetCapacityPublisherConfig struct {
	PublisherIdentity string
	ObservationTTL    time.Duration
}

type ManagedKubernetesTargetCapacityPublisher struct {
	capacity          *targetcapacity.Service
	publisherIdentity string
	observationTTL    time.Duration
	now               func() time.Time
}

func NewManagedKubernetesTargetCapacityPublisher(
	targets *Service,
	config ManagedKubernetesTargetCapacityPublisherConfig,
) *ManagedKubernetesTargetCapacityPublisher {
	ttl := config.ObservationTTL
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	return &ManagedKubernetesTargetCapacityPublisher{
		capacity:          targetcapacity.NewService(targets.db),
		publisherIdentity: normalizeManagedKubernetesTargetCapacityPublisherIdentity(config.PublisherIdentity),
		observationTTL:    ttl,
		now:               func() time.Time { return time.Now().UTC() },
	}
}

func (p *ManagedKubernetesTargetCapacityPublisher) PublishReconcile(
	ctx context.Context,
	observation ManagedKubernetesTargetCapacityObservation,
) error {
	observedAt := observation.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = p.now()
	}
	_, err := p.capacity.Observe(ctx, targetcapacity.Observation{
		TenantID: observation.TenantID, ExecutionTargetID: observation.ExecutionTargetID,
		TargetKind: "kubernetes", Source: p.publisherIdentity,
		TotalPods: observation.TotalPods, AllocatedPods: observation.AllocatedPods,
		AvailablePods: observation.AvailablePods, SchedulableUnits: observation.SchedulableUnits,
		CPU: observation.CPU, Memory: observation.Memory,
		EphemeralStorage: observation.EphemeralStorage,
		GPUResourceName:  observation.GPUResourceName, GPU: observation.GPU,
		ObservedAt: observedAt, TTL: p.observationTTL,
	})
	return err
}

func normalizeManagedKubernetesTargetCapacityPublisherIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		identity = managedKubernetesTargetCapacityPublisherPrefix + "default"
	}
	if len(identity) > 160 {
		identity = identity[:160]
	}
	return identity
}

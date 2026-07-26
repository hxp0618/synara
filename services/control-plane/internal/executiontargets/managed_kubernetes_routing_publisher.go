package executiontargets

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

const managedKubernetesRoutingPublisherPrefix = "managed-kubernetes-routing-publisher:"

type ManagedKubernetesRoutingHealthObservation struct {
	ExecutionTargetID uuid.UUID
	TenantOwned       bool
	Status            string
	CapacityStatus    string
	// AvailableCapacityUnits is the total target Pod-slot ceiling.
	AvailableCapacityUnits *int
	AllocatedCapacityUnits int
	ReservationAuthority   *routing.ReservationAuthorityObservation
	Reason                 *string
	ObservedAt             time.Time
}

type ManagedKubernetesRoutingHealthObserver func(context.Context, ManagedKubernetesRoutingHealthObservation) error

type ManagedKubernetesRoutingPublisherConfig struct {
	PublisherIdentity string
	ObservationTTL    time.Duration
}

type ManagedKubernetesRoutingPublisher struct {
	targets           *Service
	routing           *routing.Service
	publisherIdentity string
	observationTTL    time.Duration
	now               func() time.Time
}

func NewManagedKubernetesRoutingPublisher(
	targets *Service,
	config ManagedKubernetesRoutingPublisherConfig,
) *ManagedKubernetesRoutingPublisher {
	ttl := config.ObservationTTL
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	return &ManagedKubernetesRoutingPublisher{
		targets:           targets,
		routing:           routing.NewService(targets.db),
		publisherIdentity: normalizeManagedKubernetesRoutingPublisherIdentity(config.PublisherIdentity),
		observationTTL:    ttl,
		now:               func() time.Time { return time.Now().UTC() },
	}
}

func (p *ManagedKubernetesRoutingPublisher) PublishReconcile(
	ctx context.Context,
	observation ManagedKubernetesRoutingHealthObservation,
) error {
	if !observation.TenantOwned {
		return invalidManagedKubernetesRoutingTarget()
	}
	var eligibleTargetCount int64
	if err := p.targets.db.WithContext(ctx).
		Model(&persistence.ExecutionTarget{}).
		Where("id = ? AND kind = ? AND tenant_id IS NOT NULL", observation.ExecutionTargetID, "kubernetes").
		Count(&eligibleTargetCount).Error; err != nil {
		return problem.Wrap(
			500,
			"managed_kubernetes_routing_target_load_failed",
			"Managed Kubernetes routing target authority could not be verified.",
			err,
		)
	}
	if eligibleTargetCount != 1 {
		return invalidManagedKubernetesRoutingTarget()
	}
	observedAt := observation.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = p.now()
	}
	_, err := p.routing.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID:      observation.ExecutionTargetID,
		Status:                 observation.Status,
		CapacityStatus:         observation.CapacityStatus,
		AvailableCapacityUnits: observation.AvailableCapacityUnits,
		AllocatedCapacityUnits: observation.AllocatedCapacityUnits,
		ReservationAuthority:   observation.ReservationAuthority,
		Source:                 p.publisherIdentity,
		Reason:                 observation.Reason,
		ObservedAt:             observedAt,
		TTL:                    p.observationTTL,
	})
	return err
}

func invalidManagedKubernetesRoutingTarget() error {
	return problem.New(
		409,
		"managed_kubernetes_routing_target_invalid",
		"Managed Kubernetes routing health observations require a tenant-owned Kubernetes Execution Target.",
	)
}

func normalizeManagedKubernetesRoutingPublisherIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		identity = managedKubernetesRoutingPublisherPrefix + "default"
	}
	if len(identity) > 160 {
		identity = identity[:160]
	}
	return identity
}

func managedKubernetesRoutingReasonPointer(reason string) *string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	return &reason
}

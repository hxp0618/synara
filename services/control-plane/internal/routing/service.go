package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

const (
	StrategyPriority = "priority"
	StrategyBalanced = "balanced"
	StrategyLatency  = "latency"

	GroupStatusActive   = "active"
	GroupStatusDraining = "draining"
	GroupStatusDisabled = "disabled"

	MemberStatusActive   = "active"
	MemberStatusDraining = "draining"
	MemberStatusDisabled = "disabled"

	HealthHealthy     = "healthy"
	HealthDegraded    = "degraded"
	HealthUnreachable = "unreachable"
	HealthUnknown     = "unknown"

	CapacityAvailable = "available"
	CapacitySaturated = "saturated"
	CapacityUnknown   = "unknown"

	LocationStatusDraining    = "draining"
	LocationStatusUnreachable = "unreachable"
)

type CreateGroupInput struct {
	ID                        uuid.UUID
	TenantID                  uuid.UUID
	OrganizationID            *uuid.UUID
	Name                      string
	Strategy                  string
	PreferredRegions          []string
	AllowCrossRegion          bool
	MaxFailoverAttempts       *int
	HealthMaxStalenessSeconds int
}

type AddMemberInput struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	TargetGroupID     uuid.UUID
	ExecutionTargetID uuid.UUID
	Region            string
	ClusterID         string
	Priority          int
	Weight            int
}

type HealthObservation struct {
	ExecutionTargetID uuid.UUID
	Status            string
	CapacityStatus    string
	// AvailableCapacityUnits is the legacy API name for the total schedulable
	// capacity ceiling, not the remaining free units. Free units are this value
	// minus AllocatedCapacityUnits.
	AvailableCapacityUnits *int
	AllocatedCapacityUnits int
	Source                 string
	Reason                 *string
	ObservedAt             time.Time
	TTL                    time.Duration
}

type DRReadinessObservation struct {
	ExecutionTargetID   uuid.UUID
	SourceDRDomain      string
	DRDomain            string
	ReplicatedThroughAt time.Time
	ArtifactsReady      bool
	CheckpointsReady    bool
	MemoryReady         bool
	PublisherIdentity   string
	Reason              *string
	ObservedAt          time.Time
	TTL                 time.Duration
}

type LocationOutageObservation struct {
	TenantID          uuid.UUID
	Region            string
	ClusterID         string
	Status            string
	PublisherIdentity string
	Reason            *string
	ObservedAt        time.Time
	TTL               time.Duration
}

type DRStoreRequirements struct {
	Artifacts   bool
	Checkpoints bool
	Memory      bool
}

func (requirements DRStoreRequirements) Any() bool {
	return requirements.Artifacts || requirements.Checkpoints || requirements.Memory
}

func (requirements DRStoreRequirements) Names() []string {
	names := make([]string, 0, 3)
	if requirements.Artifacts {
		names = append(names, "artifacts")
	}
	if requirements.Checkpoints {
		names = append(names, "checkpoints")
	}
	if requirements.Memory {
		names = append(names, "memory")
	}
	return names
}

type SelectRequest struct {
	TenantID            uuid.UUID
	OrganizationID      uuid.UUID
	TargetGroupID       uuid.UUID
	Provider            string
	PreferredTargetID   *uuid.UUID
	PreferredRegions    []string
	RequiredTargetKind  string
	ExcludedTargetIDs   []uuid.UUID
	SourceRegion        string
	SourceClusterID     string
	SourceDRDomain      string
	ReplicatedThroughAt time.Time
	RequiredDRStores    DRStoreRequirements
	DisasterRecovery    bool
}

type Selection struct {
	Target                   persistence.ExecutionTarget
	Group                    persistence.ExecutionTargetGroup
	Member                   persistence.ExecutionTargetGroupMember
	Health                   persistence.ExecutionTargetHealth
	DRReadiness              *persistence.ExecutionTargetDRReadiness
	SchedulingPolicySnapshot schedulingpolicy.Snapshot
	QueuePressure            QueuePressureSnapshot
	RoutingReason            string
}

// QueuePressureSnapshot freezes the durable, not-yet-serviced Execution
// pressure used by queue-pressure-v1. The health authority remains the source
// of truth for hard capacity admission: a queued Execution may already have a
// Pod counted by AllocatedCapacityUnits, so adding these values is deliberately
// only a conservative routing rank.
type QueuePressureSnapshot struct {
	QueuedExecutionUnits int64
	EffectiveLoadRank    int64
}

type MemberState struct {
	Member                 ExecutionTargetGroupMemberView `json:"member"`
	Target                 ExecutionTargetView            `json:"target"`
	Health                 *TargetHealthView              `json:"health,omitempty"`
	DRReadinessAuthorities []TargetDRReadinessView        `json:"drReadinessAuthorities,omitempty"`
}

type GroupView struct {
	ID                        uuid.UUID  `json:"id"`
	TenantID                  uuid.UUID  `json:"tenantId"`
	OrganizationID            *uuid.UUID `json:"organizationId,omitempty"`
	Name                      string     `json:"name"`
	Strategy                  string     `json:"strategy"`
	PreferredRegions          []string   `json:"preferredRegions"`
	AllowCrossRegion          bool       `json:"allowCrossRegion"`
	MaxFailoverAttempts       int        `json:"maxFailoverAttempts"`
	HealthMaxStalenessSeconds int        `json:"healthMaxStalenessSeconds"`
	Status                    string     `json:"status"`
	Version                   int64      `json:"version"`
	CreatedAt                 time.Time  `json:"createdAt"`
	UpdatedAt                 time.Time  `json:"updatedAt"`
}

type ExecutionTargetView struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       *uuid.UUID `json:"tenantId,omitempty"`
	OrganizationID *uuid.UUID `json:"organizationId,omitempty"`
	Kind           string     `json:"kind"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
}

type TargetHealthView struct {
	Status         string `json:"status"`
	CapacityStatus string `json:"capacityStatus"`
	// AvailableCapacityUnits is the total schedulable capacity ceiling.
	AvailableCapacityUnits *int      `json:"availableCapacityUnits,omitempty"`
	AllocatedCapacityUnits int       `json:"allocatedCapacityUnits"`
	Source                 string    `json:"source"`
	Reason                 *string   `json:"reason,omitempty"`
	ObservedAt             time.Time `json:"observedAt"`
	ExpiresAt              time.Time `json:"expiresAt"`
	Version                int64     `json:"version"`
}

type TargetDRReadinessView struct {
	SourceDRDomain      string    `json:"sourceDrDomain"`
	DRDomain            string    `json:"drDomain"`
	ReplicatedThroughAt time.Time `json:"replicatedThroughAt"`
	ArtifactsReady      bool      `json:"artifactsReady"`
	CheckpointsReady    bool      `json:"checkpointsReady"`
	MemoryReady         bool      `json:"memoryReady"`
	PublisherIdentity   string    `json:"publisherIdentity"`
	Reason              *string   `json:"reason,omitempty"`
	ObservedAt          time.Time `json:"observedAt"`
	ExpiresAt           time.Time `json:"expiresAt"`
	Version             int64     `json:"version"`
}

type LocationOutageView struct {
	Region            string    `json:"region"`
	ClusterID         *string   `json:"clusterId,omitempty"`
	Status            string    `json:"status"`
	PublisherIdentity string    `json:"publisherIdentity"`
	Reason            *string   `json:"reason,omitempty"`
	ObservedAt        time.Time `json:"observedAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	Version           int64     `json:"version"`
}

type ExecutionTargetGroupMemberView struct {
	ID                uuid.UUID `json:"id"`
	ExecutionTargetID uuid.UUID `json:"executionTargetId"`
	Region            string    `json:"region"`
	ClusterID         string    `json:"clusterId"`
	Priority          int       `json:"priority"`
	Weight            int       `json:"weight"`
	Status            string    `json:"status"`
	Version           int64     `json:"version"`
}

type GroupState struct {
	Group   GroupView     `json:"group"`
	Members []MemberState `json:"members"`
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]GroupState, error) {
	if tenantID == uuid.Nil {
		return nil, problem.New(400, "target_group_tenant_required", "tenantId is required.")
	}
	var groups []persistence.ExecutionTargetGroup
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("lower(name), id").Find(&groups).Error; err != nil {
		return nil, problem.Wrap(500, "target_groups_load_failed", "Execution Target Groups could not be loaded.", err)
	}
	states := make([]GroupState, 0, len(groups))
	for _, group := range groups {
		var members []persistence.ExecutionTargetGroupMember
		if err := s.db.WithContext(ctx).Where("tenant_id = ? AND target_group_id = ?", tenantID, group.ID).
			Order("priority, execution_target_id").Find(&members).Error; err != nil {
			return nil, problem.Wrap(500, "target_group_members_load_failed", "Execution Target Group members could not be loaded.", err)
		}
		memberStates := make([]MemberState, 0, len(members))
		for _, member := range members {
			var target persistence.ExecutionTarget
			if err := s.db.WithContext(ctx).Where("id = ?", member.ExecutionTargetID).Take(&target).Error; err != nil {
				return nil, problem.Wrap(500, "target_group_member_target_load_failed", "Execution Target Group member target could not be loaded.", err)
			}
			var health persistence.ExecutionTargetHealth
			var healthPointer *TargetHealthView
			err := s.db.WithContext(ctx).Where("execution_target_id = ?", member.ExecutionTargetID).Take(&health).Error
			if err == nil {
				view := toTargetHealthView(health)
				healthPointer = &view
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, problem.Wrap(500, "target_group_member_health_load_failed", "Execution Target health could not be loaded.", err)
			}
			var readinessRows []persistence.ExecutionTargetDRReadiness
			err = s.db.WithContext(ctx).
				Where("execution_target_id = ?", member.ExecutionTargetID).
				Order("source_dr_domain, observed_at DESC").
				Find(&readinessRows).Error
			if err != nil {
				return nil, problem.Wrap(500, "target_group_member_dr_readiness_load_failed", "Execution Target DR readiness could not be loaded.", err)
			}
			readinessViews := make([]TargetDRReadinessView, 0, len(readinessRows))
			for _, readiness := range readinessRows {
				readinessViews = append(readinessViews, toTargetDRReadinessView(readiness))
			}
			memberStates = append(memberStates, MemberState{
				Member: ExecutionTargetGroupMemberView{
					ID: member.ID, ExecutionTargetID: member.ExecutionTargetID,
					Region: member.Region, ClusterID: member.ClusterID, Priority: member.Priority,
					Weight: member.Weight, Status: member.Status, Version: member.Version,
				},
				Target: ExecutionTargetView{
					ID: target.ID, TenantID: target.TenantID, OrganizationID: target.OrganizationID,
					Kind: target.Kind, Name: target.Name, Status: target.Status,
				},
				Health:                 healthPointer,
				DRReadinessAuthorities: readinessViews,
			})
		}
		states = append(states, GroupState{Group: toGroupView(group), Members: memberStates})
	}
	return states, nil
}

func toGroupView(group persistence.ExecutionTargetGroup) GroupView {
	return GroupView{
		ID: group.ID, TenantID: group.TenantID, OrganizationID: group.OrganizationID,
		Name: group.Name, Strategy: group.Strategy,
		PreferredRegions: append([]string(nil), group.PreferredRegions...),
		AllowCrossRegion: group.AllowCrossRegion, MaxFailoverAttempts: group.MaxFailoverAttempts,
		HealthMaxStalenessSeconds: group.HealthMaxStalenessSeconds, Status: group.Status,
		Version: group.Version, CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt,
	}
}

func GroupViewOf(group persistence.ExecutionTargetGroup) GroupView { return toGroupView(group) }

func MemberViewOf(member persistence.ExecutionTargetGroupMember) ExecutionTargetGroupMemberView {
	return ExecutionTargetGroupMemberView{
		ID: member.ID, ExecutionTargetID: member.ExecutionTargetID,
		Region: member.Region, ClusterID: member.ClusterID, Priority: member.Priority,
		Weight: member.Weight, Status: member.Status, Version: member.Version,
	}
}

func HealthViewOf(health persistence.ExecutionTargetHealth) TargetHealthView {
	return toTargetHealthView(health)
}

func DRReadinessViewOf(readiness persistence.ExecutionTargetDRReadiness) TargetDRReadinessView {
	return toTargetDRReadinessView(readiness)
}

func LocationOutageViewOf(outage persistence.ExecutionLocationOutage) LocationOutageView {
	return toLocationOutageView(outage)
}

func (s *Service) ListLocationOutages(ctx context.Context, tenantID uuid.UUID) ([]LocationOutageView, error) {
	if tenantID == uuid.Nil {
		return nil, problem.New(400, "location_outage_tenant_required", "tenantId is required.")
	}
	var outages []persistence.ExecutionLocationOutage
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("lower(region), lower(cluster_id), observed_at DESC").
		Find(&outages).Error; err != nil {
		return nil, problem.Wrap(500, "location_outages_load_failed", "Location outages could not be loaded.", err)
	}
	views := make([]LocationOutageView, 0, len(outages))
	for _, outage := range outages {
		views = append(views, toLocationOutageView(outage))
	}
	return views, nil
}

func (s *Service) CreateGroup(ctx context.Context, input CreateGroupInput) (persistence.ExecutionTargetGroup, error) {
	if input.TenantID == uuid.Nil {
		return persistence.ExecutionTargetGroup{}, problem.New(400, "target_group_tenant_required", "tenantId is required.")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return persistence.ExecutionTargetGroup{}, problem.New(400, "invalid_target_group_name", "Target Group name must be between 1 and 160 characters.")
	}
	strategy, err := normalizeStrategy(input.Strategy)
	if err != nil {
		return persistence.ExecutionTargetGroup{}, err
	}
	regions, err := normalizeRegions(input.PreferredRegions)
	if err != nil {
		return persistence.ExecutionTargetGroup{}, err
	}
	maxFailovers := 2
	if input.MaxFailoverAttempts != nil {
		maxFailovers = *input.MaxFailoverAttempts
	}
	if maxFailovers < 0 || maxFailovers > 20 {
		return persistence.ExecutionTargetGroup{}, problem.New(400, "invalid_target_group_failover_limit", "maxFailoverAttempts must be between 0 and 20.")
	}
	staleness := input.HealthMaxStalenessSeconds
	if staleness == 0 {
		staleness = 90
	}
	if staleness < 10 || staleness > 3600 {
		return persistence.ExecutionTargetGroup{}, problem.New(400, "invalid_target_group_health_staleness", "healthMaxStalenessSeconds must be between 10 and 3600.")
	}
	now := s.now()
	group := persistence.ExecutionTargetGroup{
		ID: input.ID, TenantID: input.TenantID, OrganizationID: input.OrganizationID,
		Name: name, Strategy: strategy, PreferredRegions: regions,
		AllowCrossRegion: input.AllowCrossRegion, MaxFailoverAttempts: maxFailovers,
		HealthMaxStalenessSeconds: staleness, Status: GroupStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if group.ID == uuid.Nil {
		group.ID = uuid.New()
	}
	if err := s.db.WithContext(ctx).Create(&group).Error; err != nil {
		return persistence.ExecutionTargetGroup{}, problem.Wrap(409, "target_group_create_rejected", "Execution Target Group could not be created.", err)
	}
	return group, nil
}

func (s *Service) AddMember(ctx context.Context, input AddMemberInput) (persistence.ExecutionTargetGroupMember, error) {
	if input.TenantID == uuid.Nil || input.TargetGroupID == uuid.Nil || input.ExecutionTargetID == uuid.Nil {
		return persistence.ExecutionTargetGroupMember{}, problem.New(400, "target_group_member_scope_required", "Tenant, Target Group, and Execution Target are required.")
	}
	region, err := normalizeLocation(input.Region, 120, "invalid_target_group_member_region", "region")
	if err != nil {
		return persistence.ExecutionTargetGroupMember{}, err
	}
	if strings.Contains(region, "/") {
		return persistence.ExecutionTargetGroupMember{}, problem.New(400, "invalid_target_group_member_region", "region must not contain '/'.")
	}
	clusterID, err := normalizeLocation(input.ClusterID, 200, "invalid_target_group_member_cluster", "clusterId")
	if err != nil {
		return persistence.ExecutionTargetGroupMember{}, err
	}
	if strings.Contains(clusterID, "/") {
		return persistence.ExecutionTargetGroupMember{}, problem.New(400, "invalid_target_group_member_cluster", "clusterId must not contain '/'.")
	}
	priority := input.Priority
	if priority < 0 || priority > 1_000_000 {
		return persistence.ExecutionTargetGroupMember{}, problem.New(400, "invalid_target_group_member_priority", "priority must be between 0 and 1000000.")
	}
	weight := input.Weight
	if weight == 0 {
		weight = 100
	}
	if weight < 1 || weight > 10_000 {
		return persistence.ExecutionTargetGroupMember{}, problem.New(400, "invalid_target_group_member_weight", "weight must be between 1 and 10000.")
	}
	var member persistence.ExecutionTargetGroupMember
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var group persistence.ExecutionTargetGroup
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ? AND status <> ?", input.TenantID, input.TargetGroupID, GroupStatusDisabled).
			Take(&group).Error; err != nil {
			return problem.Wrap(404, "target_group_not_found", "Execution Target Group not found.", err)
		}
		var target persistence.ExecutionTarget
		if err := tx.WithContext(ctx).
			Where("id = ? AND status <> ? AND (tenant_id IS NULL OR tenant_id = ?)", input.ExecutionTargetID, "disabled", input.TenantID).
			Take(&target).Error; err != nil {
			return problem.Wrap(404, "target_group_member_target_not_found", "Execution Target is not available to this tenant.", err)
		}
		if group.OrganizationID != nil && target.OrganizationID != nil && *group.OrganizationID != *target.OrganizationID {
			return problem.New(409, "target_group_member_organization_mismatch", "Execution Target belongs to another organization.")
		}
		now := s.now()
		member = persistence.ExecutionTargetGroupMember{
			ID: input.ID, TenantID: input.TenantID, TargetGroupID: input.TargetGroupID,
			ExecutionTargetID: input.ExecutionTargetID, Region: region, ClusterID: clusterID,
			Priority: priority, Weight: weight, Status: MemberStatusActive, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if member.ID == uuid.Nil {
			member.ID = uuid.New()
		}
		if err := tx.WithContext(ctx).Create(&member).Error; err != nil {
			return problem.Wrap(409, "target_group_member_create_rejected", "Execution Target Group member could not be created.", err)
		}
		return nil
	})
	return member, err
}

func (s *Service) ObserveHealth(ctx context.Context, input HealthObservation) (persistence.ExecutionTargetHealth, error) {
	status := strings.ToLower(strings.TrimSpace(input.Status))
	if !slices.Contains([]string{HealthHealthy, HealthDegraded, HealthUnreachable, HealthUnknown}, status) {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_health_status", "Target health status is invalid.")
	}
	capacity := strings.ToLower(strings.TrimSpace(input.CapacityStatus))
	if capacity == "" {
		capacity = CapacityUnknown
	}
	if !slices.Contains([]string{CapacityAvailable, CapacitySaturated, CapacityUnknown}, capacity) {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_capacity_status", "Target capacity status is invalid.")
	}
	if input.ExecutionTargetID == uuid.Nil || input.AllocatedCapacityUnits < 0 ||
		(input.AvailableCapacityUnits != nil && *input.AvailableCapacityUnits < 0) {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_health_capacity", "Target health capacity values are invalid.")
	}
	source := strings.TrimSpace(input.Source)
	if source == "" || len(source) > 160 {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_health_source", "Target health source must be between 1 and 160 characters.")
	}
	observedAt := input.ObservedAt.UTC()
	now := s.now()
	if observedAt.IsZero() {
		observedAt = now
	}
	if observedAt.After(now) {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_health_observed_at", "observedAt must not be later than server time.")
	}
	if input.TTL < 10*time.Second || input.TTL > time.Hour {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_health_ttl", "Target health TTL must be between 10 seconds and 1 hour.")
	}
	if input.Reason != nil && len(*input.Reason) > 2000 {
		return persistence.ExecutionTargetHealth{}, problem.New(400, "invalid_target_health_reason", "Target health reason is too long.")
	}
	var result persistence.ExecutionTargetHealth
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var target persistence.ExecutionTarget
		if err := tx.WithContext(ctx).Select("id").Where("id = ?", input.ExecutionTargetID).Take(&target).Error; err != nil {
			return problem.Wrap(404, "execution_target_not_found", "Execution Target not found.", err)
		}
		var current persistence.ExecutionTargetHealth
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_target_id = ?", input.ExecutionTargetID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = persistence.ExecutionTargetHealth{
				ExecutionTargetID: input.ExecutionTargetID, Status: status, CapacityStatus: capacity,
				AvailableCapacityUnits: input.AvailableCapacityUnits, AllocatedCapacityUnits: input.AllocatedCapacityUnits,
				Source: source, Reason: input.Reason, ObservedAt: observedAt, ExpiresAt: observedAt.Add(input.TTL),
				Version: 1, UpdatedAt: now,
			}
			if err := tx.WithContext(ctx).Create(&result).Error; err != nil {
				return problem.Wrap(409, "target_health_create_rejected", "Target health observation could not be created.", err)
			}
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "target_health_load_failed", "Target health observation could not be loaded.", err)
		}
		if !observedAt.After(current.ObservedAt) {
			return problem.New(409, "target_health_observation_stale", "Target health observation is not newer than the current authority.")
		}
		result = current
		result.Status = status
		result.CapacityStatus = capacity
		result.AvailableCapacityUnits = input.AvailableCapacityUnits
		result.AllocatedCapacityUnits = input.AllocatedCapacityUnits
		result.Source = source
		result.Reason = input.Reason
		result.ObservedAt = observedAt
		result.ExpiresAt = observedAt.Add(input.TTL)
		result.Version++
		result.UpdatedAt = now
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionTargetHealth{}).
			Where("execution_target_id = ? AND version = ?", current.ExecutionTargetID, current.Version).
			Select("status", "capacity_status", "available_capacity_units", "allocated_capacity_units", "source", "reason", "observed_at", "expires_at", "version", "updated_at").
			Updates(&result).Error; err != nil {
			return problem.Wrap(409, "target_health_update_rejected", "Target health observation could not be advanced.", err)
		}
		return nil
	})
	return result, err
}

func (s *Service) ObserveDRReadiness(ctx context.Context, input DRReadinessObservation) (persistence.ExecutionTargetDRReadiness, error) {
	if input.ExecutionTargetID == uuid.Nil {
		return persistence.ExecutionTargetDRReadiness{}, problem.New(400, "invalid_target_dr_readiness_scope", "Execution Target is required.")
	}
	sourceDRDomain, err := normalizeLocation(
		input.SourceDRDomain, 200, "invalid_target_dr_source_domain", "sourceDrDomain",
	)
	if err != nil {
		return persistence.ExecutionTargetDRReadiness{}, err
	}
	drDomain, err := normalizeLocation(input.DRDomain, 200, "invalid_target_dr_domain", "drDomain")
	if err != nil {
		return persistence.ExecutionTargetDRReadiness{}, err
	}
	publisherIdentity, err := normalizeLocation(
		input.PublisherIdentity, 200, "invalid_target_dr_publisher_identity", "publisherIdentity",
	)
	if err != nil {
		return persistence.ExecutionTargetDRReadiness{}, err
	}
	replicatedThroughAt := input.ReplicatedThroughAt.UTC()
	if replicatedThroughAt.IsZero() {
		return persistence.ExecutionTargetDRReadiness{}, problem.New(400, "invalid_target_dr_replicated_through_at", "replicatedThroughAt is required.")
	}
	if input.Reason != nil && len(*input.Reason) > 2000 {
		return persistence.ExecutionTargetDRReadiness{}, problem.New(400, "invalid_target_dr_readiness_reason", "Target DR readiness reason is too long.")
	}
	observedAt := input.ObservedAt.UTC()
	now := s.now()
	if observedAt.IsZero() {
		observedAt = now
	}
	if observedAt.After(now) {
		return persistence.ExecutionTargetDRReadiness{}, problem.New(400, "invalid_target_dr_readiness_observed_at", "observedAt must not be later than server time.")
	}
	if replicatedThroughAt.After(observedAt) {
		return persistence.ExecutionTargetDRReadiness{}, problem.New(400, "invalid_target_dr_replicated_through_at", "replicatedThroughAt must not be later than observedAt.")
	}
	if input.TTL < 10*time.Second || input.TTL > time.Hour {
		return persistence.ExecutionTargetDRReadiness{}, problem.New(400, "invalid_target_dr_readiness_ttl", "Target DR readiness TTL must be between 10 seconds and 1 hour.")
	}
	var result persistence.ExecutionTargetDRReadiness
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var target persistence.ExecutionTarget
		if err := tx.WithContext(ctx).Select("id").Where("id = ?", input.ExecutionTargetID).Take(&target).Error; err != nil {
			return problem.Wrap(404, "execution_target_not_found", "Execution Target not found.", err)
		}
		var current persistence.ExecutionTargetDRReadiness
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_target_id = ? AND source_dr_domain = ?", input.ExecutionTargetID, sourceDRDomain).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = persistence.ExecutionTargetDRReadiness{
				ExecutionTargetID:   input.ExecutionTargetID,
				SourceDRDomain:      sourceDRDomain,
				DRDomain:            drDomain,
				ReplicatedThroughAt: replicatedThroughAt,
				ArtifactsReady:      input.ArtifactsReady,
				CheckpointsReady:    input.CheckpointsReady,
				MemoryReady:         input.MemoryReady,
				PublisherIdentity:   publisherIdentity,
				Reason:              input.Reason,
				ObservedAt:          observedAt,
				ExpiresAt:           observedAt.Add(input.TTL),
				Version:             1,
				UpdatedAt:           now,
			}
			if err := tx.WithContext(ctx).Create(&result).Error; err != nil {
				return problem.Wrap(409, "target_dr_readiness_create_rejected", "Target DR readiness observation could not be created.", err)
			}
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "target_dr_readiness_load_failed", "Target DR readiness observation could not be loaded.", err)
		}
		if !observedAt.After(current.ObservedAt) {
			return problem.New(409, "target_dr_readiness_observation_stale", "Target DR readiness observation is not newer than the current authority.")
		}
		result = current
		result.DRDomain = drDomain
		result.ReplicatedThroughAt = replicatedThroughAt
		result.ArtifactsReady = input.ArtifactsReady
		result.CheckpointsReady = input.CheckpointsReady
		result.MemoryReady = input.MemoryReady
		result.PublisherIdentity = publisherIdentity
		result.Reason = input.Reason
		result.ObservedAt = observedAt
		result.ExpiresAt = observedAt.Add(input.TTL)
		result.Version++
		result.UpdatedAt = now
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionTargetDRReadiness{}).
			Where(
				"execution_target_id = ? AND source_dr_domain = ? AND version = ?",
				current.ExecutionTargetID, current.SourceDRDomain, current.Version,
			).
			Select(
				"dr_domain", "replicated_through_at", "artifacts_ready", "checkpoints_ready",
				"memory_ready", "publisher_identity", "reason", "observed_at", "expires_at", "version", "updated_at",
			).
			Updates(&result).Error; err != nil {
			return problem.Wrap(409, "target_dr_readiness_update_rejected", "Target DR readiness observation could not be advanced.", err)
		}
		return nil
	})
	return result, err
}

func (s *Service) ObserveLocationOutage(
	ctx context.Context,
	input LocationOutageObservation,
) (persistence.ExecutionLocationOutage, error) {
	if input.TenantID == uuid.Nil {
		return persistence.ExecutionLocationOutage{}, problem.New(400, "invalid_location_outage_scope", "Tenant and region are required.")
	}
	region, err := normalizeLocation(input.Region, 120, "invalid_location_outage_region", "region")
	if err != nil {
		return persistence.ExecutionLocationOutage{}, err
	}
	clusterID, err := normalizeOptionalLocation(input.ClusterID, 200, "invalid_location_outage_cluster", "clusterId")
	if err != nil {
		return persistence.ExecutionLocationOutage{}, err
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	if !slices.Contains([]string{LocationStatusDraining, LocationStatusUnreachable}, status) {
		return persistence.ExecutionLocationOutage{}, problem.New(400, "invalid_location_outage_status", "Location outage status is invalid.")
	}
	publisherIdentity, err := normalizeLocation(
		input.PublisherIdentity, 200, "invalid_location_outage_publisher_identity", "publisherIdentity",
	)
	if err != nil {
		return persistence.ExecutionLocationOutage{}, err
	}
	if input.Reason != nil && len(*input.Reason) > 2000 {
		return persistence.ExecutionLocationOutage{}, problem.New(400, "invalid_location_outage_reason", "Location outage reason is too long.")
	}
	serverNow := s.now()
	observedAt := input.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = serverNow
	} else if observedAt.After(serverNow) {
		return persistence.ExecutionLocationOutage{}, problem.New(400, "invalid_location_outage_observed_at", "Location outage observedAt must not be later than server time.")
	}
	if input.TTL < 10*time.Second || input.TTL > time.Hour {
		return persistence.ExecutionLocationOutage{}, problem.New(400, "invalid_location_outage_ttl", "Location outage TTL must be between 10 seconds and 1 hour.")
	}

	var result persistence.ExecutionLocationOutage
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		// The Tenant row is the range-lock authority for location outages. An
		// exact outage row may not exist yet, so row locking alone cannot prevent
		// an insert from crossing selection-to-commit validation.
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id").Where("id = ? AND deleted_at IS NULL", input.TenantID).Take(&tenant).Error; err != nil {
			return problem.Wrap(500, "location_outage_tenant_lock_failed", "Location outage authority could not lock its Tenant scope.", err)
		}
		var current persistence.ExecutionLocationOutage
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND region = ? AND cluster_id = ?", input.TenantID, region, clusterID).
			Take(&current).Error
		now := s.now()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = persistence.ExecutionLocationOutage{
				TenantID: input.TenantID, Region: region, ClusterID: clusterID, Status: status,
				PublisherIdentity: publisherIdentity, Reason: input.Reason,
				ObservedAt: observedAt, ExpiresAt: observedAt.Add(input.TTL),
				Version: 1, UpdatedAt: now,
			}
			if err := tx.WithContext(ctx).Create(&result).Error; err != nil {
				return problem.Wrap(409, "location_outage_create_rejected", "Location outage observation could not be created.", err)
			}
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "location_outage_load_failed", "Location outage observation could not be loaded.", err)
		}
		if !observedAt.After(current.ObservedAt) {
			return problem.New(409, "location_outage_observation_stale", "Location outage observation is not newer than the current authority.")
		}
		result = current
		result.Status = status
		result.PublisherIdentity = publisherIdentity
		result.Reason = input.Reason
		result.ObservedAt = observedAt
		result.ExpiresAt = observedAt.Add(input.TTL)
		result.Version++
		result.UpdatedAt = now
		if err := tx.WithContext(ctx).Model(&persistence.ExecutionLocationOutage{}).
			Where(
				"tenant_id = ? AND region = ? AND cluster_id = ? AND version = ?",
				current.TenantID, current.Region, current.ClusterID, current.Version,
			).
			Select("status", "publisher_identity", "reason", "observed_at", "expires_at", "version", "updated_at").
			Updates(&result).Error; err != nil {
			return problem.Wrap(409, "location_outage_update_rejected", "Location outage observation could not be advanced.", err)
		}
		return nil
	})
	return result, err
}

func (s *Service) Select(ctx context.Context, tx *gorm.DB, request SelectRequest) (Selection, error) {
	if request.TenantID == uuid.Nil || request.OrganizationID == uuid.Nil || request.TargetGroupID == uuid.Nil {
		return Selection{}, problem.New(400, "target_routing_scope_required", "Tenant, organization, and Target Group are required for global routing.")
	}
	db := tx
	if db == nil {
		db = s.db
	}
	organizationID := request.OrganizationID
	policySnapshot, err := schedulingpolicy.NewService(db).Resolve(
		ctx,
		db,
		request.TenantID,
		&organizationID,
	)
	if err != nil {
		return Selection{}, problem.Wrap(
			500,
			"execution_scheduling_policy_load_failed",
			"The effective Execution Scheduling Policy could not be loaded.",
			err,
		)
	}
	var group persistence.ExecutionTargetGroup
	err = db.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND status = ?", request.TenantID, request.TargetGroupID, GroupStatusActive).
		Where("organization_id IS NULL OR organization_id = ?", request.OrganizationID).
		Take(&group).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Selection{}, problem.New(409, "target_group_unavailable", "Execution Target Group is not active or is outside the organization scope.")
	}
	if err != nil {
		return Selection{}, problem.Wrap(500, "target_group_load_failed", "Execution Target Group could not be loaded.", err)
	}
	var members []persistence.ExecutionTargetGroupMember
	if err := db.WithContext(ctx).
		Where("tenant_id = ? AND target_group_id = ? AND status = ?", request.TenantID, group.ID, MemberStatusActive).
		Find(&members).Error; err != nil {
		return Selection{}, problem.Wrap(500, "target_group_members_load_failed", "Execution Target Group members could not be loaded.", err)
	}
	if len(members) == 0 {
		return Selection{}, problem.New(409, "target_group_has_no_members", "Execution Target Group has no active members.")
	}
	targetIDs := make([]uuid.UUID, 0, len(members))
	for _, member := range members {
		targetIDs = append(targetIDs, member.ExecutionTargetID)
	}
	var targets []persistence.ExecutionTarget
	if err := db.WithContext(ctx).
		Where("id IN ? AND status = ?", targetIDs, "active").
		Where("tenant_id IS NULL OR tenant_id = ?", request.TenantID).
		Find(&targets).Error; err != nil {
		return Selection{}, problem.Wrap(500, "target_group_targets_load_failed", "Execution Targets could not be loaded.", err)
	}
	var healthRows []persistence.ExecutionTargetHealth
	if err := db.WithContext(ctx).Where("execution_target_id IN ?", targetIDs).Find(&healthRows).Error; err != nil {
		return Selection{}, problem.Wrap(500, "target_group_health_load_failed", "Execution Target health could not be loaded.", err)
	}
	queuedExecutionUnitsByTarget, err := loadQueuedExecutionUnits(ctx, db, targetIDs)
	if err != nil {
		return Selection{}, problem.Wrap(
			500,
			"target_group_queue_pressure_load_failed",
			"Execution Target queue pressure could not be loaded.",
			err,
		)
	}
	now := s.now()
	var locationOutageRows []persistence.ExecutionLocationOutage
	if err := db.WithContext(ctx).
		Where("tenant_id = ? AND observed_at <= ? AND expires_at > ?", request.TenantID, now, now).
		Find(&locationOutageRows).Error; err != nil {
		return Selection{}, problem.Wrap(500, "location_outages_load_failed", "Location outages could not be loaded.", err)
	}
	var readinessRows []persistence.ExecutionTargetDRReadiness
	if err := db.WithContext(ctx).Where("execution_target_id IN ?", targetIDs).Find(&readinessRows).Error; err != nil {
		return Selection{}, problem.Wrap(500, "target_group_dr_readiness_load_failed", "Execution Target DR readiness could not be loaded.", err)
	}
	targetByID := make(map[uuid.UUID]persistence.ExecutionTarget, len(targets))
	for _, target := range targets {
		targetByID[target.ID] = target
	}
	healthByID := make(map[uuid.UUID]persistence.ExecutionTargetHealth, len(healthRows))
	for _, health := range healthRows {
		healthByID[health.ExecutionTargetID] = health
	}
	readinessByTargetAndSource := make(map[uuid.UUID]map[string]persistence.ExecutionTargetDRReadiness, len(readinessRows))
	for _, readiness := range readinessRows {
		bySource, ok := readinessByTargetAndSource[readiness.ExecutionTargetID]
		if !ok {
			bySource = make(map[string]persistence.ExecutionTargetDRReadiness)
			readinessByTargetAndSource[readiness.ExecutionTargetID] = bySource
		}
		bySource[readiness.SourceDRDomain] = readiness
	}
	excluded := make(map[uuid.UUID]struct{}, len(request.ExcludedTargetIDs))
	for _, targetID := range request.ExcludedTargetIDs {
		excluded[targetID] = struct{}{}
	}
	preferredRegions := request.PreferredRegions
	if len(preferredRegions) == 0 {
		preferredRegions = group.PreferredRegions
	}
	preferredRegions, err = normalizeRegions(preferredRegions)
	if err != nil {
		return Selection{}, err
	}
	locationOutages := buildLocationOutageIndex(locationOutageRows)
	candidates := make([]routeCandidate, 0, len(members))
	blockedLocationCandidates := make([]blockedLocationCandidate, 0, len(members))
	blockedCandidates := make([]blockedDRCandidate, 0, len(members))
	policyCandidateCount := 0
	policyBlockedCandidateCount := 0
	for _, member := range members {
		if _, skip := excluded[member.ExecutionTargetID]; skip {
			continue
		}
		target, ok := targetByID[member.ExecutionTargetID]
		if !ok || (target.OrganizationID != nil && *target.OrganizationID != request.OrganizationID) {
			continue
		}
		if request.RequiredTargetKind != "" && target.Kind != request.RequiredTargetKind {
			continue
		}
		policyCandidateCount++
		if !schedulingpolicy.AllowsTarget(policySnapshot.Effective, schedulingpolicy.Target{
			ID: target.ID, Region: member.Region, Cluster: member.ClusterID, Provider: request.Provider,
		}) {
			policyBlockedCandidateCount++
			continue
		}
		health, ok := healthByID[member.ExecutionTargetID]
		if !ok || !healthEligible(health, group, now) {
			continue
		}
		regionRank := indexOrMax(preferredRegions, member.Region)
		if !group.AllowCrossRegion {
			requiredRegion := strings.TrimSpace(request.SourceRegion)
			if requiredRegion != "" && member.Region != requiredRegion {
				continue
			}
			if requiredRegion == "" && len(preferredRegions) > 0 && regionRank == math.MaxInt {
				continue
			}
		}
		queuePressure := queuePressureSnapshot(
			health,
			queuedExecutionUnitsByTarget[member.ExecutionTargetID],
			member.Weight,
		)
		candidate := routeCandidate{
			member: member, target: target, health: health,
			preferred:  request.PreferredTargetID != nil && member.ExecutionTargetID == *request.PreferredTargetID,
			regionRank: regionRank, healthRank: healthStatusRank(health.Status), queuePressure: queuePressure,
		}
		affinityRank, err := providerAffinityRank(target.Capabilities, request.Provider)
		if err != nil {
			return Selection{}, err
		}
		candidate.providerAffinityRank = affinityRank
		if outage, ok := locationOutages.match(member.Region, member.ClusterID); ok {
			blockedLocationCandidates = append(blockedLocationCandidates, blockedLocationCandidate{
				candidate: candidate,
				outage:    outage,
			})
			continue
		}
		requirement := drReadinessRequirement(request, member)
		if requirement.required {
			if requirement.sourceDRDomain == "" || requirement.replicatedThroughAt.IsZero() {
				blockedCandidates = append(blockedCandidates, blockedDRCandidate{
					candidate:     candidate,
					requirement:   requirement,
					blockedReason: "dr-readiness-context-missing",
				})
				continue
			}
			readinessBySource := readinessByTargetAndSource[member.ExecutionTargetID]
			readiness, ok := readinessBySource[requirement.sourceDRDomain]
			if !ok {
				blockedCandidates = append(blockedCandidates, blockedDRCandidate{
					candidate:     candidate,
					requirement:   requirement,
					blockedReason: "dr-readiness-missing",
				})
				continue
			}
			if blockedReason, unreadyStores := drReadinessBlockedReason(readiness, requirement, now); blockedReason != "" {
				blockedCandidates = append(blockedCandidates, blockedDRCandidate{
					candidate:     candidate,
					requirement:   requirement,
					blockedReason: blockedReason,
					readiness:     &readiness,
					unreadyStores: unreadyStores,
				})
				continue
			}
			candidate.drReadiness = &readiness
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		if policyCandidateCount != 0 && policyBlockedCandidateCount == policyCandidateCount {
			return Selection{}, noPolicyEligibleDestination(policySnapshot)
		}
		if len(blockedCandidates) != 0 {
			return Selection{}, blockedCandidates[bestBlockedCandidateIndex(blockedCandidates, group.Strategy)].problem()
		}
		if len(blockedLocationCandidates) != 0 {
			return Selection{}, blockedLocationCandidates[bestBlockedLocationCandidateIndex(blockedLocationCandidates, group.Strategy)].problem()
		}
		return Selection{}, problem.New(409, "target_group_no_eligible_destination", "No active Target Group member has fresh health, capacity, scope, and region eligibility.")
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidateLess(candidates[left], candidates[right], group.Strategy)
	})
	selected := candidates[0]
	reason := group.Strategy
	if selected.preferred {
		reason = "preferred-target"
	}
	if request.DisasterRecovery {
		reason = "disaster-recovery"
	}
	return Selection{
		Target:                   selected.target,
		Group:                    group,
		Member:                   selected.member,
		Health:                   selected.health,
		DRReadiness:              selected.drReadiness,
		SchedulingPolicySnapshot: policySnapshot,
		QueuePressure:            selected.queuePressure,
		RoutingReason:            reason,
	}, nil
}

// LockSelectionForCommit linearizes a previously computed routing decision
// against its mutable authorities. It never selects a replacement candidate:
// any changed identity or version fails closed so the caller can retry from a
// new decision. The lock order is tenant, organization, Tenant/Organization
// scheduling-policy heads and revisions, target, group, member, location
// outage rows, health, readiness, then a read of durable queued/recovering
// Executions while the Target serialization lock is retained.
// Health and readiness publishers only lock their own authority row, so they
// cannot form a reverse dependency on the preceding routing locks.
func (s *Service) LockSelectionForCommit(
	ctx context.Context,
	tx *gorm.DB,
	request SelectRequest,
	selection Selection,
) (Selection, error) {
	if tx == nil {
		return Selection{}, problem.New(500, "target_routing_commit_transaction_required", "Routing commit validation requires an active transaction.")
	}
	if request.TenantID == uuid.Nil || request.OrganizationID == uuid.Nil || request.TargetGroupID == uuid.Nil ||
		selection.Target.ID == uuid.Nil || selection.Group.ID == uuid.Nil || selection.Member.ID == uuid.Nil {
		return Selection{}, staleSelectionProblem("selection-scope-invalid", selection)
	}
	var tenant persistence.Tenant
	if err := lockRoutingAuthority(tx, ctx).
		Select("id").Where("id = ? AND deleted_at IS NULL", request.TenantID).
		Take(&tenant).Error; err != nil {
		return Selection{}, routingCommitLoadError(err, "tenant", selection)
	}
	organizationID := request.OrganizationID
	if err := schedulingpolicy.NewService(s.db).LockEffectiveForCommit(
		ctx,
		tx,
		request.TenantID,
		&organizationID,
		selection.SchedulingPolicySnapshot,
	); err != nil {
		if errors.Is(err, schedulingpolicy.ErrPolicyStale) {
			return Selection{}, staleSelectionProblem("scheduling-policy-changed", selection)
		}
		return Selection{}, problem.Wrap(
			500,
			"target_routing_commit_authority_load_failed",
			"The effective Execution Scheduling Policy could not be locked for routing commit.",
			err,
		)
	}

	var target persistence.ExecutionTarget
	if err := lockRoutingAuthority(tx, ctx).
		Where("id = ?", selection.Target.ID).
		Take(&target).Error; err != nil {
		return Selection{}, routingCommitLoadError(err, "target", selection)
	}
	if target.Status != "active" || !sameExecutionTargetAuthority(target, selection.Target) ||
		(target.TenantID != nil && *target.TenantID != request.TenantID) ||
		(target.OrganizationID != nil && *target.OrganizationID != request.OrganizationID) {
		return Selection{}, staleSelectionProblem("target-changed", selection)
	}

	var group persistence.ExecutionTargetGroup
	if err := lockRoutingAuthority(tx, ctx).
		Where("tenant_id = ? AND id = ?", request.TenantID, selection.Group.ID).
		Take(&group).Error; err != nil {
		return Selection{}, routingCommitLoadError(err, "group", selection)
	}
	if group.ID != request.TargetGroupID || group.Status != GroupStatusActive ||
		!sameTargetGroupAuthority(group, selection.Group) ||
		(group.OrganizationID != nil && *group.OrganizationID != request.OrganizationID) {
		return Selection{}, staleSelectionProblem("group-changed", selection)
	}

	var member persistence.ExecutionTargetGroupMember
	if err := lockRoutingAuthority(tx, ctx).
		Where("tenant_id = ? AND id = ?", request.TenantID, selection.Member.ID).
		Take(&member).Error; err != nil {
		return Selection{}, routingCommitLoadError(err, "member", selection)
	}
	if member.Status != MemberStatusActive || member.TargetGroupID != group.ID || member.ExecutionTargetID != target.ID ||
		!sameTargetGroupMemberAuthority(member, selection.Member) {
		return Selection{}, staleSelectionProblem("member-changed", selection)
	}

	var locationOutages []persistence.ExecutionLocationOutage
	if err := lockRoutingAuthority(tx, ctx).
		Where(
			"tenant_id = ? AND region = ? AND cluster_id IN ?",
			request.TenantID,
			member.Region,
			[]string{"", member.ClusterID},
		).
		Order("cluster_id").Find(&locationOutages).Error; err != nil {
		return Selection{}, problem.Wrap(500, "target_routing_commit_authority_load_failed", "Destination location outage authority could not be locked for commit.", err)
	}

	var health persistence.ExecutionTargetHealth
	if err := lockRoutingAuthority(tx, ctx).
		Where("execution_target_id = ?", target.ID).
		Take(&health).Error; err != nil {
		return Selection{}, routingCommitLoadError(err, "health", selection)
	}
	now := s.now()
	for _, outage := range locationOutages {
		if outage.ObservedAt.After(now) ||
			(outage.ExpiresAt.After(now) && slices.Contains(
				[]string{LocationStatusDraining, LocationStatusUnreachable}, outage.Status,
			)) {
			return Selection{}, staleSelectionProblem("destination-location-outage-changed", selection)
		}
	}
	if !sameTargetHealthAuthority(health, selection.Health) || !healthEligible(health, group, now) {
		return Selection{}, staleSelectionProblem("health-changed-or-ineligible", selection)
	}

	requirement := drReadinessRequirement(request, member)
	var readiness *persistence.ExecutionTargetDRReadiness
	if requirement.required {
		if selection.DRReadiness == nil || requirement.sourceDRDomain == "" || requirement.replicatedThroughAt.IsZero() {
			return Selection{}, staleSelectionProblem("dr-readiness-selection-missing", selection)
		}
		var current persistence.ExecutionTargetDRReadiness
		if err := lockRoutingAuthority(tx, ctx).
			Where("execution_target_id = ? AND source_dr_domain = ?", target.ID, requirement.sourceDRDomain).
			Take(&current).Error; err != nil {
			return Selection{}, routingCommitLoadError(err, "dr-readiness", selection)
		}
		blockedReason, _ := drReadinessBlockedReason(current, requirement, now)
		if !sameTargetDRReadinessAuthority(current, *selection.DRReadiness) || blockedReason != "" {
			return Selection{}, staleSelectionProblem("dr-readiness-changed-or-ineligible", selection)
		}
		readiness = &current
	} else if selection.DRReadiness != nil {
		return Selection{}, staleSelectionProblem("dr-readiness-selection-unexpected", selection)
	}
	queuedExecutionUnitsByTarget, err := loadQueuedExecutionUnits(ctx, tx, []uuid.UUID{target.ID})
	if err != nil {
		return Selection{}, problem.Wrap(
			500,
			"target_routing_commit_queue_pressure_load_failed",
			"Destination queue pressure could not be revalidated for routing commit.",
			err,
		)
	}
	actualQueuedExecutionUnits := queuedExecutionUnitsByTarget[target.ID]
	if actualQueuedExecutionUnits != selection.QueuePressure.QueuedExecutionUnits {
		return Selection{}, staleQueuePressureProblem(
			selection,
			selection.QueuePressure.QueuedExecutionUnits,
			actualQueuedExecutionUnits,
		)
	}

	locked := selection
	locked.Target = target
	locked.Group = group
	locked.Member = member
	locked.Health = health
	locked.DRReadiness = readiness
	locked.QueuePressure = queuePressureSnapshot(health, actualQueuedExecutionUnits, member.Weight)
	return locked, nil
}

func lockRoutingAuthority(tx *gorm.DB, ctx context.Context) *gorm.DB {
	return persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "")
}

func routingCommitLoadError(err error, authority string, selection Selection) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return staleSelectionProblem(authority+"-missing", selection)
	}
	return problem.Wrap(500, "target_routing_commit_authority_load_failed", "Destination routing authority could not be locked for commit.", err)
}

func staleSelectionProblem(reason string, selection Selection) *problem.Error {
	apiError := problem.New(409, "target_routing_selection_stale", "Destination routing authority changed after selection; retry from a new routing decision.")
	apiError.Details = map[string]any{
		"reason":            reason,
		"targetGroupId":     selection.Group.ID,
		"memberId":          selection.Member.ID,
		"executionTargetId": selection.Target.ID,
	}
	return apiError
}

func staleQueuePressureProblem(
	selection Selection,
	expectedQueuedExecutionUnits int64,
	actualQueuedExecutionUnits int64,
) *problem.Error {
	apiError := staleSelectionProblem("queue-pressure-changed", selection)
	apiError.Details["expectedQueuedExecutionUnits"] = expectedQueuedExecutionUnits
	apiError.Details["actualQueuedExecutionUnits"] = actualQueuedExecutionUnits
	return apiError
}

func noPolicyEligibleDestination(snapshot schedulingpolicy.Snapshot) error {
	apiError := problem.New(
		409,
		"target_group_no_policy_eligible_destination",
		"The effective Execution Scheduling Policy does not allow a Target Group destination for this launch.",
	)
	details := map[string]any{
		"tenantPolicyVersion": snapshot.Tenant.Version,
		"tenantPolicyDigest":  snapshot.Tenant.Digest,
	}
	if snapshot.Organization != nil {
		details["organizationPolicyVersion"] = snapshot.Organization.Version
		details["organizationPolicyDigest"] = snapshot.Organization.Digest
	}
	apiError.Details = details
	return apiError
}

func sameExecutionTargetAuthority(current, selected persistence.ExecutionTarget) bool {
	return current.ID == selected.ID && sameOptionalUUID(current.TenantID, selected.TenantID) &&
		sameOptionalUUID(current.OrganizationID, selected.OrganizationID) && current.Kind == selected.Kind &&
		current.Status == selected.Status && current.UpdatedAt.Equal(selected.UpdatedAt) &&
		reflect.DeepEqual(current.ConfigurationEncrypted, selected.ConfigurationEncrypted) &&
		reflect.DeepEqual(current.Capabilities, selected.Capabilities)
}

func sameTargetGroupAuthority(current, selected persistence.ExecutionTargetGroup) bool {
	return current.ID == selected.ID && current.TenantID == selected.TenantID &&
		sameOptionalUUID(current.OrganizationID, selected.OrganizationID) && current.Version == selected.Version &&
		current.Strategy == selected.Strategy && slices.Equal(current.PreferredRegions, selected.PreferredRegions) &&
		current.AllowCrossRegion == selected.AllowCrossRegion && current.MaxFailoverAttempts == selected.MaxFailoverAttempts &&
		current.HealthMaxStalenessSeconds == selected.HealthMaxStalenessSeconds && current.Status == selected.Status
}

func sameTargetGroupMemberAuthority(current, selected persistence.ExecutionTargetGroupMember) bool {
	return current.ID == selected.ID && current.TenantID == selected.TenantID && current.TargetGroupID == selected.TargetGroupID &&
		current.ExecutionTargetID == selected.ExecutionTargetID && current.Region == selected.Region &&
		current.ClusterID == selected.ClusterID && current.Priority == selected.Priority && current.Weight == selected.Weight &&
		current.Status == selected.Status && current.Version == selected.Version
}

func sameTargetHealthAuthority(current, selected persistence.ExecutionTargetHealth) bool {
	return current.ExecutionTargetID == selected.ExecutionTargetID && current.Version == selected.Version &&
		current.Status == selected.Status && current.CapacityStatus == selected.CapacityStatus &&
		sameOptionalInt(current.AvailableCapacityUnits, selected.AvailableCapacityUnits) &&
		current.AllocatedCapacityUnits == selected.AllocatedCapacityUnits && current.Source == selected.Source &&
		sameOptionalString(current.Reason, selected.Reason) && current.ObservedAt.Equal(selected.ObservedAt) &&
		current.ExpiresAt.Equal(selected.ExpiresAt)
}

func sameTargetDRReadinessAuthority(current, selected persistence.ExecutionTargetDRReadiness) bool {
	return current.ExecutionTargetID == selected.ExecutionTargetID && current.SourceDRDomain == selected.SourceDRDomain &&
		current.DRDomain == selected.DRDomain && current.Version == selected.Version &&
		current.ReplicatedThroughAt.Equal(selected.ReplicatedThroughAt) &&
		current.ArtifactsReady == selected.ArtifactsReady && current.CheckpointsReady == selected.CheckpointsReady &&
		current.MemoryReady == selected.MemoryReady && current.PublisherIdentity == selected.PublisherIdentity &&
		sameOptionalString(current.Reason, selected.Reason) && current.ObservedAt.Equal(selected.ObservedAt) &&
		current.ExpiresAt.Equal(selected.ExpiresAt)
}

func sameOptionalUUID(left, right *uuid.UUID) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func sameOptionalInt(left, right *int) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func sameOptionalString(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func ApplySessionSelection(session *persistence.AgentSession, selection Selection, requestedTargetID *uuid.UUID, preferredRegion *string) {
	if session == nil {
		return
	}
	session.ExecutionTargetID = selection.Target.ID
	if requestedTargetID != nil && *requestedTargetID != uuid.Nil {
		session.RequestedExecutionTargetID = *requestedTargetID
	} else if session.RequestedExecutionTargetID == uuid.Nil {
		session.RequestedExecutionTargetID = selection.Target.ID
	}
	groupID := selection.Group.ID
	version := selection.Group.Version
	session.ExecutionTargetGroupID = &groupID
	session.RoutingPolicyVersion = &version
	session.PreferredExecutionRegion = preferredRegion
}

func ApplyExecutionSelection(execution *persistence.AgentExecution, selection Selection) {
	if execution == nil {
		return
	}
	groupID := selection.Group.ID
	groupVersion := selection.Group.Version
	memberVersion := selection.Member.Version
	region := selection.Member.Region
	clusterID := selection.Member.ClusterID
	reason := selection.RoutingReason
	execution.TargetGroupID = &groupID
	execution.TargetGroupVersion = &groupVersion
	execution.TargetGroupMemberVersion = &memberVersion
	execution.SelectedRegion = &region
	execution.SelectedClusterID = &clusterID
	execution.RoutingReason = &reason
	if execution.PlacementRegion == "" {
		execution.PlacementRegion = region
	}
	if execution.PlacementClusterID == "" {
		execution.PlacementClusterID = clusterID
	}
}

type routeCandidate struct {
	member               persistence.ExecutionTargetGroupMember
	target               persistence.ExecutionTarget
	health               persistence.ExecutionTargetHealth
	drReadiness          *persistence.ExecutionTargetDRReadiness
	preferred            bool
	regionRank           int
	healthRank           int
	providerAffinityRank int
	queuePressure        QueuePressureSnapshot
}

type drReadinessRequirementState struct {
	required            bool
	reason              string
	sourceDRDomain      string
	destinationDRDomain string
	replicatedThroughAt time.Time
	requiredStores      DRStoreRequirements
}

type blockedDRCandidate struct {
	candidate     routeCandidate
	requirement   drReadinessRequirementState
	blockedReason string
	readiness     *persistence.ExecutionTargetDRReadiness
	unreadyStores []string
}

type blockedLocationCandidate struct {
	candidate routeCandidate
	outage    persistence.ExecutionLocationOutage
}

func (candidate blockedDRCandidate) problem() error {
	code := "target_group_dr_readiness_required"
	message := "Cross-domain placement requires a fresh DR readiness authority for the exact source domain and replication watermark."
	switch candidate.blockedReason {
	case "dr-readiness-context-missing":
		code = "target_group_dr_readiness_context_required"
		message = "Cross-domain placement is blocked because the required recovery authority watermark or source DR domain is missing."
	case "dr-readiness-expired":
		code = "target_group_dr_readiness_expired"
		message = "Cross-domain placement requires a non-expired DR readiness authority."
	case "dr-readiness-unready":
		code = "target_group_dr_readiness_unready"
		message = "Cross-domain placement is blocked because the target's DR backing stores are not all ready."
	case "dr-readiness-source-mismatch":
		code = "target_group_dr_source_domain_mismatch"
		message = "Cross-domain placement is blocked because the target DR readiness authority does not cover the required source DR domain."
	case "dr-readiness-destination-mismatch":
		code = "target_group_dr_destination_domain_mismatch"
		message = "Cross-domain placement is blocked because the target DR readiness authority does not cover the candidate's current destination DR domain."
	case "dr-readiness-observed-in-future":
		code = "target_group_dr_readiness_observed_in_future"
		message = "Cross-domain placement is blocked because the target DR readiness authority was observed later than server time."
	case "dr-readiness-watermark-stale":
		code = "target_group_dr_recovery_watermark_stale"
		message = "Cross-domain placement is blocked because the target DR readiness watermark does not cover the required recovery authority timestamp."
	}
	apiError := problem.New(409, code, message)
	apiError.Details = map[string]any{
		"blockedReason":        candidate.blockedReason,
		"readinessRequiredFor": candidate.requirement.reason,
		"sourceDrDomain":       candidate.requirement.sourceDRDomain,
		"destinationDrDomain":  candidate.requirement.destinationDRDomain,
		"executionTargetId":    candidate.candidate.target.ID,
		"region":               candidate.candidate.member.Region,
		"clusterId":            candidate.candidate.member.ClusterID,
	}
	if !candidate.requirement.replicatedThroughAt.IsZero() {
		apiError.Details["replicatedThroughAt"] = candidate.requirement.replicatedThroughAt
	}
	if requiredStores := candidate.requirement.requiredStores.Names(); len(requiredStores) != 0 {
		apiError.Details["requiredStores"] = requiredStores
	}
	if len(candidate.unreadyStores) != 0 {
		apiError.Details["stores"] = append([]string(nil), candidate.unreadyStores...)
	}
	if candidate.readiness != nil {
		apiError.Details["authority"] = toTargetDRReadinessView(*candidate.readiness)
	}
	return apiError
}

func (candidate blockedLocationCandidate) problem() error {
	apiError := problem.New(409, "target_group_location_outage_active", "Target Group placement is blocked by an active region or cluster outage authority.")
	apiError.Details = map[string]any{
		"executionTargetId": candidate.candidate.target.ID,
		"region":            candidate.candidate.member.Region,
	}
	if candidate.candidate.member.ClusterID != "" {
		apiError.Details["clusterId"] = candidate.candidate.member.ClusterID
	}
	apiError.Details["authority"] = toLocationOutageView(candidate.outage)
	return apiError
}

func candidateLess(left, right routeCandidate, strategy string) bool {
	if left.preferred != right.preferred {
		return left.preferred
	}
	if left.healthRank != right.healthRank {
		return left.healthRank < right.healthRank
	}
	if left.regionRank != right.regionRank {
		return left.regionRank < right.regionRank
	}
	if left.providerAffinityRank != right.providerAffinityRank {
		return left.providerAffinityRank < right.providerAffinityRank
	}
	if strategy == StrategyBalanced && left.queuePressure.EffectiveLoadRank != right.queuePressure.EffectiveLoadRank {
		return left.queuePressure.EffectiveLoadRank < right.queuePressure.EffectiveLoadRank
	}
	if left.member.Priority != right.member.Priority {
		return left.member.Priority < right.member.Priority
	}
	if strategy != StrategyBalanced && left.queuePressure.EffectiveLoadRank != right.queuePressure.EffectiveLoadRank {
		return left.queuePressure.EffectiveLoadRank < right.queuePressure.EffectiveLoadRank
	}
	if left.member.Weight != right.member.Weight {
		return left.member.Weight > right.member.Weight
	}
	return left.target.ID.String() < right.target.ID.String()
}

func healthEligible(health persistence.ExecutionTargetHealth, group persistence.ExecutionTargetGroup, now time.Time) bool {
	if health.Status != HealthHealthy && health.Status != HealthDegraded {
		return false
	}
	if health.ObservedAt.After(now) || !health.ExpiresAt.After(now) || health.ObservedAt.Add(time.Duration(group.HealthMaxStalenessSeconds)*time.Second).Before(now) {
		return false
	}
	if health.CapacityStatus == CapacitySaturated {
		return false
	}
	return health.AvailableCapacityUnits == nil || health.AllocatedCapacityUnits < *health.AvailableCapacityUnits
}

func drReadinessRequirement(
	request SelectRequest,
	candidate persistence.ExecutionTargetGroupMember,
) drReadinessRequirementState {
	sourceRegion := strings.TrimSpace(request.SourceRegion)
	sourceClusterID := strings.TrimSpace(request.SourceClusterID)
	requirement := drReadinessRequirementState{
		sourceDRDomain:      strings.TrimSpace(request.SourceDRDomain),
		destinationDRDomain: DRDomainForLocation(candidate.Region, candidate.ClusterID),
		replicatedThroughAt: request.ReplicatedThroughAt.UTC(),
		requiredStores:      request.RequiredDRStores,
	}
	if requirement.sourceDRDomain == "" {
		if sourceRegion != "" && sourceClusterID != "" {
			requirement.sourceDRDomain = DRDomainForLocation(sourceRegion, sourceClusterID)
		}
	}
	if request.PreferredTargetID != nil &&
		*request.PreferredTargetID != uuid.Nil &&
		candidate.ExecutionTargetID == *request.PreferredTargetID &&
		sourceRegion != "" &&
		sourceClusterID != "" &&
		candidate.Region == sourceRegion &&
		candidate.ClusterID == sourceClusterID {
		return requirement
	}
	if !request.RequiredDRStores.Any() {
		return requirement
	}
	if sourceRegion == "" || sourceClusterID == "" {
		requirement.required = true
		if request.DisasterRecovery {
			requirement.reason = "disaster-recovery"
		} else {
			requirement.reason = "cross-domain"
		}
		return requirement
	}
	if candidate.Region == sourceRegion && candidate.ClusterID == sourceClusterID {
		return requirement
	}
	requirement.required = true
	if request.DisasterRecovery {
		requirement.reason = "disaster-recovery"
	} else if candidate.Region != sourceRegion {
		requirement.reason = "cross-region"
	} else {
		requirement.reason = "cross-cluster"
	}
	return requirement
}

func (requirements DRStoreRequirements) unreadyStores(
	readiness persistence.ExecutionTargetDRReadiness,
) []string {
	stores := make([]string, 0, 3)
	if requirements.Artifacts && !readiness.ArtifactsReady {
		stores = append(stores, "artifacts")
	}
	if requirements.Checkpoints && !readiness.CheckpointsReady {
		stores = append(stores, "checkpoints")
	}
	if requirements.Memory && !readiness.MemoryReady {
		stores = append(stores, "memory")
	}
	return stores
}

func drReadinessBlockedReason(
	readiness persistence.ExecutionTargetDRReadiness,
	requirement drReadinessRequirementState,
	now time.Time,
) (string, []string) {
	if readiness.SourceDRDomain != requirement.sourceDRDomain {
		return "dr-readiness-source-mismatch", nil
	}
	if readiness.DRDomain != requirement.destinationDRDomain {
		return "dr-readiness-destination-mismatch", nil
	}
	if readiness.ObservedAt.After(now) {
		return "dr-readiness-observed-in-future", nil
	}
	if !readiness.ExpiresAt.After(now) {
		return "dr-readiness-expired", nil
	}
	if readiness.ReplicatedThroughAt.Before(requirement.replicatedThroughAt) {
		return "dr-readiness-watermark-stale", nil
	}
	unreadyStores := requirement.requiredStores.unreadyStores(readiness)
	if len(unreadyStores) != 0 {
		return "dr-readiness-unready", unreadyStores
	}
	return "", nil
}

func queuePressureSnapshot(
	health persistence.ExecutionTargetHealth,
	queuedExecutionUnits int64,
	weight int,
) QueuePressureSnapshot {
	return QueuePressureSnapshot{
		QueuedExecutionUnits: queuedExecutionUnits,
		EffectiveLoadRank:    effectiveLoadRank(health, queuedExecutionUnits, weight),
	}
}

func effectiveLoadRank(health persistence.ExecutionTargetHealth, queuedExecutionUnits int64, weight int) int64 {
	if weight <= 0 {
		weight = 1
	}
	pressure := queuedExecutionUnits
	if health.AvailableCapacityUnits != nil && *health.AvailableCapacityUnits > 0 {
		pressure += int64(health.AllocatedCapacityUnits)
	}
	if pressure < 0 || pressure > math.MaxInt64/1_000_000 {
		return math.MaxInt64
	}
	denominator := int64(weight)
	if health.AvailableCapacityUnits != nil && *health.AvailableCapacityUnits > 0 {
		available := int64(*health.AvailableCapacityUnits)
		if available > math.MaxInt64/denominator {
			denominator = math.MaxInt64
		} else {
			denominator *= available
		}
	}
	if denominator <= 0 {
		return math.MaxInt64
	}
	return pressure * 1_000_000 / denominator
}

type queuedExecutionUnitsRow struct {
	ExecutionTargetID    uuid.UUID `gorm:"column:execution_target_id"`
	QueuedExecutionUnits int64     `gorm:"column:queued_execution_units"`
}

func loadQueuedExecutionUnits(
	ctx context.Context,
	db *gorm.DB,
	targetIDs []uuid.UUID,
) (map[uuid.UUID]int64, error) {
	unitsByTarget := make(map[uuid.UUID]int64, len(targetIDs))
	if len(targetIDs) == 0 {
		return unitsByTarget, nil
	}
	var rows []queuedExecutionUnitsRow
	if err := db.WithContext(ctx).
		Model(&persistence.AgentExecution{}).
		Select("execution_target_id, COUNT(*) AS queued_execution_units").
		Where("execution_target_id IN ? AND status IN ?", targetIDs, []string{"queued", "recovering"}).
		Group("execution_target_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.ExecutionTargetID != uuid.Nil && row.QueuedExecutionUnits >= 0 {
			unitsByTarget[row.ExecutionTargetID] = row.QueuedExecutionUnits
		}
	}
	return unitsByTarget, nil
}

func healthStatusRank(status string) int {
	if status == HealthHealthy {
		return 0
	}
	return 1
}

func providerAffinityRank(capabilities map[string]any, provider string) (int, error) {
	canonicalProvider, valid := providercatalog.CanonicalName(provider)
	if !valid {
		return 1, nil
	}
	preference, err := parseTargetRoutingPreference(capabilities, canonicalProvider)
	if err != nil {
		return 0, err
	}
	switch preference {
	case "prefer":
		return 0, nil
	case "avoid":
		return 2, nil
	default:
		return 1, nil
	}
}

func parseTargetRoutingPreference(capabilities map[string]any, provider string) (string, error) {
	rawPolicy, found := capabilities["providerPolicy"]
	if !found {
		return "", nil
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok {
		return "", invalidTargetProviderPolicy()
	}
	rawPreferences, found := policy["routingPreferences"]
	if !found {
		return "", nil
	}
	preferences, ok := rawPreferences.(map[string]any)
	if !ok {
		return "", invalidTargetProviderPolicy()
	}
	for key, preference := range preferences {
		canonical, valid := providercatalog.CanonicalName(key)
		if !valid || canonical != key {
			return "", invalidTargetProviderPolicy()
		}
		value, ok := preference.(string)
		if !ok {
			return "", invalidTargetProviderPolicy()
		}
		switch value {
		case "prefer", "avoid":
			if key == provider {
				return value, nil
			}
		default:
			return "", invalidTargetProviderPolicy()
		}
	}
	return "", nil
}

func invalidTargetProviderPolicy() error {
	return problem.New(
		500,
		"target_provider_policy_invalid",
		"Execution Target Provider Policy is invalid.",
	)
}

func bestBlockedCandidateIndex(candidates []blockedDRCandidate, strategy string) int {
	bestIndex := 0
	for index := 1; index < len(candidates); index++ {
		if candidateLess(candidates[index].candidate, candidates[bestIndex].candidate, strategy) {
			bestIndex = index
		}
	}
	return bestIndex
}

func bestBlockedLocationCandidateIndex(candidates []blockedLocationCandidate, strategy string) int {
	bestIndex := 0
	for index := 1; index < len(candidates); index++ {
		if candidateLess(candidates[index].candidate, candidates[bestIndex].candidate, strategy) {
			bestIndex = index
		}
	}
	return bestIndex
}

func indexOrMax(values []string, value string) int {
	for index, candidate := range values {
		if candidate == value {
			return index
		}
	}
	return math.MaxInt
}

func normalizeStrategy(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = StrategyPriority
	}
	if !slices.Contains([]string{StrategyPriority, StrategyBalanced, StrategyLatency}, value) {
		return "", problem.New(400, "invalid_target_group_strategy", "Target Group strategy must be priority, balanced, or latency.")
	}
	return value, nil
}

func normalizeRegions(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		region, err := normalizeLocation(value, 120, "invalid_target_group_region", "preferred region")
		if err != nil {
			return nil, err
		}
		if _, exists := seen[region]; exists {
			continue
		}
		seen[region] = struct{}{}
		result = append(result, region)
	}
	return result, nil
}

func normalizeLocation(value string, maximum int, code, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum || strings.ContainsAny(value, "\r\n\t\x00") {
		return "", problem.New(400, code, fmt.Sprintf("%s must be between 1 and %d characters.", label, maximum))
	}
	return value, nil
}

func normalizeOptionalLocation(value string, maximum int, code, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maximum || strings.ContainsAny(value, "\r\n\t\x00") {
		return "", problem.New(400, code, fmt.Sprintf("%s must be between 1 and %d characters.", label, maximum))
	}
	return value, nil
}

func DRDomainForLocation(region, clusterID string) string {
	return strings.TrimSpace(region) + "/" + strings.TrimSpace(clusterID)
}

func toTargetHealthView(health persistence.ExecutionTargetHealth) TargetHealthView {
	return TargetHealthView{
		Status: health.Status, CapacityStatus: health.CapacityStatus,
		AvailableCapacityUnits: health.AvailableCapacityUnits,
		AllocatedCapacityUnits: health.AllocatedCapacityUnits,
		Source:                 health.Source, Reason: health.Reason, ObservedAt: health.ObservedAt,
		ExpiresAt: health.ExpiresAt, Version: health.Version,
	}
}

func toTargetDRReadinessView(readiness persistence.ExecutionTargetDRReadiness) TargetDRReadinessView {
	return TargetDRReadinessView{
		SourceDRDomain:      readiness.SourceDRDomain,
		DRDomain:            readiness.DRDomain,
		ReplicatedThroughAt: readiness.ReplicatedThroughAt,
		ArtifactsReady:      readiness.ArtifactsReady,
		CheckpointsReady:    readiness.CheckpointsReady,
		MemoryReady:         readiness.MemoryReady,
		PublisherIdentity:   readiness.PublisherIdentity,
		Reason:              readiness.Reason,
		ObservedAt:          readiness.ObservedAt,
		ExpiresAt:           readiness.ExpiresAt,
		Version:             readiness.Version,
	}
}

type locationOutageIndex struct {
	region    map[string]persistence.ExecutionLocationOutage
	byCluster map[string]persistence.ExecutionLocationOutage
}

func buildLocationOutageIndex(outages []persistence.ExecutionLocationOutage) locationOutageIndex {
	index := locationOutageIndex{
		region:    make(map[string]persistence.ExecutionLocationOutage, len(outages)),
		byCluster: make(map[string]persistence.ExecutionLocationOutage, len(outages)),
	}
	for _, outage := range outages {
		if outage.ClusterID == "" {
			index.region[outage.Region] = outage
			continue
		}
		index.byCluster[outage.Region+"\x00"+outage.ClusterID] = outage
	}
	return index
}

func (index locationOutageIndex) match(region, clusterID string) (persistence.ExecutionLocationOutage, bool) {
	if outage, ok := index.byCluster[region+"\x00"+clusterID]; ok {
		return outage, true
	}
	outage, ok := index.region[region]
	return outage, ok
}

func toLocationOutageView(outage persistence.ExecutionLocationOutage) LocationOutageView {
	var clusterID *string
	if outage.ClusterID != "" {
		value := outage.ClusterID
		clusterID = &value
	}
	return LocationOutageView{
		Region:            outage.Region,
		ClusterID:         clusterID,
		Status:            outage.Status,
		PublisherIdentity: outage.PublisherIdentity,
		Reason:            outage.Reason,
		ObservedAt:        outage.ObservedAt,
		ExpiresAt:         outage.ExpiresAt,
		Version:           outage.Version,
	}
}

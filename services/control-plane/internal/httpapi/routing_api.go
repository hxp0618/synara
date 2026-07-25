package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

type createExecutionTargetGroupInput struct {
	OrganizationID            *uuid.UUID `json:"organizationId,omitempty"`
	Name                      string     `json:"name"`
	Strategy                  string     `json:"strategy"`
	PreferredRegions          []string   `json:"preferredRegions"`
	AllowCrossRegion          bool       `json:"allowCrossRegion"`
	MaxFailoverAttempts       *int       `json:"maxFailoverAttempts,omitempty"`
	HealthMaxStalenessSeconds int        `json:"healthMaxStalenessSeconds"`
}

type addExecutionTargetGroupMemberInput struct {
	ExecutionTargetID uuid.UUID `json:"executionTargetId"`
	Region            string    `json:"region"`
	ClusterID         string    `json:"clusterId"`
	Priority          int       `json:"priority"`
	Weight            int       `json:"weight"`
}

type observeExecutionTargetHealthInput struct {
	Status         string `json:"status"`
	CapacityStatus string `json:"capacityStatus"`
	// AvailableCapacityUnits is the total schedulable capacity ceiling; callers
	// with a free-unit measurement must add AllocatedCapacityUnits.
	AvailableCapacityUnits *int                                    `json:"availableCapacityUnits,omitempty"`
	AllocatedCapacityUnits int                                     `json:"allocatedCapacityUnits"`
	Reason                 *string                                 `json:"reason,omitempty"`
	ObservedAt             *time.Time                              `json:"observedAt,omitempty"`
	TTLSeconds             int                                     `json:"ttlSeconds"`
	DRReadiness            *observeExecutionTargetDRReadinessInput `json:"drReadiness,omitempty"`
}

type observeExecutionTargetDRReadinessInput struct {
	SourceDRDomain      string     `json:"sourceDrDomain"`
	DRDomain            string     `json:"drDomain"`
	ReplicatedThroughAt *time.Time `json:"replicatedThroughAt,omitempty"`
	ArtifactsReady      bool       `json:"artifactsReady"`
	CheckpointsReady    bool       `json:"checkpointsReady"`
	MemoryReady         bool       `json:"memoryReady"`
	PublisherIdentity   string     `json:"publisherIdentity"`
	Reason              *string    `json:"reason,omitempty"`
	ObservedAt          *time.Time `json:"observedAt,omitempty"`
	TTLSeconds          int        `json:"ttlSeconds"`
}

type observeLocationOutageInput struct {
	Region            string     `json:"region"`
	ClusterID         string     `json:"clusterId,omitempty"`
	Status            string     `json:"status"`
	PublisherIdentity string     `json:"publisherIdentity,omitempty"`
	Reason            *string    `json:"reason,omitempty"`
	ObservedAt        *time.Time `json:"observedAt,omitempty"`
	TTLSeconds        int        `json:"ttlSeconds"`
}

func (s *Server) listExecutionTargetGroups(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireRoutingPermission(w, r, tenantID, authorization.WorkerRead) {
		return
	}
	states, err := s.routing.List(r.Context(), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": states})
}

func (s *Server) createExecutionTargetGroup(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireRoutingPermission(w, r, tenantID, authorization.WorkerManage) {
		return
	}
	var input createExecutionTargetGroupInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	principal := mustPrincipal(r)
	if input.OrganizationID != nil {
		if _, err := authorization.NewAuthorizer(s.db).RequireOrganization(
			r.Context(), principal.UserID, tenantID, *input.OrganizationID, authorization.OrganizationRead,
		); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	group, err := s.routing.CreateGroup(r.Context(), routing.CreateGroupInput{
		TenantID: tenantID, OrganizationID: input.OrganizationID, Name: input.Name,
		Strategy: input.Strategy, PreferredRegions: input.PreferredRegions,
		AllowCrossRegion: input.AllowCrossRegion, MaxFailoverAttempts: input.MaxFailoverAttempts,
		HealthMaxStalenessSeconds: input.HealthMaxStalenessSeconds,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, routing.GroupViewOf(group))
}

func (s *Server) addExecutionTargetGroupMember(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireRoutingPermission(w, r, tenantID, authorization.WorkerManage) {
		return
	}
	targetGroupID, ok := s.pathUUID(w, r, "targetGroupID")
	if !ok {
		return
	}
	var input addExecutionTargetGroupMemberInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	member, err := s.routing.AddMember(r.Context(), routing.AddMemberInput{
		TenantID: tenantID, TargetGroupID: targetGroupID, ExecutionTargetID: input.ExecutionTargetID,
		Region: input.Region, ClusterID: input.ClusterID, Priority: input.Priority, Weight: input.Weight,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, routing.MemberViewOf(member))
}

func (s *Server) observeExecutionTargetHealth(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireRoutingPermission(w, r, tenantID, authorization.WorkerManage) {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	var target persistence.ExecutionTarget
	if err := s.db.WithContext(r.Context()).Select("id", "tenant_id").
		Where("id = ? AND (tenant_id IS NULL OR tenant_id = ?)", targetID, tenantID).Take(&target).Error; err != nil {
		s.writeError(w, r, problem.New(404, "execution_target_not_found", "Execution Target not found."))
		return
	}
	if target.TenantID == nil {
		s.writeError(
			w,
			r,
			problem.New(
				403,
				"shared_execution_target_health_immutable",
				"Platform-shared execution target health cannot be changed by a tenant.",
			),
		)
		return
	}
	var input observeExecutionTargetHealthInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(input.Status) == "" && input.DRReadiness == nil {
		s.writeError(w, r, problem.New(400, "invalid_target_authority_observation", "Provide health status and/or drReadiness."))
		return
	}
	principal := mustPrincipal(r)
	response := map[string]any{"executionTargetId": targetID}
	if strings.TrimSpace(input.Status) != "" {
		ttl := input.TTLSeconds
		if ttl == 0 {
			ttl = 60
		}
		var observedAt time.Time
		if input.ObservedAt != nil {
			observedAt = input.ObservedAt.UTC()
		}
		health, err := s.routing.ObserveHealth(r.Context(), routing.HealthObservation{
			ExecutionTargetID: targetID, Status: input.Status, CapacityStatus: input.CapacityStatus,
			AvailableCapacityUnits: input.AvailableCapacityUnits,
			AllocatedCapacityUnits: input.AllocatedCapacityUnits,
			Source:                 "operator:" + principal.UserID.String(), Reason: input.Reason,
			ObservedAt: observedAt, TTL: time.Duration(ttl) * time.Second,
		})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		response["health"] = routing.HealthViewOf(health)
	}
	if input.DRReadiness != nil {
		ttl := input.DRReadiness.TTLSeconds
		if ttl == 0 {
			ttl = 60
		}
		var observedAt time.Time
		if input.DRReadiness.ObservedAt != nil {
			observedAt = input.DRReadiness.ObservedAt.UTC()
		}
		var replicatedThroughAt time.Time
		if input.DRReadiness.ReplicatedThroughAt != nil {
			replicatedThroughAt = input.DRReadiness.ReplicatedThroughAt.UTC()
		}
		publisherIdentity := input.DRReadiness.PublisherIdentity
		if strings.TrimSpace(publisherIdentity) == "" {
			publisherIdentity = "operator:" + principal.UserID.String()
		}
		readiness, err := s.routing.ObserveDRReadiness(r.Context(), routing.DRReadinessObservation{
			ExecutionTargetID:   targetID,
			SourceDRDomain:      input.DRReadiness.SourceDRDomain,
			DRDomain:            input.DRReadiness.DRDomain,
			ReplicatedThroughAt: replicatedThroughAt,
			ArtifactsReady:      input.DRReadiness.ArtifactsReady,
			CheckpointsReady:    input.DRReadiness.CheckpointsReady,
			MemoryReady:         input.DRReadiness.MemoryReady,
			PublisherIdentity:   publisherIdentity,
			Reason:              input.DRReadiness.Reason,
			ObservedAt:          observedAt,
			TTL:                 time.Duration(ttl) * time.Second,
		})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		response["drReadiness"] = routing.DRReadinessViewOf(readiness)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listLocationOutages(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireRoutingPermission(w, r, tenantID, authorization.WorkerRead) {
		return
	}
	items, err := s.routing.ListLocationOutages(r.Context(), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) observeLocationOutage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireRoutingPermission(w, r, tenantID, authorization.WorkerManage) {
		return
	}
	var input observeLocationOutageInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	var observedAt time.Time
	if input.ObservedAt != nil {
		observedAt = input.ObservedAt.UTC()
	}
	ttl := input.TTLSeconds
	if ttl == 0 {
		ttl = 60
	}
	publisherIdentity := input.PublisherIdentity
	if strings.TrimSpace(publisherIdentity) == "" {
		publisherIdentity = "operator:" + mustPrincipal(r).UserID.String()
	}
	outage, err := s.routing.ObserveLocationOutage(r.Context(), routing.LocationOutageObservation{
		TenantID:          tenantID,
		Region:            input.Region,
		ClusterID:         input.ClusterID,
		Status:            input.Status,
		PublisherIdentity: publisherIdentity,
		Reason:            input.Reason,
		ObservedAt:        observedAt,
		TTL:               time.Duration(ttl) * time.Second,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, routing.LocationOutageViewOf(outage))
}

func (s *Server) requireRoutingPermission(
	w http.ResponseWriter,
	r *http.Request,
	tenantID uuid.UUID,
	permission authorization.Permission,
) bool {
	principal := mustPrincipal(r)
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		s.writeError(w, r, problem.New(409, "active_tenant_mismatch", "The path tenant is not the active tenant."))
		return false
	}
	if _, err := authorization.NewAuthorizer(s.db).RequireTenant(
		r.Context(), principal.UserID, tenantID, permission,
	); err != nil {
		s.writeError(w, r, err)
		return false
	}
	return true
}

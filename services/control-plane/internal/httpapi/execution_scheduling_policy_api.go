package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

type executionSchedulingPolicyUpdateBody struct {
	ExpectedVersion int64                     `json:"expectedVersion"`
	Document        schedulingpolicy.Document `json:"document"`
}

type executionSchedulingPolicyResponse struct {
	Scope     schedulingpolicy.ScopeSnapshot  `json:"scope"`
	Effective schedulingpolicy.Document       `json:"effective"`
	Parent    *schedulingpolicy.ScopeSnapshot `json:"parent,omitempty"`
}

func (s *Server) getTenantExecutionSchedulingPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireSchedulingPolicyTenantPermission(w, r, tenantID, authorization.SchedulingPolicyRead) {
		return
	}
	snapshot, err := s.schedulingPolicies.GetTenant(r.Context(), tenantID)
	if err != nil {
		s.writeError(w, r, executionSchedulingPolicyProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, tenantSchedulingPolicyResponse(snapshot))
}

func (s *Server) putTenantExecutionSchedulingPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireSchedulingPolicyTenantPermission(w, r, tenantID, authorization.SchedulingPolicyManage) {
		return
	}
	input, ok := s.decodeExecutionSchedulingPolicyUpdate(w, r)
	if !ok {
		return
	}
	principal := mustPrincipal(r)
	snapshot, err := s.schedulingPolicies.UpdateTenant(r.Context(), tenantID, schedulingpolicy.UpdateInput{
		ExpectedVersion: input.ExpectedVersion,
		Document:        input.Document,
		ActorID:         principal.UserID,
		RequestID:       requestID(r),
		IPAddress:       clientIP(r),
	})
	if err != nil {
		s.writeError(w, r, executionSchedulingPolicyProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, tenantSchedulingPolicyResponse(snapshot))
}

func (s *Server) getOrganizationExecutionSchedulingPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok || !s.requireSchedulingPolicyOrganizationPermission(
		w, r, tenantID, organizationID, authorization.SchedulingPolicyRead,
	) {
		return
	}
	snapshot, err := s.schedulingPolicies.GetOrganization(r.Context(), tenantID, organizationID)
	if err != nil {
		s.writeError(w, r, executionSchedulingPolicyProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, organizationSchedulingPolicyResponse(snapshot))
}

func (s *Server) putOrganizationExecutionSchedulingPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok || !s.requireSchedulingPolicyOrganizationPermission(
		w, r, tenantID, organizationID, authorization.SchedulingPolicyManage,
	) {
		return
	}
	input, ok := s.decodeExecutionSchedulingPolicyUpdate(w, r)
	if !ok {
		return
	}
	principal := mustPrincipal(r)
	snapshot, err := s.schedulingPolicies.UpdateOrganization(r.Context(), tenantID, organizationID, schedulingpolicy.UpdateInput{
		ExpectedVersion: input.ExpectedVersion,
		Document:        input.Document,
		ActorID:         principal.UserID,
		RequestID:       requestID(r),
		IPAddress:       clientIP(r),
	})
	if err != nil {
		s.writeError(w, r, executionSchedulingPolicyProblem(err))
		return
	}
	scope := snapshot.Organization
	if scope == nil {
		s.writeError(w, r, problem.New(500, "execution_scheduling_policy_corrupt", "The execution scheduling policy is corrupt."))
		return
	}
	writeJSON(w, http.StatusOK, organizationSchedulingPolicyResponse(snapshot))
}

func (s *Server) decodeExecutionSchedulingPolicyUpdate(
	w http.ResponseWriter,
	r *http.Request,
) (executionSchedulingPolicyUpdateBody, bool) {
	var input executionSchedulingPolicyUpdateBody
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return executionSchedulingPolicyUpdateBody{}, false
	}
	if input.ExpectedVersion < 0 {
		s.writeError(w, r, problem.New(400, "invalid_execution_scheduling_policy", "expectedVersion must be non-negative."))
		return executionSchedulingPolicyUpdateBody{}, false
	}
	if _, _, err := schedulingpolicy.Normalize(input.Document); err != nil {
		s.writeError(w, r, problem.Wrap(400, "invalid_execution_scheduling_policy", "The execution scheduling policy document is invalid.", err))
		return executionSchedulingPolicyUpdateBody{}, false
	}
	return input, true
}

func (s *Server) requireSchedulingPolicyTenantPermission(
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

func (s *Server) requireSchedulingPolicyOrganizationPermission(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, organizationID uuid.UUID,
	permission authorization.Permission,
) bool {
	principal := mustPrincipal(r)
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		s.writeError(w, r, problem.New(409, "active_tenant_mismatch", "The path tenant is not the active tenant."))
		return false
	}
	if _, err := authorization.NewAuthorizer(s.db).RequireOrganization(
		r.Context(), principal.UserID, tenantID, organizationID, permission,
	); err != nil {
		s.writeError(w, r, err)
		return false
	}
	return true
}

func tenantSchedulingPolicyResponse(snapshot schedulingpolicy.Snapshot) executionSchedulingPolicyResponse {
	return executionSchedulingPolicyResponse{Scope: snapshot.Tenant, Effective: snapshot.Effective}
}

func organizationSchedulingPolicyResponse(snapshot schedulingpolicy.Snapshot) executionSchedulingPolicyResponse {
	response := executionSchedulingPolicyResponse{Effective: snapshot.Effective, Parent: &snapshot.Tenant}
	if snapshot.Organization != nil {
		response.Scope = *snapshot.Organization
	}
	return response
}

func executionSchedulingPolicyProblem(err error) error {
	switch {
	case errors.Is(err, schedulingpolicy.ErrVersionConflict):
		return problem.New(409, "execution_scheduling_policy_version_conflict", "The execution scheduling policy changed before this update.")
	case errors.Is(err, schedulingpolicy.ErrOrganizationWidensTenant):
		return problem.New(409, "organization_execution_scheduling_policy_widens_tenant", "The organization execution scheduling policy cannot widen tenant authority.")
	case errors.Is(err, schedulingpolicy.ErrPolicyCorrupt):
		return problem.Wrap(500, "execution_scheduling_policy_corrupt", "The execution scheduling policy is corrupt.", err)
	default:
		return err
	}
}

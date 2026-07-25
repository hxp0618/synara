package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Server) getTenantResourceLifecyclePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	policy, err := s.lifecyclePolicies.GetTenant(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) putTenantResourceLifecyclePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input lifecyclepolicy.UpdateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	policy, err := s.lifecyclePolicies.UpdateTenant(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) getProjectResourceLifecyclePolicy(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	tenantID, err := sessions.ActiveTenant(mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	policy, err := s.lifecyclePolicies.GetProject(r.Context(), mustPrincipal(r), tenantID, projectID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) putProjectResourceLifecyclePolicy(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	tenantID, err := sessions.ActiveTenant(mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var input lifecyclepolicy.UpdateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	policy, err := s.lifecyclePolicies.UpdateProject(
		r.Context(), mustPrincipal(r), tenantID, projectID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

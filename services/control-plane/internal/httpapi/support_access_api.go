package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
)

func (s *Server) getTenantSupportPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	result, err := s.supportAccess.GetPolicy(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) updateTenantSupportPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input supportaccess.UpdatePolicyInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.supportAccess.UpdatePolicy(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listTenantSupportAccess(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.supportAccess.ListTenant(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) revokeTenantSupportAccess(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	if err := identity.RequireActiveTenant(mustPrincipal(r), tenantID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.revokeSupportAccess(w, r)
}

func (s *Server) listPlatformSupportAccess(w http.ResponseWriter, r *http.Request) {
	items, err := s.supportAccess.ListPlatform(r.Context(), mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) listPlatformTenants(w http.ResponseWriter, r *http.Request) {
	result, err := s.supportAccess.ListPlatformTenants(r.Context(), mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) provisionPlatformTenant(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformAdmin(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return
	}
	var input tenancy.ProvisionTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.tenancy.ProvisionTenant(
		r.Context(), principal, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) requestPlatformSupportAccess(w http.ResponseWriter, r *http.Request) {
	var input supportaccess.RequestInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.supportAccess.Request(
		r.Context(), mustPrincipal(r), input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) approvePlatformSupportAccess(w http.ResponseWriter, r *http.Request) {
	grantID, input, ok := s.supportAccessDecisionInput(w, r)
	if !ok {
		return
	}
	result, err := s.supportAccess.Approve(
		r.Context(), mustPrincipal(r), grantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) denyPlatformSupportAccess(w http.ResponseWriter, r *http.Request) {
	grantID, input, ok := s.supportAccessDecisionInput(w, r)
	if !ok {
		return
	}
	result, err := s.supportAccess.Deny(
		r.Context(), mustPrincipal(r), grantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) revokePlatformSupportAccess(w http.ResponseWriter, r *http.Request) {
	s.revokeSupportAccess(w, r)
}

func (s *Server) revokeSupportAccess(w http.ResponseWriter, r *http.Request) {
	grantID, input, ok := s.supportAccessDecisionInput(w, r)
	if !ok {
		return
	}
	result, err := s.supportAccess.Revoke(
		r.Context(), mustPrincipal(r), grantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) supportAccessDecisionInput(
	w http.ResponseWriter,
	r *http.Request,
) (uuid.UUID, supportaccess.DecisionInput, bool) {
	grantID, ok := s.pathUUID(w, r, "grantID")
	if !ok {
		return uuid.Nil, supportaccess.DecisionInput{}, false
	}
	var input supportaccess.DecisionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return uuid.Nil, supportaccess.DecisionInput{}, false
	}
	return grantID, input, true
}

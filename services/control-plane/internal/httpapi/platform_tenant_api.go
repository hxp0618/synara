package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) getPlatformTenantEntitlements(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.authorizePlatformTenant(w, r)
	if !ok {
		return
	}
	snapshot, err := s.entitlements.Resolve(r.Context(), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entitlementProfileView(snapshot))
}

func (s *Server) assignPlatformTenantEntitlementProfile(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := s.authorizePlatformTenant(w, r)
	if !ok {
		return
	}
	var request assignEntitlementProfileRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	if request.Status != "active" && request.Status != "evaluation" {
		s.writeError(w, r, problem.New(
			http.StatusBadRequest,
			"platform_entitlement_profile_status_unsupported",
			"Platform entitlement profile management supports active or evaluation assignments; suspend or close the Tenant through its lifecycle workflow.",
		))
		return
	}
	input := request.internalInput()
	input.AssignmentSource = "platform_admin"
	snapshot, err := s.entitlements.AssignPlanByPlatform(
		r.Context(), principal.UserID, tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entitlementProfileView(snapshot))
}

func (s *Server) authorizePlatformTenant(
	w http.ResponseWriter,
	r *http.Request,
) (uuid.UUID, identity.Principal, bool) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformAdmin(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return uuid.Nil, identity.Principal{}, false
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return uuid.Nil, identity.Principal{}, false
	}
	if tenantID == s.config.PlatformOperatorTenantID {
		s.writeError(w, r, problem.New(
			http.StatusForbidden,
			"platform_operator_entitlement_profile_forbidden",
			"The Platform Operator Tenant is not an internal workload entitlement-profile target.",
		))
		return uuid.Nil, identity.Principal{}, false
	}
	return tenantID, principal, true
}

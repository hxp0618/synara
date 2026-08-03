package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercommercial"
)

func (s *Server) listPlatformProviderCommercialAuthorizations(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authorizeProviderCommercial(w, r, false); !ok {
		return
	}
	items, err := s.providerCommercial.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createPlatformProviderCommercialAuthorization(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeProviderCommercial(w, r, true)
	if !ok {
		return
	}
	var input providercommercial.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.providerCommercial.Create(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) recordPlatformProviderCommercialApproval(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeProviderCommercial(w, r, false)
	if !ok {
		return
	}
	authorizationID, ok := s.pathUUID(w, r, "authorizationID")
	if !ok {
		return
	}
	var input providercommercial.ApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.providerCommercial.RecordApproval(r.Context(), principal.UserID, authorizationID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) transitionPlatformProviderCommercialAuthorization(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeProviderCommercial(w, r, true)
	if !ok {
		return
	}
	authorizationID, ok := s.pathUUID(w, r, "authorizationID")
	if !ok {
		return
	}
	var input providercommercial.TransitionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.providerCommercial.Transition(r.Context(), principal.UserID, authorizationID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) authorizeProviderCommercial(w http.ResponseWriter, r *http.Request, manage bool) (identity.Principal, string, bool) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return identity.Principal{}, "", false
	}
	if manage && role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "provider_commercial_manage_forbidden", "Platform Owner or Admin authority is required to manage Provider commercial authorizations."))
		return identity.Principal{}, "", false
	}
	return principal, role, true
}

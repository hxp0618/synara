package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listPlatformGovernanceAuthorities(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authorizeGovernanceAuthority(w, r, false); !ok {
		return
	}
	items, err := s.governanceAuthority.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	operators, err := s.governanceAuthority.ListEligibleOperators(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "authorityKeys": governanceauthority.AuthorityKeys, "operators": operators})
}

func (s *Server) createPlatformGovernanceAuthority(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeGovernanceAuthority(w, r, true)
	if !ok {
		return
	}
	var input governanceauthority.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.governanceAuthority.Create(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) revokePlatformGovernanceAuthority(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeGovernanceAuthority(w, r, true)
	if !ok {
		return
	}
	grantID, ok := s.pathUUID(w, r, "grantID")
	if !ok {
		return
	}
	var input governanceauthority.RevokeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.governanceAuthority.Revoke(r.Context(), principal.UserID, grantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) authorizeGovernanceAuthority(w http.ResponseWriter, r *http.Request, manage bool) (identity.Principal, string, bool) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return identity.Principal{}, "", false
	}
	if manage && role != "owner" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "governance_authority_manage_forbidden", "Platform Owner authority is required to manage Stage 6 governance authorities."))
		return identity.Principal{}, "", false
	}
	return principal, role, true
}

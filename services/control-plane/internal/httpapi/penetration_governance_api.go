package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/penetrationgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listPlatformPenetrationEngagements(w http.ResponseWriter, r *http.Request) {
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), mustPrincipal(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.penetrationGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) importPlatformPenetrationEngagement(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "penetration_engagement_import_forbidden", "Platform Owner or Admin authority is required to import a Penetration engagement."))
		return
	}
	var input penetrationgovernance.ImportInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.penetrationGovernance.Import(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) recordPlatformPenetrationApproval(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return
	}
	recordID, ok := s.pathUUID(w, r, "penetrationEngagementID")
	if !ok {
		return
	}
	var input penetrationgovernance.ApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.penetrationGovernance.RecordApproval(r.Context(), principal.UserID, recordID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

package httpapi

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) projectProviderCapabilities(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	var requestedTargetID *uuid.UUID
	if rawTargetID := strings.TrimSpace(r.URL.Query().Get("executionTargetId")); rawTargetID != "" {
		parsed, err := uuid.Parse(rawTargetID)
		if err != nil {
			s.writeError(w, r, problem.New(400, "invalid_query_parameter", "executionTargetId must be a UUID."))
			return
		}
		requestedTargetID = &parsed
	}
	projection, err := s.executions.ProjectProviderCapabilitiesForProject(
		r.Context(), mustPrincipal(r), projectID, requestedTargetID,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

func (s *Server) sessionProviderCapabilities(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	projection, err := s.executions.ProjectProviderCapabilitiesForSession(
		r.Context(), mustPrincipal(r), sessionID,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

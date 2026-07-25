package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/memories"
)

func (s *Server) publishMemoryRevision(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input memories.PublishInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	publication, err := s.memories.Publish(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, publication)
}

func (s *Server) listEffectiveMemoryReferences(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	references, err := s.memories.ListEffectiveForSession(r.Context(), mustPrincipal(r), sessionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": references})
}

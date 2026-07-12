package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/retention"
)

func (s *Server) getRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	policy, err := s.retention.Get(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) putRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input retention.UpdateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	policy, err := s.retention.Update(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func (s *Server) resolveExecutionCredentialGrant(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	grantID, ok := s.pathUUID(w, r, "grantID")
	if !ok {
		return
	}
	var input executions.LeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	resolved, err := s.credentials.ResolveGrantForExecution(
		r.Context(), s.executions, mustWorker(r), executionID, grantID, input,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resolved)
}

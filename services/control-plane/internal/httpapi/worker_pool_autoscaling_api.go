package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/poolautoscaling"
)

func (s *Server) getWorkerPoolAutoscaling(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	poolID, ok := s.pathUUID(w, r, "workerPoolID")
	if !ok {
		return
	}
	view, err := s.poolAutoscaling.Get(r.Context(), mustPrincipal(r), tenantID, targetID, poolID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) putWorkerPoolAutoscaling(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	poolID, ok := s.pathUUID(w, r, "workerPoolID")
	if !ok {
		return
	}
	var input poolautoscaling.PutInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	view, err := s.poolAutoscaling.Put(
		r.Context(), mustPrincipal(r), tenantID, targetID, poolID,
		input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

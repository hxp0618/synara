package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

func (s *Server) listWorkerPools(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	state, err := s.placement.List(r.Context(), mustPrincipal(r), tenantID, targetID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) createWorkerPool(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	var input placement.CreatePoolInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	pool, err := s.placement.CreatePool(r.Context(), mustPrincipal(r), tenantID, targetID, input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, pool)
}

func (s *Server) updateWorkerPool(w http.ResponseWriter, r *http.Request) {
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
	var input placement.UpdatePoolInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	pool, err := s.placement.UpdatePool(r.Context(), mustPrincipal(r), tenantID, targetID, poolID, input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pool)
}

func (s *Server) updateExecutionPlacementPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	var input placement.UpdatePolicyInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	state, err := s.placement.UpdatePolicy(r.Context(), mustPrincipal(r), tenantID, targetID, input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

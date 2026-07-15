package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
)

func (s *Server) listWorkerReleases(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	overview, err := s.workerReleases.List(r.Context(), mustPrincipal(r), tenantID, targetID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) createWorkerRelease(w http.ResponseWriter, r *http.Request) {
	tenantID, targetID, ok := s.workerReleaseTargetPath(w, r)
	if !ok {
		return
	}
	var input workerreleases.CreateRevisionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.workerReleases.CreateRevision(
		r.Context(), mustPrincipal(r), tenantID, targetID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) startWorkerReleaseCanary(w http.ResponseWriter, r *http.Request) {
	s.handleWorkerReleasePolicyAction(w, r, "canary")
}

func (s *Server) promoteWorkerRelease(w http.ResponseWriter, r *http.Request) {
	s.handleWorkerReleasePolicyAction(w, r, "promote")
}

func (s *Server) rollbackWorkerRelease(w http.ResponseWriter, r *http.Request) {
	s.handleWorkerReleasePolicyAction(w, r, "rollback")
}

func (s *Server) handleWorkerReleasePolicyAction(w http.ResponseWriter, r *http.Request, action string) {
	tenantID, targetID, ok := s.workerReleaseTargetPath(w, r)
	if !ok {
		return
	}
	revisionID, ok := s.pathUUID(w, r, "releaseRevisionID")
	if !ok {
		return
	}
	var input workerreleases.PolicyChangeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	var (
		result workerreleases.OperationResult[workerreleases.Policy]
		err    error
	)
	switch action {
	case "canary":
		result, err = s.workerReleases.StartCanary(
			r.Context(), mustPrincipal(r), tenantID, targetID, revisionID, input,
			r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
		)
	case "promote":
		result, err = s.workerReleases.Promote(
			r.Context(), mustPrincipal(r), tenantID, targetID, revisionID, input,
			r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
		)
	case "rollback":
		result, err = s.workerReleases.Rollback(
			r.Context(), mustPrincipal(r), tenantID, targetID, revisionID, input,
			r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
		)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) workerReleaseTargetPath(w http.ResponseWriter, r *http.Request) (tenantID, targetID uuid.UUID, ok bool) {
	tenant, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return tenantID, targetID, false
	}
	target, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return tenantID, targetID, false
	}
	return tenant, target, true
}

package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.credentials.List(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createCredential(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input credentials.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.credentials.Create(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) rotateCredential(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	credentialID, ok := s.pathUUID(w, r, "credentialID")
	if !ok {
		return
	}
	var input credentials.RotateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.credentials.Rotate(r.Context(), mustPrincipal(r), tenantID, credentialID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	credentialID, ok := s.pathUUID(w, r, "credentialID")
	if !ok {
		return
	}
	if err := s.credentials.Revoke(r.Context(), mustPrincipal(r), tenantID, credentialID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolveExecutionCredential(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	credentialID, ok := s.pathUUID(w, r, "credentialID")
	if !ok {
		return
	}
	var input executions.LeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload, err := s.credentials.ResolveForExecution(
		r.Context(), s.executions, mustWorker(r), executionID, credentialID, input,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"payload": payload})
}

func (s *Server) resolveExecutionGitCredential(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	credentialID, ok := s.pathUUID(w, r, "credentialID")
	if !ok {
		return
	}
	var input executions.LeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload, err := s.credentials.ResolveGitForExecution(
		r.Context(), s.executions, mustWorker(r), executionID, credentialID, input,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"payload": payload})
}

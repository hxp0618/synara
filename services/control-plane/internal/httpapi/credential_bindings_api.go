package httpapi

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialbindings"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listCredentialBindings(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	filter, err := credentialBindingOwnerFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.credentialBindings.List(r.Context(), mustPrincipal(r), tenantID, filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createCredentialBinding(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input credentialbindings.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.credentialBindings.Create(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) disableCredentialBinding(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	bindingID, ok := s.pathUUID(w, r, "bindingID")
	if !ok {
		return
	}
	item, err := s.credentialBindings.Disable(
		r.Context(), mustPrincipal(r), tenantID, bindingID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func credentialBindingOwnerFilter(r *http.Request) (credentialbindings.OwnerFilter, error) {
	projectValue := strings.TrimSpace(r.URL.Query().Get("projectId"))
	targetValue := strings.TrimSpace(r.URL.Query().Get("executionTargetId"))
	if (projectValue == "") == (targetValue == "") {
		return credentialbindings.OwnerFilter{}, problem.New(
			400,
			"invalid_credential_binding_filter",
			"Exactly one projectId or executionTargetId query parameter is required.",
		)
	}
	if projectValue != "" {
		projectID, err := uuid.Parse(projectValue)
		if err != nil {
			return credentialbindings.OwnerFilter{}, problem.New(400, "invalid_project_id", "projectId is invalid.")
		}
		return credentialbindings.OwnerFilter{ProjectID: &projectID}, nil
	}
	targetID, err := uuid.Parse(targetValue)
	if err != nil {
		return credentialbindings.OwnerFilter{}, problem.New(400, "invalid_execution_target_id", "executionTargetId is invalid.")
	}
	return credentialbindings.OwnerFilter{ExecutionTargetID: &targetID}, nil
}

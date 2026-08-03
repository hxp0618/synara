package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/incidentgovernance"
)

func (s *Server) listPlatformIncidents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeIncidentGovernance(w, r); !ok {
		return
	}
	items, err := s.incidentGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createPlatformIncident(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeIncidentGovernance(w, r)
	if !ok {
		return
	}
	var input incidentgovernance.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	incident, err := s.incidentGovernance.Create(
		r.Context(), principal.UserID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, incident)
}

func (s *Server) bindPlatformIncidentStatusBoard(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeIncidentGovernance(w, r)
	if !ok {
		return
	}
	incidentID, ok := s.pathUUID(w, r, "incidentID")
	if !ok {
		return
	}
	var input incidentgovernance.BindStatusBoardInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	incident, err := s.incidentGovernance.BindStatusBoard(
		r.Context(), principal.UserID, incidentID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (s *Server) addPlatformIncidentInternalUpdate(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeIncidentGovernance(w, r)
	if !ok {
		return
	}
	incidentID, ok := s.pathUUID(w, r, "incidentID")
	if !ok {
		return
	}
	var input incidentgovernance.AddUpdateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	incident, err := s.incidentGovernance.AddInternalUpdate(
		r.Context(), principal.UserID, incidentID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (s *Server) recordPlatformIncidentResolutionApproval(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeIncidentGovernance(w, r)
	if !ok {
		return
	}
	incidentID, ok := s.pathUUID(w, r, "incidentID")
	if !ok {
		return
	}
	var input incidentgovernance.ResolutionApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	incident, err := s.incidentGovernance.RecordResolutionApproval(
		r.Context(), principal.UserID, incidentID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (s *Server) transitionPlatformIncident(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeIncidentGovernance(w, r)
	if !ok {
		return
	}
	incidentID, ok := s.pathUUID(w, r, "incidentID")
	if !ok {
		return
	}
	var input incidentgovernance.TransitionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	incident, err := s.incidentGovernance.Transition(
		r.Context(), principal.UserID, incidentID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (s *Server) authorizeIncidentGovernance(
	w http.ResponseWriter,
	r *http.Request,
) (identity.Principal, bool) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return identity.Principal{}, false
	}
	return principal, true
}

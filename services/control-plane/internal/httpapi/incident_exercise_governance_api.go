package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/incidentexercisegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listPlatformIncidentExercises(w http.ResponseWriter, r *http.Request) {
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), mustPrincipal(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.incidentExerciseGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) importPlatformIncidentExercise(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "incident_exercise_import_forbidden", "Platform Owner or Admin authority is required to import an Incident exercise."))
		return
	}
	var input incidentexercisegovernance.ImportInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.incidentExerciseGovernance.Import(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) recordPlatformIncidentExerciseApproval(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return
	}
	exerciseID, ok := s.pathUUID(w, r, "incidentExerciseID")
	if !ok {
		return
	}
	var input incidentexercisegovernance.ApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.incidentExerciseGovernance.RecordApproval(r.Context(), principal.UserID, exerciseID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

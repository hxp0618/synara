package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/operationsexercisegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listPlatformOperationsExercises(w http.ResponseWriter, r *http.Request) {
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), mustPrincipal(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.operationsExerciseGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) importPlatformOperationsExercise(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "operations_exercise_import_forbidden", "Platform Owner or Admin authority is required to import an Operations exercise."))
		return
	}
	var input operationsexercisegovernance.ImportInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.operationsExerciseGovernance.Import(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) recordPlatformOperationsExerciseApproval(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return
	}
	exerciseID, ok := s.pathUUID(w, r, "operationsExerciseID")
	if !ok {
		return
	}
	var input operationsexercisegovernance.ApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.operationsExerciseGovernance.RecordApproval(r.Context(), principal.UserID, exerciseID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

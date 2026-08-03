package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/recoverygovernance"
)

func (s *Server) listPlatformRecoveryDrills(w http.ResponseWriter, r *http.Request) {
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), mustPrincipal(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.recoveryGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) importPlatformRecoveryDrill(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "recovery_drill_import_forbidden", "Platform Owner or Admin authority is required to import a Recovery drill."))
		return
	}
	var input recoverygovernance.ImportInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.recoveryGovernance.Import(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) recordPlatformRecoveryApproval(w http.ResponseWriter, r *http.Request) {
	principal := mustPrincipal(r)
	if _, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return
	}
	recordID, ok := s.pathUUID(w, r, "recoveryDrillRecordID")
	if !ok {
		return
	}
	var input recoverygovernance.ApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.recoveryGovernance.RecordApproval(r.Context(), principal.UserID, recordID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

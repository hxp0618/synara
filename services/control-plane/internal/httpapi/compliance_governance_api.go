package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/compliancegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listPlatformCompliancePrograms(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authorizeComplianceGovernance(w, r, false); !ok {
		return
	}
	items, err := s.complianceGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createPlatformComplianceProgram(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeComplianceGovernance(w, r, true)
	if !ok {
		return
	}
	var input compliancegovernance.CreateProgramInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.complianceGovernance.CreateProgram(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) createPlatformComplianceControl(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeComplianceGovernance(w, r, true)
	if !ok {
		return
	}
	programID, ok := s.pathUUID(w, r, "programID")
	if !ok {
		return
	}
	var input compliancegovernance.CreateControlInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.complianceGovernance.CreateControl(r.Context(), principal.UserID, programID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) submitPlatformComplianceEvidence(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeComplianceGovernance(w, r, false)
	if !ok {
		return
	}
	programID, ok := s.pathUUID(w, r, "programID")
	if !ok {
		return
	}
	var input compliancegovernance.SubmitEvidenceInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.complianceGovernance.SubmitEvidence(r.Context(), principal.UserID, programID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) reviewPlatformComplianceEvidence(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeComplianceGovernance(w, r, false)
	if !ok {
		return
	}
	programID, ok := s.pathUUID(w, r, "programID")
	if !ok {
		return
	}
	evidenceRecordID, ok := s.pathUUID(w, r, "evidenceRecordID")
	if !ok {
		return
	}
	var input compliancegovernance.ReviewEvidenceInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.complianceGovernance.ReviewEvidence(r.Context(), principal.UserID, programID, evidenceRecordID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) recordPlatformComplianceDecision(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeComplianceGovernance(w, r, false)
	if !ok {
		return
	}
	programID, ok := s.pathUUID(w, r, "programID")
	if !ok {
		return
	}
	var input compliancegovernance.DecisionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.complianceGovernance.RecordDecision(r.Context(), principal.UserID, programID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) transitionPlatformComplianceProgram(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeComplianceGovernance(w, r, true)
	if !ok {
		return
	}
	programID, ok := s.pathUUID(w, r, "programID")
	if !ok {
		return
	}
	var input compliancegovernance.TransitionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.complianceGovernance.Transition(r.Context(), principal.UserID, programID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) authorizeComplianceGovernance(w http.ResponseWriter, r *http.Request, manage bool) (identity.Principal, string, bool) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return identity.Principal{}, "", false
	}
	if manage && role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(http.StatusForbidden, "compliance_governance_manage_forbidden", "Platform Owner or Admin authority is required to manage compliance programs."))
		return identity.Principal{}, "", false
	}
	return principal, role, true
}

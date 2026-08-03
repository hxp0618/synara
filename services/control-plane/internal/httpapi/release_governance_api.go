package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
)

func (s *Server) listPlatformReleaseCandidates(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authorizeReleaseGovernance(w, r, false); !ok {
		return
	}
	items, err := s.releaseGovernance.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getPlatformReleaseCandidateReadiness(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authorizeReleaseGovernance(w, r, false); !ok {
		return
	}
	candidateRecordID, ok := s.pathUUID(w, r, "candidateRecordID")
	if !ok {
		return
	}
	readiness, err := s.releaseGovernance.GetReadiness(r.Context(), candidateRecordID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, readiness)
}

func (s *Server) createPlatformReleaseCandidate(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeReleaseGovernance(w, r, true)
	if !ok {
		return
	}
	var input releasegovernance.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	candidate, err := s.releaseGovernance.Create(r.Context(), principal.UserID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, candidate)
}

func (s *Server) recordPlatformReleaseApproval(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeReleaseGovernance(w, r, false)
	if !ok {
		return
	}
	candidateRecordID, ok := s.pathUUID(w, r, "candidateRecordID")
	if !ok {
		return
	}
	var input releasegovernance.ApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	candidate, err := s.releaseGovernance.RecordApproval(
		r.Context(), principal.UserID, candidateRecordID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, candidate)
}

func (s *Server) transitionPlatformReleaseCandidate(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeReleaseGovernance(w, r, true)
	if !ok {
		return
	}
	candidateRecordID, ok := s.pathUUID(w, r, "candidateRecordID")
	if !ok {
		return
	}
	var input releasegovernance.TransitionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	candidate, err := s.releaseGovernance.Transition(
		r.Context(), principal.UserID, candidateRecordID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, candidate)
}

func (s *Server) recordPlatformReleaseFinalReview(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := s.authorizeReleaseGovernance(w, r, true)
	if !ok {
		return
	}
	candidateRecordID, ok := s.pathUUID(w, r, "candidateRecordID")
	if !ok {
		return
	}
	var input releasegovernance.FinalReviewInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	candidate, err := s.releaseGovernance.RecordFinalReview(
		r.Context(), principal.UserID, candidateRecordID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, candidate)
}

func (s *Server) authorizeReleaseGovernance(
	w http.ResponseWriter,
	r *http.Request,
	manage bool,
) (identity.Principal, string, bool) {
	principal := mustPrincipal(r)
	role, err := s.supportAccess.RequirePlatformOperator(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return identity.Principal{}, "", false
	}
	if manage && role != "owner" && role != "admin" {
		s.writeError(w, r, problem.New(
			http.StatusForbidden,
			"release_governance_manage_forbidden",
			"Platform Owner or Admin authority is required to manage release candidates.",
		))
		return identity.Principal{}, "", false
	}
	return principal, role, true
}

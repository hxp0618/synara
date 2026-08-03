package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/privacy"
)

func (s *Server) listPrivacyRequests(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.privacy.List(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getPrivacyRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, privacyRequestID, ok := s.privacyPathIDs(w, r)
	if !ok {
		return
	}
	result, err := s.privacy.Get(r.Context(), mustPrincipal(r), tenantID, privacyRequestID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createPrivacyRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input privacy.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.privacy.Create(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) transitionPrivacyRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, privacyRequestID, ok := s.privacyPathIDs(w, r)
	if !ok {
		return
	}
	var input privacy.TransitionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.privacy.Transition(
		r.Context(), mustPrincipal(r), tenantID, privacyRequestID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) executePrivacyExport(w http.ResponseWriter, r *http.Request) {
	tenantID, privacyRequestID, input, ok := s.privacyExecuteInput(w, r)
	if !ok {
		return
	}
	result, err := s.privacy.ExecuteExport(
		r.Context(), mustPrincipal(r), tenantID, privacyRequestID, input.ExpectedVersion,
		requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) executePrivacyErasure(w http.ResponseWriter, r *http.Request) {
	tenantID, privacyRequestID, input, ok := s.privacyExecuteInput(w, r)
	if !ok {
		return
	}
	result, err := s.privacy.ExecuteErasure(
		r.Context(), mustPrincipal(r), tenantID, privacyRequestID, input.ExpectedVersion,
		requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) executeTenantDataExport(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	result, err := s.privacy.ExecuteTenantExport(
		r.Context(), mustPrincipal(r), tenantID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) privacyPathIDs(
	w http.ResponseWriter,
	r *http.Request,
) (uuid.UUID, uuid.UUID, bool) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	privacyRequestID, ok := s.pathUUID(w, r, "privacyRequestID")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, privacyRequestID, true
}

func (s *Server) privacyExecuteInput(
	w http.ResponseWriter,
	r *http.Request,
) (uuid.UUID, uuid.UUID, privacy.ExecuteInput, bool) {
	tenantID, privacyRequestID, ok := s.privacyPathIDs(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, privacy.ExecuteInput{}, false
	}
	var input privacy.ExecuteInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return uuid.Nil, uuid.Nil, privacy.ExecuteInput{}, false
	}
	return tenantID, privacyRequestID, input, true
}

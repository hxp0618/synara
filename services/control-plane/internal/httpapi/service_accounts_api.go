package httpapi

import (
	"net/http"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
)

func (s *Server) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.serviceAccounts.List(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input serviceaccounts.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	issued, err := s.serviceAccounts.Create(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (s *Server) rotateServiceAccountToken(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	serviceAccountID, ok := s.pathUUID(w, r, "serviceAccountID")
	if !ok {
		return
	}
	var input struct {
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	issued, err := s.serviceAccounts.RotateToken(r.Context(), mustPrincipal(r), tenantID, serviceAccountID, input.ExpiresAt, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, issued)
}

func (s *Server) revokeServiceAccount(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	serviceAccountID, ok := s.pathUUID(w, r, "serviceAccountID")
	if !ok {
		return
	}
	if err := s.serviceAccounts.Revoke(r.Context(), mustPrincipal(r), tenantID, serviceAccountID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

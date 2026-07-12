package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/quotas"
)

func (s *Server) getTenantQuota(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	item, err := s.quotas.Get(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) putTenantQuota(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input quotas.PutInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.quotas.Put(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

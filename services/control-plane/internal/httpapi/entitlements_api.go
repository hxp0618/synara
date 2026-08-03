package httpapi

import "net/http"

func (s *Server) getTenantEntitlements(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	snapshot, err := s.entitlements.Get(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entitlementProfileView(snapshot))
}

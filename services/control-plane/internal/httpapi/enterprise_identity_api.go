package httpapi

import (
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/enterpriseidentity"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listPublicIdentityConnections(w http.ResponseWriter, r *http.Request) {
	items, err := s.enterpriseIdentity.ListPublic(r.Context(), r.URL.Query().Get("tenantSlug"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) startSSO(w http.ResponseWriter, r *http.Request) {
	connectionID, ok := s.pathUUID(w, r, "connectionID")
	if !ok {
		return
	}
	callbackURL, err := s.ssoCallbackURL(r, connectionID.String())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.enterpriseIdentity.Start(r.Context(), connectionID, callbackURL, r.URL.Query().Get("returnTo"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) completeSSO(w http.ResponseWriter, r *http.Request) {
	connectionID, ok := s.pathUUID(w, r, "connectionID")
	if !ok {
		return
	}
	callbackURL, err := s.ssoCallbackURL(r, connectionID.String())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var result enterpriseidentity.CallbackResult
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			s.writeError(w, r, problem.New(400, "saml_callback_invalid", "SAML callback form is invalid."))
			return
		}
		result, err = s.enterpriseIdentity.CompleteSAML(r.Context(), connectionID, r.Form.Get("RelayState"), callbackURL, r, clientIP(r), r.UserAgent(), requestID(r))
	} else {
		result, err = s.enterpriseIdentity.CompleteOIDC(r.Context(), connectionID, r.URL.Query().Get("state"), r.URL.Query().Get("code"), callbackURL, clientIP(r), r.UserAgent(), requestID(r))
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, result.Session.Token)
	http.Redirect(w, r, result.ReturnTo, http.StatusSeeOther)
}

func (s *Server) samlMetadata(w http.ResponseWriter, r *http.Request) {
	connectionID, ok := s.pathUUID(w, r, "connectionID")
	if !ok {
		return
	}
	callbackURL, err := s.ssoCallbackURL(r, connectionID.String())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	metadata, err := s.enterpriseIdentity.SAMLMetadata(r.Context(), connectionID, callbackURL)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(metadata)
}

func (s *Server) listIdentityConnections(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.enterpriseIdentity.List(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createIdentityConnection(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input enterpriseidentity.CreateConnectionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.enterpriseIdentity.Create(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) disableIdentityConnection(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	connectionID, ok := s.pathUUID(w, r, "connectionID")
	if !ok {
		return
	}
	if err := s.enterpriseIdentity.Disable(r.Context(), mustPrincipal(r), tenantID, connectionID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIdentityGroupMappings(w http.ResponseWriter, r *http.Request) {
	tenantID, connectionID, ok := s.identityConnectionPath(w, r)
	if !ok {
		return
	}
	items, err := s.enterpriseIdentity.ListMappings(r.Context(), mustPrincipal(r), tenantID, connectionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) replaceIdentityGroupMappings(w http.ResponseWriter, r *http.Request) {
	tenantID, connectionID, ok := s.identityConnectionPath(w, r)
	if !ok {
		return
	}
	var input struct {
		Items []enterpriseidentity.MappingInput `json:"items"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.enterpriseIdentity.ReplaceMappings(r.Context(), mustPrincipal(r), tenantID, connectionID, input.Items, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) identityConnectionPath(w http.ResponseWriter, r *http.Request) (tenantID, connectionID uuid.UUID, ok bool) {
	tenant, parsed := s.pathUUID(w, r, "tenantID")
	if !parsed {
		return tenantID, connectionID, false
	}
	connection, parsed := s.pathUUID(w, r, "connectionID")
	if !parsed {
		return tenantID, connectionID, false
	}
	return tenant, connection, true
}

func (s *Server) ssoCallbackURL(r *http.Request, connectionID string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.config.PublicControlPlaneURL), "/")
	if base == "" {
		remote, ok := directRemoteIP(r.RemoteAddr)
		if !ok || !remote.IsLoopback() {
			return "", problem.New(503, "public_control_plane_url_required", "Public control-plane URL is required outside loopback.")
		}
		hostURL, parseErr := url.Parse("//" + r.Host)
		if parseErr != nil || !isLoopbackRequestHost(hostURL.Hostname()) {
			return "", problem.New(503, "public_control_plane_url_required", "Public control-plane URL is required outside loopback.")
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", problem.New(503, "public_control_plane_url_invalid", "Public control-plane URL is invalid.")
	}
	return strings.TrimRight(base, "/") + "/v1/auth/sso/" + connectionID + "/callback", nil
}

func isLoopbackRequestHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

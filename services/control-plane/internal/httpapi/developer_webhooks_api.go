package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/developerwebhooks"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) requireDeveloperWebhooks() error {
	if s.developerWebhooks == nil {
		return problem.New(http.StatusServiceUnavailable, "developer_webhooks_unavailable", "Developer Webhooks are unavailable.")
	}
	return nil
}

func (s *Server) listDeveloperWebhooks(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDeveloperWebhooks(); err != nil {
		s.writeError(w, r, err)
		return
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.developerWebhooks.List(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createDeveloperWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDeveloperWebhooks(); err != nil {
		s.writeError(w, r, err)
		return
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input developerwebhooks.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	issued, err := s.developerWebhooks.Create(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (s *Server) rotateDeveloperWebhookSecret(w http.ResponseWriter, r *http.Request) {
	tenantID, webhookID, ok := s.developerWebhookPath(w, r)
	if !ok {
		return
	}
	issued, err := s.developerWebhooks.RotateSecret(r.Context(), mustPrincipal(r), tenantID, webhookID, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, issued)
}

func (s *Server) enableDeveloperWebhook(w http.ResponseWriter, r *http.Request) {
	s.setDeveloperWebhookEnabled(w, r, true)
}

func (s *Server) disableDeveloperWebhook(w http.ResponseWriter, r *http.Request) {
	s.setDeveloperWebhookEnabled(w, r, false)
}

func (s *Server) setDeveloperWebhookEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	tenantID, webhookID, ok := s.developerWebhookPath(w, r)
	if !ok {
		return
	}
	endpoint, err := s.developerWebhooks.SetEnabled(r.Context(), mustPrincipal(r), tenantID, webhookID, enabled, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, endpoint)
}

func (s *Server) revokeDeveloperWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, webhookID, ok := s.developerWebhookPath(w, r)
	if !ok {
		return
	}
	endpoint, err := s.developerWebhooks.Revoke(r.Context(), mustPrincipal(r), tenantID, webhookID, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, endpoint)
}

func (s *Server) listDeveloperWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	tenantID, webhookID, ok := s.developerWebhookPath(w, r)
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.developerWebhooks.ListDeliveries(r.Context(), mustPrincipal(r), tenantID, webhookID, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) replayDeveloperWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	tenantID, webhookID, ok := s.developerWebhookPath(w, r)
	if !ok {
		return
	}
	deliveryID, ok := s.pathUUID(w, r, "deliveryID")
	if !ok {
		return
	}
	if s.outbox == nil {
		s.writeError(w, r, problem.New(http.StatusServiceUnavailable, "outbox_unavailable", "Outbox replay is unavailable."))
		return
	}
	principal := mustPrincipal(r)
	if err := s.developerWebhooks.AuthorizeDeliveryReplay(r.Context(), principal, tenantID, webhookID, deliveryID); err != nil {
		s.writeError(w, r, err)
		return
	}
	message, err := s.outbox.ReplayAuthorized(r.Context(), principal, tenantID, deliveryID, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func (s *Server) developerWebhookPath(w http.ResponseWriter, r *http.Request) (tenantID, webhookID uuid.UUID, ok bool) {
	if err := s.requireDeveloperWebhooks(); err != nil {
		s.writeError(w, r, err)
		return tenantID, webhookID, false
	}
	tenantID, ok = s.pathUUID(w, r, "tenantID")
	if !ok {
		return tenantID, webhookID, false
	}
	webhookID, ok = s.pathUUID(w, r, "webhookID")
	return tenantID, webhookID, ok
}

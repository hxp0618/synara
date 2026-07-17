package httpapi

import (
	"net/http"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
)

func (s *Server) listOutboxMessages(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.outbox.ListForTenant(
		r.Context(),
		mustPrincipal(r),
		tenantID,
		outbox.ListQuery{
			Status:      strings.TrimSpace(r.URL.Query().Get("status")),
			TopicPrefix: strings.TrimSpace(r.URL.Query().Get("topicPrefix")),
			Limit:       limit,
		},
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) replayOutboxMessage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	messageID, ok := s.pathUUID(w, r, "messageID")
	if !ok {
		return
	}
	message, err := s.outbox.ReplayAuthorized(
		r.Context(), mustPrincipal(r), tenantID, messageID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

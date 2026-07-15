package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func (s *Server) compactSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input executions.CompactSessionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.RequestCompact(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, result.Value)
}

func (s *Server) startSessionReview(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input executions.StartReviewInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.RequestReview(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, result.Value)
}

func (s *Server) rollbackSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input sessions.RollbackSessionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, replayed, err := s.sessions.Rollback(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) forkSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input sessions.ForkSessionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, replayed, err := s.sessions.Fork(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusCreated, result)
}

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	sessionEventPollInterval = 2 * time.Second
	sessionEventHeartbeat    = 15 * time.Second
)

func (s *Server) streamSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	afterSequence, err := sessionEventCursor(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		s.writeError(w, r, problem.New(500, "streaming_unsupported", "The server does not support event streaming."))
		return
	}
	principal := mustPrincipal(r)
	tenantID, notifications, unsubscribe, err := s.sessions.SubscribeEvents(r.Context(), principal, sessionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer unsubscribe()

	var leaseID uuid.UUID
	if s.eventStreams != nil {
		lease, err := s.eventStreams.Acquire(r.Context(), tenantID, principal.UserID, sessionID)
		if err != nil {
			var apiError *problem.Error
			if errors.As(err, &apiError) && apiError.Status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "2")
				if s.metrics != nil {
					scope := "other"
					if apiError.Code == "sse_user_connection_limit" {
						scope = "user"
					} else if apiError.Code == "sse_tenant_connection_limit" {
						scope = "tenant"
					}
					s.metrics.ObserveSSELimit(scope)
				}
			}
			s.writeError(w, r, err)
			return
		}
		leaseID = lease.ID
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := s.eventStreams.Release(releaseCtx, lease.ID); err != nil {
				s.logger.Warn("session event stream lease release failed", "requestId", requestID(r), "sessionId", sessionID, "error", err)
			}
		}()
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := s.writeSSE(w, func() error {
		_, err := fmt.Fprint(w, "retry: 2000\n\n")
		return err
	}); err != nil {
		return
	}

	cursor := afterSequence
	flushBacklog := func() (catchupErr error) {
		started := time.Now()
		delivered := 0
		defer func() {
			if s.metrics != nil {
				s.metrics.ObserveSSECatchup(time.Since(started), delivered, catchupErr)
			}
		}()
		for {
			page, err := s.sessions.ListEvents(r.Context(), principal, sessionID, cursor, 500)
			if err != nil {
				return err
			}
			if len(page.Items) > 0 {
				if err := s.writeSSE(w, func() error {
					for _, event := range page.Items {
						if err := writeSessionEvent(w, event); err != nil {
							return err
						}
						cursor = event.Sequence
						delivered++
					}
					return nil
				}); err != nil {
					return err
				}
			}
			if len(page.Items) < 500 {
				return nil
			}
		}
	}
	if err := flushBacklog(); err != nil {
		s.logger.Warn("session event stream backlog failed", "requestId", requestID(r), "sessionId", sessionID, "error", err)
		return
	}

	pollInterval := s.sessionEventPoll
	if pollInterval <= 0 {
		pollInterval = sessionEventPollInterval
	}
	heartbeatInterval := s.sessionEventBeat
	if heartbeatInterval <= 0 {
		heartbeatInterval = sessionEventHeartbeat
	}
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-notifications:
			if !open {
				return
			}
			if event.Sequence <= cursor {
				continue
			}
			if event.Sequence != cursor+1 {
				if err := flushBacklog(); err != nil {
					return
				}
				continue
			}
			if err := s.writeSSE(w, func() error { return writeSessionEvent(w, event) }); err != nil {
				return
			}
			cursor = event.Sequence
		case <-poll.C:
			if err := flushBacklog(); err != nil {
				return
			}
		case <-heartbeat.C:
			if s.eventStreams != nil && leaseID != uuid.Nil {
				if _, err := s.eventStreams.Renew(r.Context(), leaseID); err != nil {
					s.logger.Warn("session event stream lease renewal failed", "requestId", requestID(r), "sessionId", sessionID, "error", err)
					return
				}
			}
			if err := s.writeSSE(w, func() error {
				_, err := fmt.Fprint(w, ": keep-alive\n\n")
				return err
			}); err != nil {
				return
			}
		}
	}
}

func (s *Server) writeSSE(w http.ResponseWriter, write func() error) error {
	controller := http.NewResponseController(w)
	timeout := s.sessionEventWrite
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	if err := write(); err != nil {
		return err
	}
	return controller.Flush()
}

func sessionEventCursor(r *http.Request) (int64, error) {
	if r.URL.Query().Has("afterSequence") {
		value, err := queryInt64(r, "afterSequence", 0)
		if err != nil || value < 0 {
			return 0, problem.New(400, "invalid_event_sequence", "afterSequence must be a non-negative integer.")
		}
		return value, nil
	}
	lastEventID := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if lastEventID == "" {
		return 0, nil
	}
	request := r.Clone(r.Context())
	query := request.URL.Query()
	query.Set("lastEventId", lastEventID)
	request.URL.RawQuery = query.Encode()
	value, err := queryInt64(request, "lastEventId", 0)
	if err != nil || value < 0 {
		return 0, problem.New(400, "invalid_event_sequence", "Last-Event-ID must be a non-negative integer.")
	}
	return value, nil
}

func writeSessionEvent(w http.ResponseWriter, event sessions.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: session-event\ndata: %s\n\n", event.Sequence, payload)
	return err
}

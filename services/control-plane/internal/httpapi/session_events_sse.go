package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	_, notifications, unsubscribe, err := s.sessions.SubscribeEvents(r.Context(), mustPrincipal(r), sessionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer unsubscribe()

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, problem.New(500, "streaming_unsupported", "The server does not support event streaming."))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	cursor := afterSequence
	flushBacklog := func() error {
		for {
			page, err := s.sessions.ListEvents(r.Context(), mustPrincipal(r), sessionID, cursor, 500)
			if err != nil {
				return err
			}
			for _, event := range page.Items {
				if err := writeSessionEvent(w, event); err != nil {
					return err
				}
				cursor = event.Sequence
			}
			if len(page.Items) < 500 {
				flusher.Flush()
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
			if err := writeSessionEvent(w, event); err != nil {
				return
			}
			cursor = event.Sequence
			flusher.Flush()
		case <-poll.C:
			if err := flushBacklog(); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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

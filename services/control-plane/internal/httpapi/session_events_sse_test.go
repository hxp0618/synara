package httpapi

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/eventstream"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSessionEventCursorPrefersExplicitSequence(t *testing.T) {
	request := httptest.NewRequest("GET", "/events?afterSequence=7", nil)
	request.Header.Set("Last-Event-ID", "4")
	value, err := sessionEventCursor(request)
	if err != nil {
		t.Fatal(err)
	}
	if value != 7 {
		t.Fatalf("cursor = %d, want 7", value)
	}
}

func TestSessionEventCursorRejectsInvalidLastEventID(t *testing.T) {
	request := httptest.NewRequest("GET", "/events", nil)
	request.Header.Set("Last-Event-ID", "invalid")
	if _, err := sessionEventCursor(request); err == nil {
		t.Fatal("invalid Last-Event-ID was accepted")
	}
}

func TestWriteSessionEventUsesSequenceAsSSEID(t *testing.T) {
	recorder := httptest.NewRecorder()
	event := sessions.Event{
		EventID: uuid.New(), EventVersion: 1, TenantID: uuid.New(), OrganizationID: uuid.New(),
		ProjectID: uuid.New(), SessionID: uuid.New(), Sequence: 9, EventType: "turn.created",
		ActorType: "user", Payload: map[string]any{"status": "queued"},
	}
	if err := writeSessionEvent(recorder, event); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "id: 9\nevent: session-event\ndata: {") {
		t.Fatalf("unexpected SSE body: %s", body)
	}
}

func TestSessionEventStreamCatchesUpAcrossServiceInstances(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sse-cross-replica-test")
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: domain.UserID, SessionID: uuid.New(), ActiveTenantID: &domain.TenantID,
		Email: "local-owner@localhost.invalid", DisplayName: "Local Owner",
	}
	projectService := projects.NewService(store.DB())
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	project, err := projectService.Create(ctx, principal, domain.TenantID, domain.OrganizationID, projects.CreateProjectInput{
		Name: "Cross Replica SSE", DefaultBranch: "main", Visibility: "organization",
	}, "sse-project", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	firstReplica := sessions.NewService(store.DB(), projectService, targetService)
	secondReplica := sessions.NewService(store.DB(), projectService, targetService)
	session, err := firstReplica.Create(ctx, principal, project.ID, sessions.CreateSessionInput{
		Title: "Cross Replica SSE", Visibility: "project", Provider: "codex",
	}, "sse-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		sessions: firstReplica, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionEventPoll: 20 * time.Millisecond, sessionEventBeat: time.Second,
		metrics: observability.New(store.DB()),
	}
	server.eventStreams, err = eventstream.New(store.DB(), eventstream.Config{
		InstanceID: "sse-http-test", LeaseTTL: 5 * time.Second,
		MaxConnectionsPerUser: 1, MaxConnectionsPerTenant: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{sessionID}/events/stream", func(w http.ResponseWriter, r *http.Request) {
		server.streamSessionEvents(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	response, err := http.Get(httpServer.URL + "/v1/sessions/" + session.ID.String() + "/events/stream?afterSequence=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	lines := make(chan string, 64)
	scanDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanDone <- scanner.Err()
	}()
	waitForSSELine(t, lines, scanDone, "retry: 2000")
	limitedResponse, err := http.Get(httpServer.URL + "/v1/sessions/" + session.ID.String() + "/events/stream?afterSequence=1")
	if err != nil {
		t.Fatal(err)
	}
	limitedBody, _ := io.ReadAll(limitedResponse.Body)
	_ = limitedResponse.Body.Close()
	if limitedResponse.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(limitedBody), "sse_user_connection_limit") {
		t.Fatalf("connection limit response = %d %s", limitedResponse.StatusCode, limitedBody)
	}

	if _, err := secondReplica.CreateTurn(ctx, principal, session.ID, sessions.CreateTurnInput{
		InputText: "created through another replica",
	}, "sse-turn", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	waitForSSELine(t, lines, scanDone, "id: 2")
}

func TestSessionEventStreamRechecksInteractionAccessAfterRoleDowngrade(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sse-role-downgrade-test")
	if err != nil {
		t.Fatal(err)
	}
	owner := identity.Principal{
		UserID: domain.UserID, SessionID: uuid.New(), ActiveTenantID: &domain.TenantID,
		Email: "local-owner@localhost.invalid", DisplayName: "Local Owner",
	}
	projectService := projects.NewService(store.DB())
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	project, err := projectService.Create(ctx, owner, domain.TenantID, domain.OrganizationID, projects.CreateProjectInput{
		Name: "SSE Role Downgrade", DefaultBranch: "main", Visibility: "organization",
	}, "sse-role-project", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	session, err := sessionService.Create(ctx, owner, project.ID, sessions.CreateSessionInput{
		Title: "SSE Role Downgrade", Visibility: "organization", Provider: "codex",
	}, "sse-role-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	viewerID := uuid.New()
	now := time.Now().UTC()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: viewerID, Email: uuid.NewString() + "@example.com", DisplayName: "SSE viewer",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: domain.TenantID, UserID: viewerID, Role: "member", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.OrganizationMembership{
				TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, UserID: viewerID,
				Role: "agent_operator", Status: "active", CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	viewer := identity.Principal{UserID: viewerID, SessionID: uuid.New(), ActiveTenantID: &domain.TenantID}

	server := &Server{
		sessions: sessionService, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionEventPoll: 10 * time.Second, sessionEventBeat: time.Second,
		metrics: observability.New(store.DB()),
	}
	server.eventStreams, err = eventstream.New(store.DB(), eventstream.Config{
		InstanceID: "sse-role-http-test", LeaseTTL: 5 * time.Second,
		MaxConnectionsPerUser: 1, MaxConnectionsPerTenant: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{sessionID}/events/stream", func(w http.ResponseWriter, r *http.Request) {
		server.streamSessionEvents(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, viewer)))
	})
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	response, err := http.Get(httpServer.URL + "/v1/sessions/" + session.ID.String() + "/events/stream?afterSequence=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	lines := make(chan string, 64)
	scanDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanDone <- scanner.Err()
	}()
	waitForSSELine(t, lines, scanDone, "retry: 2000")

	appendInteractionEvent := func(requestID, detail string) persistence.SessionEvent {
		t.Helper()
		var event persistence.SessionEvent
		if err := store.DB().Transaction(func(tx *gorm.DB) error {
			var appendErr error
			event, appendErr = sessionService.AppendInternalEvent(ctx, tx, domain.TenantID, session.ID, sessions.InternalEventInput{
				EventVersion: 2, EventType: "request.opened", ActorType: "worker",
				Payload: map[string]any{
					"requestId": requestID, "requestType": "exec_command_approval", "detail": detail,
				},
			})
			return appendErr
		}); err != nil {
			t.Fatal(err)
		}
		sessionService.PublishInternalEvent(event)
		return event
	}

	visible := appendInteractionEvent("approval-visible", "visible command")
	waitForSSELine(t, lines, scanDone, "id: "+strconv.FormatInt(visible.Sequence, 10))
	visibleData := waitForSSEDataLine(t, lines, scanDone)
	if !strings.Contains(visibleData, "visible command") || strings.Contains(visibleData, "session.event.redacted") {
		t.Fatalf("approver did not receive visible interaction details: %s", visibleData)
	}

	if err := store.DB().Model(&persistence.OrganizationMembership{}).
		Where("tenant_id = ? AND organization_id = ? AND user_id = ?", domain.TenantID, domain.OrganizationID, viewerID).
		Update("role", "viewer").Error; err != nil {
		t.Fatal(err)
	}
	redacted := appendInteractionEvent("approval-redacted", "secret command")
	waitForSSELine(t, lines, scanDone, "id: "+strconv.FormatInt(redacted.Sequence, 10))
	redactedData := waitForSSEDataLine(t, lines, scanDone)
	if !strings.Contains(redactedData, "session.event.redacted") || strings.Contains(redactedData, "secret command") || strings.Contains(redactedData, "approval-redacted") {
		t.Fatalf("downgraded viewer received interaction details: %s", redactedData)
	}
}

func TestSSEWriteAppliesAndClearsDeadline(t *testing.T) {
	writer := &deadlineResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	server := &Server{sessionEventWrite: 250 * time.Millisecond}
	if err := server.writeSSE(writer, func() error {
		_, err := io.WriteString(writer, ": test\n\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(writer.deadlines) < 2 || writer.deadlines[0].IsZero() || !writer.deadlines[len(writer.deadlines)-1].IsZero() {
		t.Fatalf("write deadlines were not applied and cleared: %#v", writer.deadlines)
	}
}

type deadlineResponseWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineResponseWriter) Flush() {}

func waitForSSELine(t *testing.T, lines <-chan string, scanDone <-chan error, expected string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			if line == expected {
				return
			}
		case err := <-scanDone:
			t.Fatalf("SSE stream ended before %q: %v", expected, err)
		case <-timer.C:
			t.Fatalf("timed out waiting for SSE line %q", expected)
		}
	}
}

func waitForSSEDataLine(t *testing.T, lines <-chan string, scanDone <-chan error) string {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data: ") {
				return line
			}
		case err := <-scanDone:
			t.Fatalf("SSE stream ended before data: %v", err)
		case <-timer.C:
			t.Fatal("timed out waiting for SSE data")
		}
	}
}

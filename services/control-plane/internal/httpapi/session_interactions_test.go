package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestListPendingSessionInteractionsRouteAuthorizesAndPreservesSnapshotShape(t *testing.T) {
	fixture := newSessionInteractionHTTPFixture(t)

	recorder := fixture.request(t, fixture.approverToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, envelope, "items", "snapshotSequence")

	var snapshot executions.PendingInteractionSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SnapshotSequence != fixture.snapshotSequence {
		t.Fatalf("snapshotSequence = %d, want %d", snapshot.SnapshotSequence, fixture.snapshotSequence)
	}
	if len(snapshot.Items) != 1 {
		t.Fatalf("items = %#v, want one pending interaction", snapshot.Items)
	}
	item := snapshot.Items[0]
	if item.ID != fixture.interaction.ID || item.ExecutionID != fixture.interaction.ExecutionID ||
		item.TurnID != fixture.interaction.TurnID || item.Provider != "codex" ||
		item.RequestID != "approval-route-test" || item.Kind != "approval" {
		t.Fatalf("unexpected pending interaction: %#v", item)
	}
	if item.Payload["command"] != "git status" || item.Payload["reason"] != "inspect the worktree" {
		t.Fatalf("unexpected pending interaction payload: %#v", item.Payload)
	}
	if !item.RequestedAt.Equal(fixture.interaction.RequestedAt) || !item.ExpiresAt.Equal(fixture.interaction.ExpiresAt) {
		t.Fatalf("unexpected interaction timestamps: %#v", item)
	}

	var rawItems []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["items"], &rawItems); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, rawItems[0],
		"id", "executionId", "turnId", "provider", "requestId", "kind", "payload", "requestedAt", "expiresAt",
	)
}

func TestListPendingSessionInteractionsRouteRejectsReadersWithoutApprovalPermission(t *testing.T) {
	fixture := newSessionInteractionHTTPFixture(t)

	tests := []struct {
		name  string
		token string
	}{
		{name: "viewer", token: fixture.viewerToken},
		{name: "member", token: fixture.memberToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := fixture.request(t, test.token)
			assertProblemResponse(t, recorder, http.StatusForbidden, "organization_forbidden")
		})
	}
}

func TestListPendingSessionInteractionsRouteDoesNotRevealInvisibleSessions(t *testing.T) {
	fixture := newSessionInteractionHTTPFixture(t)

	tests := []struct {
		name     string
		token    string
		wantCode string
	}{
		{name: "same tenant without organization membership", token: fixture.nonMemberToken, wantCode: "organization_not_found"},
		{name: "different active tenant", token: fixture.crossTenantToken, wantCode: "session_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := fixture.request(t, test.token)
			assertProblemResponse(t, recorder, http.StatusNotFound, test.wantCode)
		})
	}
}

func TestListPendingSessionInteractionsRouteRequiresAuthentication(t *testing.T) {
	fixture := newSessionInteractionHTTPFixture(t)
	assertProblemResponse(t, fixture.request(t, ""), http.StatusUnauthorized, "authentication_required")
}

type sessionInteractionHTTPFixture struct {
	handler          http.Handler
	cookieName       string
	sessionID        uuid.UUID
	snapshotSequence int64
	interaction      persistence.ExecutionInteraction
	approverToken    string
	viewerToken      string
	memberToken      string
	nonMemberToken   string
	crossTenantToken string
}

func newSessionInteractionHTTPFixture(t *testing.T) sessionInteractionHTTPFixture {
	t.Helper()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "session-interactions-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Platform: profile, CookieName: "synara_test_session", CookiePath: "/",
		SessionTTL: time.Hour, SessionIdleTTL: time.Hour,
		SSEPollInterval: 20 * time.Millisecond, SSEHeartbeatInterval: time.Second,
		SSEWriteTimeout: time.Second, SSELeaseTTL: time.Minute,
		SSEMaxConnectionsPerUser: 1, SSEMaxConnectionsPerTenant: 10,
	}
	identityService := identity.NewService(store.DB(), cfg.SessionTTL, cfg.SessionIdleTTL)
	projectService := projects.NewService(store.DB())
	targetService := executiontargets.NewService(store.DB(), profile, nil)
	sessionService := sessions.NewService(store.DB(), projectService, targetService)
	executionService := executions.NewService(
		store.DB(), sessionService, time.Minute, 2*time.Minute, time.Hour, nil, targetService,
	)
	owner := identity.Principal{
		UserID: domain.UserID, SessionID: uuid.New(), ActiveTenantID: &domain.TenantID,
		Email: "local-owner@localhost.invalid", DisplayName: "Local Owner",
	}
	project, err := projectService.Create(ctx, owner, domain.TenantID, domain.OrganizationID, projects.CreateProjectInput{
		Name: "Interaction HTTP authorization", DefaultBranch: "main", Visibility: "organization",
	}, "interaction-http-project", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessionService.Create(ctx, owner, project.ID, sessions.CreateSessionInput{
		Title: "Interaction HTTP authorization", Visibility: "organization", Provider: "codex",
	}, "interaction-http-session", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := sessionService.CreateTurn(ctx, owner, session.ID, sessions.CreateTurnInput{
		InputText: "exercise pending interaction authorization",
	}, "interaction-http-turn", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := store.DB().Where("tenant_id = ? AND turn_id = ?", domain.TenantID, turn.ID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	registered, err := executionService.Register(ctx, executions.RegisterWorkerInput{
		ExecutionTargetID: session.ExecutionTargetID, TargetKind: "local", InstanceUID: uuid.NewString(),
		ClusterID: "interaction-http", Namespace: "tests", PodName: "worker-" + uuid.NewString(),
		Version: "test", ProtocolVersion: executions.WorkerProtocolVersion,
		Capabilities: map[string]any{"providers": []any{"codex"}}, LeaseSupported: true, FencingSupported: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	interaction := persistence.ExecutionInteraction{
		ID: uuid.New(), TenantID: domain.TenantID, ExecutionID: execution.ID, SessionID: session.ID,
		TurnID: turn.ID, WorkerID: registered.Worker.ID, Generation: 1, Provider: "codex",
		RequestID: "approval-route-test", EventVersion: 2, Kind: "approval", Status: "pending",
		Payload:     map[string]any{"command": "git status", "reason": "inspect the worktree"},
		RequestedAt: now, ExpiresAt: now.Add(time.Hour), DeliveryStatus: "not-ready",
	}
	if err := store.DB().Create(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	var storedSession persistence.AgentSession
	if err := store.DB().Select("last_event_sequence").Where("id = ?", session.ID).Take(&storedSession).Error; err != nil {
		t.Fatal(err)
	}

	approverToken := createSessionInteractionHTTPUser(t, store.DB(), domain.TenantID, &domain.OrganizationID, "agent_operator")
	viewerToken := createSessionInteractionHTTPUser(t, store.DB(), domain.TenantID, &domain.OrganizationID, "viewer")
	memberToken := createSessionInteractionHTTPUser(t, store.DB(), domain.TenantID, &domain.OrganizationID, "member")
	nonMemberToken := createSessionInteractionHTTPUser(t, store.DB(), domain.TenantID, nil, "")
	crossTenantToken := createCrossTenantSessionInteractionHTTPUser(t, store.DB())

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.New(store.DB(), cfg.SessionIdleTTL)
	server, err := New(
		cfg, store.DB(), identityService, nil, projectService, sessionService, executionService, targetService,
		nil, nil, nil, nil, nil, metrics, nil, nil, nil, nil, nil, logger,
	)
	if err != nil {
		t.Fatal(err)
	}
	return sessionInteractionHTTPFixture{
		handler: server.Handler(), cookieName: cfg.CookieName, sessionID: session.ID,
		snapshotSequence: storedSession.LastEventSequence, interaction: interaction,
		approverToken: approverToken, viewerToken: viewerToken, memberToken: memberToken,
		nonMemberToken: nonMemberToken, crossTenantToken: crossTenantToken,
	}
}

func (f sessionInteractionHTTPFixture) request(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+f.sessionID.String()+"/interactions", nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func createSessionInteractionHTTPUser(
	t *testing.T,
	db *gorm.DB,
	tenantID uuid.UUID,
	organizationID *uuid.UUID,
	organizationRole string,
) string {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	models := []any{
		&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Interaction route user",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "member", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
	}
	if organizationID != nil {
		models = append(models, &persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: *organizationID, UserID: userID,
			Role: organizationRole, Status: "active", CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return createSessionInteractionHTTPLogin(t, db, userID, tenantID)
}

func createCrossTenantSessionInteractionHTTPUser(t *testing.T, db *gorm.DB) string {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	tenantID := uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Cross tenant route user",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Tenant{
				ID: tenantID, Slug: "cross-" + uuid.NewString(), Name: "Cross Tenant", Status: "active",
				PlanCode: "developer", Region: "local", Settings: map[string]any{}, CreatedBy: userID,
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
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
	return createSessionInteractionHTTPLogin(t, db, userID, tenantID)
}

func createSessionInteractionHTTPLogin(t *testing.T, db *gorm.DB, userID, tenantID uuid.UUID) string {
	t.Helper()
	token := "interaction-http-" + uuid.NewString()
	tokenHash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	if err := db.Create(&persistence.LoginSession{
		ID: uuid.New(), UserID: userID, ActiveTenantID: &tenantID, RefreshTokenHash: tokenHash[:],
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return token
}

func assertProblemResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, status, recorder.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != code {
		t.Fatalf("problem code = %q, want %q, body = %s", envelope.Error.Code, code, recorder.Body.String())
	}
}

func assertJSONKeys(t *testing.T, value map[string]json.RawMessage, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(value))
	for key := range value {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if len(actual) != len(expected) {
		t.Fatalf("JSON keys = %v, want %v", actual, expected)
	}
	for index := range actual {
		if actual[index] != expected[index] {
			t.Fatalf("JSON keys = %v, want %v", actual, expected)
		}
	}
}

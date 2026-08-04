package developerwebhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestWebhookProjectionSignsThinPayloadAndPublishes(t *testing.T) {
	fixture := newWebhookFixture(t)
	type capturedRequest struct {
		body      []byte
		timestamp string
		signature string
		messageID string
	}
	requests := make(chan capturedRequest, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- capturedRequest{
			body: body, timestamp: r.Header.Get("X-Polaris-Webhook-Timestamp"),
			signature: r.Header.Get("X-Polaris-Webhook-Signature"), messageID: r.Header.Get("X-Polaris-Webhook-Id"),
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	fixture.service.client = receiver.Client()

	issued, err := fixture.service.Create(
		context.Background(), fixture.principal, fixture.tenantID,
		CreateInput{Name: "delivery", URL: receiver.URL, EventTypes: []string{"execution.failed"}},
		"create-webhook", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Secret, SecretPrefix) {
		t.Fatalf("issued secret = %q", issued.Secret)
	}
	event := fixture.projectEvent(t, "execution.failed", map[string]any{
		"prompt": "must never leave the Control Plane", "credential": "also secret",
	})
	dispatcher := fixture.dispatcher(t)
	if count, err := dispatcher.DispatchOnce(context.Background()); err != nil || count != 1 {
		t.Fatalf("dispatch count=%d err=%v", count, err)
	}

	captured := <-requests
	if captured.messageID == "" || captured.timestamp == "" || captured.signature == "" {
		t.Fatalf("missing delivery headers: %#v", captured)
	}
	wantSignature := "v1=" + signWebhook([]byte(issued.Secret), captured.timestamp, captured.body)
	if captured.signature != wantSignature {
		t.Fatalf("signature = %q, want %q", captured.signature, wantSignature)
	}
	var body map[string]any
	if err := json.Unmarshal(captured.body, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"schemaVersion", "deliveryId", "eventId", "type", "sequence", "tenantId",
		"organizationId", "projectId", "sessionId", "executionId", "occurredAt",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("thin payload omitted %q: %#v", key, body)
		}
	}
	for _, forbidden := range []string{"payload", "prompt", "credential", "endpointId"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("thin payload leaked %q: %#v", forbidden, body)
		}
	}

	deliveries, err := fixture.service.ListDeliveries(
		context.Background(), fixture.principal, fixture.tenantID, issued.Endpoint.ID, 50,
	)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != "published" ||
		deliveries[0].SessionEventID != event.EventID {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	if err := fixture.service.AuthorizeDeliveryReplay(
		context.Background(), fixture.principal, fixture.tenantID, issued.Endpoint.ID, deliveries[0].ID,
	); err != nil {
		t.Fatalf("authorize delivery replay: %v", err)
	}
	if err := fixture.service.AuthorizeDeliveryReplay(
		context.Background(), fixture.principal, fixture.tenantID, uuid.New(), deliveries[0].ID,
	); err == nil {
		t.Fatal("delivery replay crossed its endpoint boundary")
	}
}

func TestWebhookDeliveryRetriesThenDeadLettersIndependently(t *testing.T) {
	fixture := newWebhookFixture(t)
	var mu sync.Mutex
	attempts := 0
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "do not persist this body", http.StatusInternalServerError)
	}))
	t.Cleanup(receiver.Close)
	fixture.service.client = receiver.Client()
	issued, err := fixture.service.Create(
		context.Background(), fixture.principal, fixture.tenantID,
		CreateInput{Name: "dead-letter", URL: receiver.URL, EventTypes: []string{"turn.completed"}},
		"create-dead-letter", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.projectEvent(t, "turn.completed", map[string]any{"output": "must remain internal"})
	dispatcher := fixture.dispatcher(t)
	if count, err := dispatcher.DispatchOnce(context.Background()); count != 1 || err == nil {
		t.Fatalf("first dispatch count=%d err=%v", count, err)
	}
	fixture.now = fixture.now.Add(time.Second)
	if count, err := dispatcher.DispatchOnce(context.Background()); count != 1 || err == nil {
		t.Fatalf("second dispatch count=%d err=%v", count, err)
	}

	deliveries, err := fixture.service.ListDeliveries(
		context.Background(), fixture.principal, fixture.tenantID, issued.Endpoint.ID, 50,
	)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != "dead-letter" || deliveries[0].Attempts != 2 {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("receiver attempts = %d, want 2", attempts)
	}
	if deliveries[0].LastError == nil || strings.Contains(*deliveries[0].LastError, "do not persist") {
		t.Fatalf("dead-letter error leaked response body: %#v", deliveries[0].LastError)
	}
}

func TestWebhookEndpointValidationRejectsSSRFAndSecretLifecycle(t *testing.T) {
	fixture := newWebhookFixture(t)
	for _, endpointURL := range []string{
		"http://example.com/hook", "https://127.0.0.1/hook", "https://169.254.169.254/latest",
		"https://192.0.2.1/hook", "https://[2001:db8::1]/hook",
		"https://user:secret@example.com/hook", "https://example.com/hook?token=secret",
	} {
		fixture.service.allowInsecureHTTP = false
		fixture.service.allowPrivateNetworks = false
		_, err := fixture.service.Create(
			context.Background(), fixture.principal, fixture.tenantID,
			CreateInput{Name: uuid.NewString(), URL: endpointURL, EventTypes: []string{"execution.failed"}},
			"validate-url", "127.0.0.1",
		)
		if err == nil {
			t.Fatalf("unsafe endpoint URL %q was accepted", endpointURL)
		}
	}
	fixture.service.allowInsecureHTTP = true
	fixture.service.allowPrivateNetworks = true
	issued, err := fixture.service.Create(
		context.Background(), fixture.principal, fixture.tenantID,
		CreateInput{Name: "rotate", URL: "http://127.0.0.1:8080/hook", EventTypes: []string{"execution.failed"}},
		"create-rotate", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := fixture.service.RotateSecret(
		context.Background(), fixture.principal, fixture.tenantID, issued.Endpoint.ID,
		"rotate", "127.0.0.1",
	)
	if err != nil || rotated.Secret == issued.Secret || rotated.Endpoint.SecretVersion != 2 {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	disabled, err := fixture.service.SetEnabled(
		context.Background(), fixture.principal, fixture.tenantID, issued.Endpoint.ID, false,
		"disable", "127.0.0.1",
	)
	if err != nil || disabled.Status != "disabled" {
		t.Fatalf("disabled=%#v err=%v", disabled, err)
	}
	revoked, err := fixture.service.Revoke(
		context.Background(), fixture.principal, fixture.tenantID, issued.Endpoint.ID,
		"revoke", "127.0.0.1",
	)
	if err != nil || revoked.Status != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("revoked=%#v err=%v", revoked, err)
	}
}

type webhookFixture struct {
	db        *gorm.DB
	service   *Service
	principal identity.Principal
	tenantID  uuid.UUID
	orgID     uuid.UUID
	projectID uuid.UUID
	sessionID uuid.UUID
	now       time.Time
}

func newWebhookFixture(t *testing.T) *webhookFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "developer-webhook-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	wrapper, err := credentialkms.NewLocalKeyWrapper("developer-webhook-test", key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := &webhookFixture{
		db:        store.DB(),
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		tenantID:  domain.TenantID, orgID: domain.OrganizationID,
		projectID: uuid.New(), sessionID: uuid.New(), now: now,
	}
	service, err := NewService(store.DB(), credentialkms.NewEnvelopeCipher(wrapper), Options{
		Timeout: time.Second, Client: &http.Client{}, AllowInsecureHTTP: true,
		AllowPrivateNetworks: true, Now: func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = service
	return fixture
}

func (f *webhookFixture) projectEvent(t *testing.T, eventType string, payload map[string]any) persistence.SessionEvent {
	t.Helper()
	event := persistence.SessionEvent{
		TenantID: f.tenantID, OrganizationID: f.orgID, ProjectID: f.projectID, SessionID: f.sessionID,
		Sequence: 1, EventID: uuid.New(), EventVersion: 2, EventType: eventType,
		ActorType: "worker", ExecutionID: pointer(uuid.New()), Payload: payload, OccurredAt: f.now,
	}
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		return f.service.ProjectSessionEvent(context.Background(), tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	return event
}

func (f *webhookFixture) dispatcher(t *testing.T) *outbox.Dispatcher {
	t.Helper()
	service, err := outbox.NewService(f.db, outbox.Config{
		BatchSize: 10, ClaimTTL: time.Second, MaxAttempts: 2,
		BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := outbox.NewDispatcher(
		service, f.service, time.Millisecond, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func pointer[T any](value T) *T { return &value }

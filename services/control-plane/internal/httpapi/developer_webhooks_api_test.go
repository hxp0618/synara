package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/developerwebhooks"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestDeveloperWebhookHTTPLifecycleAndScopedDeadLetterReplay(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	ownerToken := createProviderCapabilityLogin(t, fixture.db, fixture.ownerUserID, fixture.tenantID)
	wrapper, err := credentialkms.NewLocalKeyWrapper("webhook-http-test", bytes.Repeat([]byte{0x62}, 32))
	if err != nil {
		t.Fatal(err)
	}
	webhooks, err := developerwebhooks.NewService(
		fixture.db,
		credentialkms.NewEnvelopeCipher(wrapper),
		developerwebhooks.Options{
			Timeout: time.Second, AllowInsecureHTTP: true, AllowPrivateNetworks: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	outboxService, err := outbox.NewService(fixture.db, outbox.Config{
		BatchSize: 10, ClaimTTL: time.Minute, MaxAttempts: 3,
		BaseBackoff: time.Second, MaxBackoff: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.developerWebhooks = webhooks
	fixture.server.outbox = outboxService

	basePath := "/v1/tenants/" + fixture.tenantID.String() + "/developer-webhooks"
	created := fixture.webhookRequest(t, http.MethodPost, basePath, ownerToken, map[string]any{
		"name": "CI delivery", "url": "http://127.0.0.1:9080/hooks/polaris",
		"eventTypes": []string{"turn.completed", "execution.failed"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create Webhook status=%d body=%s", created.Code, created.Body.String())
	}
	var issued developerwebhooks.IssuedEndpoint
	if err := json.Unmarshal(created.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Secret, developerwebhooks.SecretPrefix) {
		t.Fatalf("issued Webhook secret = %q", issued.Secret)
	}

	listed := fixture.webhookRequest(t, http.MethodGet, basePath, ownerToken, nil)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), issued.Secret) || strings.Contains(listed.Body.String(), "encryptedSecret") {
		t.Fatalf("list Webhooks status=%d body=%s", listed.Code, listed.Body.String())
	}
	rotated := fixture.webhookRequest(
		t, http.MethodPost, basePath+"/"+issued.Endpoint.ID.String()+"/rotate-secret", ownerToken, nil,
	)
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate Webhook status=%d body=%s", rotated.Code, rotated.Body.String())
	}
	var rotatedIssue developerwebhooks.IssuedEndpoint
	if err := json.Unmarshal(rotated.Body.Bytes(), &rotatedIssue); err != nil {
		t.Fatal(err)
	}
	if rotatedIssue.Secret == issued.Secret || rotatedIssue.Endpoint.SecretVersion != 2 {
		t.Fatalf("rotated Webhook issue = %#v", rotatedIssue)
	}

	disabled := fixture.webhookRequest(
		t, http.MethodPost, basePath+"/"+issued.Endpoint.ID.String()+"/disable", ownerToken, nil,
	)
	if disabled.Code != http.StatusOK || !strings.Contains(disabled.Body.String(), `"status":"disabled"`) {
		t.Fatalf("disable Webhook status=%d body=%s", disabled.Code, disabled.Body.String())
	}

	now := time.Now().UTC()
	deliveryID := uuid.New()
	if err := fixture.db.Create(&persistence.OutboxMessage{
		ID: deliveryID, TenantID: &fixture.tenantID, Topic: developerwebhooks.OutboxTopic,
		MessageKey: issued.Endpoint.ID.String(), Payload: map[string]any{"deliveryId": deliveryID.String()},
		Headers:  map[string]any{"webhookEndpointId": issued.Endpoint.ID.String()},
		Attempts: 3, AvailableAt: now, CreatedAt: now, DeadLetteredAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.DeveloperWebhookDelivery{
		ID: deliveryID, TenantID: fixture.tenantID, EndpointID: issued.Endpoint.ID,
		SessionEventID: uuid.New(), EventType: "execution.failed", SessionID: fixture.sessionID,
		Sequence: 1, OutboxMessageID: deliveryID, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	deliveriesPath := basePath + "/" + issued.Endpoint.ID.String() + "/deliveries"
	deliveries := fixture.webhookRequest(t, http.MethodGet, deliveriesPath+"?limit=10", ownerToken, nil)
	if deliveries.Code != http.StatusOK || !strings.Contains(deliveries.Body.String(), `"status":"dead-letter"`) {
		t.Fatalf("list deliveries status=%d body=%s", deliveries.Code, deliveries.Body.String())
	}
	replayed := fixture.webhookRequest(
		t, http.MethodPost, deliveriesPath+"/"+deliveryID.String()+"/replay", ownerToken, nil,
	)
	if replayed.Code != http.StatusOK {
		t.Fatalf("replay delivery status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	var message persistence.OutboxMessage
	if err := fixture.db.Where("id = ?", deliveryID).Take(&message).Error; err != nil {
		t.Fatal(err)
	}
	if message.DeadLetteredAt != nil || message.Attempts != 0 {
		t.Fatalf("replayed Outbox state = %#v", message)
	}

	revoked := fixture.webhookRequest(
		t, http.MethodPost, basePath+"/"+issued.Endpoint.ID.String()+"/revoke", ownerToken, nil,
	)
	if revoked.Code != http.StatusOK || !strings.Contains(revoked.Body.String(), `"status":"revoked"`) {
		t.Fatalf("revoke Webhook status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where(
		"tenant_id = ? AND resource_type = ?", fixture.tenantID, "developer_webhook",
	).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount < 4 {
		t.Fatalf("Webhook audit entries = %d, want at least 4", auditCount)
	}
}

func TestDeveloperWebhookHTTPFailsClosedWithoutConfiguredService(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	ownerToken := createProviderCapabilityLogin(t, fixture.db, fixture.ownerUserID, fixture.tenantID)
	response := fixture.webhookRequest(
		t, http.MethodGet, "/v1/tenants/"+fixture.tenantID.String()+"/developer-webhooks", ownerToken, nil,
	)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "developer_webhooks_unavailable") {
		t.Fatalf("unconfigured Webhook API status=%d body=%s", response.Code, response.Body.String())
	}
}

func (f providerCapabilityHTTPFixture) webhookRequest(
	t *testing.T,
	method, path, token string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if len(payload) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

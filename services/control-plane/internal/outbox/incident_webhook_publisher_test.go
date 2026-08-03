package outbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIncidentWebhookPublisherSignsStableIdempotentEnvelope(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	message := Message{
		ID: uuid.New(), Topic: InternalIncidentUpdateTopic, MessageKey: "incident-1:3",
		CreatedAt: time.Date(2026, 8, 3, 2, 3, 4, 0, time.UTC),
		Payload:   map[string]any{"incidentId": "incident-1", "summary": "Employee-safe update"},
	}

	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Idempotency-Key") != message.ID.String() {
			t.Fatalf("unexpected request identity: %s %#v", request.Method, request.Header)
		}
		if request.Header.Get("X-Synara-Key-Id") != "incident-hmac-v2" {
			t.Fatalf("unexpected signing key ID: %q", request.Header.Get("X-Synara-Key-Id"))
		}
		received, _ = io.ReadAll(request.Body)
		signature := hmac.New(sha256.New, key)
		_, _ = signature.Write(received)
		if got, want := request.Header.Get("X-Synara-Signature"), "v1="+hex.EncodeToString(signature.Sum(nil)); got != want {
			t.Fatalf("signature = %q, want %q", got, want)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	publisher, err := NewIncidentWebhookPublisher(IncidentWebhookPublisherConfig{
		Endpoint: server.URL, KeyID: "incident-hmac-v2", HMACKey: key, Timeout: time.Second, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	var envelope incidentWebhookEnvelope
	if err := json.Unmarshal(received, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != "synara.internal-incident-notification.v1" || envelope.MessageID != message.ID.String() ||
		envelope.Topic != InternalIncidentUpdateTopic || envelope.MessageKey != message.MessageKey {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
}

func TestIncidentWebhookPublisherFailsClosed(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	for _, test := range []struct {
		name     string
		endpoint string
		keyID    string
		key      []byte
	}{
		{name: "http", endpoint: "http://notify.example.test/hook", keyID: "incident-v2", key: key},
		{name: "credential", endpoint: "https://user:secret@notify.example.test/hook", keyID: "incident-v2", key: key},
		{name: "query", endpoint: "https://notify.example.test/hook?token=secret", keyID: "incident-v2", key: key},
		{name: "missing key ID", endpoint: "https://notify.example.test/hook", key: key},
		{name: "invalid key ID", endpoint: "https://notify.example.test/hook", keyID: "incident key", key: key},
		{name: "short key", endpoint: "https://notify.example.test/hook", keyID: "incident-v2", key: []byte("short")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewIncidentWebhookPublisher(IncidentWebhookPublisherConfig{
				Endpoint: test.endpoint, KeyID: test.keyID, HMACKey: test.key, Timeout: time.Second,
			}); err == nil {
				t.Fatal("expected invalid publisher configuration")
			}
		})
	}
}

func TestIncidentWebhookPublisherDoesNotFollowRedirect(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	redirectTargetCalled := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetCalled = true
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	publisher, err := NewIncidentWebhookPublisher(IncidentWebhookPublisherConfig{
		Endpoint: server.URL, KeyID: "incident-hmac-v2", HMACKey: key, Timeout: time.Second, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.Publish(context.Background(), Message{
		ID: uuid.New(), Topic: InternalIncidentUpdateTopic, MessageKey: "incident-1:4",
		CreatedAt: time.Now(), Payload: map[string]any{"summary": "safe"},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("expected redirect rejection, got %v", err)
	}
	if redirectTargetCalled {
		t.Fatal("incident publisher followed a redirect")
	}
}

func TestTopicPublisherDoesNotAcknowledgeIncidentWithoutAdapter(t *testing.T) {
	publisher := TopicPublisher{
		Default: DatabasePublisher{},
		Topics: map[string]Publisher{
			InternalIncidentUpdateTopic: PublisherFunc(func(context.Context, Message) error {
				return context.DeadlineExceeded
			}),
		},
	}
	if err := publisher.Publish(context.Background(), Message{Topic: InternalIncidentUpdateTopic}); err == nil {
		t.Fatal("expected incident delivery to fail closed")
	}
	if err := publisher.Publish(context.Background(), Message{Topic: "execution.ready"}); err != nil {
		t.Fatalf("database-backed topic should use the default publisher: %v", err)
	}
}

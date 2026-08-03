package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

const InternalIncidentUpdateTopic = "incident.internal-update"

type TopicPublisher struct {
	Default Publisher
	Topics  map[string]Publisher
}

func (p TopicPublisher) Publish(ctx context.Context, message Message) error {
	if publisher := p.Topics[message.Topic]; publisher != nil {
		return publisher.Publish(ctx, message)
	}
	if p.Default == nil {
		return fmt.Errorf("no Outbox publisher configured for topic %q", message.Topic)
	}
	return p.Default.Publish(ctx, message)
}

type IncidentWebhookPublisherConfig struct {
	Endpoint string
	KeyID    string
	HMACKey  []byte
	Timeout  time.Duration
	Client   *http.Client
}

type IncidentWebhookPublisher struct {
	endpoint string
	keyID    string
	hmacKey  []byte
	client   *http.Client
}

type incidentWebhookEnvelope struct {
	SchemaVersion string         `json:"schemaVersion"`
	MessageID     string         `json:"messageId"`
	Topic         string         `json:"topic"`
	MessageKey    string         `json:"messageKey"`
	CreatedAt     time.Time      `json:"createdAt"`
	Payload       map[string]any `json:"payload"`
}

func NewIncidentWebhookPublisher(cfg IncidentWebhookPublisherConfig) (*IncidentWebhookPublisher, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("incident webhook endpoint must be a credential-free HTTPS URL without query or fragment")
	}
	if len(cfg.HMACKey) != 32 {
		return nil, errors.New("incident webhook HMAC key must contain exactly 32 bytes")
	}
	keyID, keyIDValid := validation.OpaqueIdentifier(cfg.KeyID, 2, 200)
	if !keyIDValid {
		return nil, errors.New("incident webhook key ID must be a bounded opaque identifier")
	}
	if cfg.Timeout <= 0 || cfg.Timeout > 30*time.Second {
		return nil, errors.New("incident webhook timeout must be positive and at most 30 seconds")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	clientCopy.Timeout = cfg.Timeout
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &IncidentWebhookPublisher{
		endpoint: parsed.String(),
		keyID:    keyID,
		hmacKey:  append([]byte(nil), cfg.HMACKey...),
		client:   &clientCopy,
	}, nil
}

func (p *IncidentWebhookPublisher) Publish(ctx context.Context, message Message) error {
	if message.Topic != InternalIncidentUpdateTopic {
		return fmt.Errorf("incident webhook refuses topic %q", message.Topic)
	}
	body, err := json.Marshal(incidentWebhookEnvelope{
		SchemaVersion: "synara.internal-incident-notification.v1",
		MessageID:     message.ID.String(),
		Topic:         message.Topic,
		MessageKey:    message.MessageKey,
		CreatedAt:     message.CreatedAt.UTC(),
		Payload:       message.Payload,
	})
	if err != nil {
		return fmt.Errorf("encode incident webhook envelope: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("create incident webhook request")
	}
	signature := hmac.New(sha256.New, p.hmacKey)
	_, _ = signature.Write(body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", message.ID.String())
	request.Header.Set("X-Synara-Topic", message.Topic)
	request.Header.Set("X-Synara-Key-Id", p.keyID)
	request.Header.Set("X-Synara-Signature", "v1="+hex.EncodeToString(signature.Sum(nil)))

	response, err := p.client.Do(request)
	if err != nil {
		return errors.New("incident webhook request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("incident webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

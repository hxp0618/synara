package developerwebhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	OutboxTopic  = "developer.webhook.delivery"
	SecretPrefix = "whsec_"
)

var supportedEventTypes = map[string]struct{}{
	"turn.completed": {}, "request.opened": {}, "approval.requested": {},
	"execution.completed": {}, "execution.failed": {}, "execution.cancelled": {},
	"execution.interrupted": {}, "execution.suspended": {},
}

type Options struct {
	Client               *http.Client
	Timeout              time.Duration
	AllowInsecureHTTP    bool
	AllowPrivateNetworks bool
	Now                  func() time.Time
}

type Service struct {
	db                   *gorm.DB
	authorizer           *authorization.Authorizer
	cipher               *credentialkms.EnvelopeCipher
	client               *http.Client
	allowInsecureHTTP    bool
	allowPrivateNetworks bool
	now                  func() time.Time
}

type Endpoint struct {
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenantId"`
	Name               string     `json:"name"`
	URL                string     `json:"url"`
	Status             string     `json:"status"`
	EventTypes         []string   `json:"eventTypes"`
	SecretVersion      int64      `json:"secretVersion"`
	LastDeliveredAt    *time.Time `json:"lastDeliveredAt"`
	LastFailedAt       *time.Time `json:"lastFailedAt"`
	LastFailureSummary *string    `json:"lastFailureSummary"`
	CreatedBy          uuid.UUID  `json:"createdBy"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	RevokedAt          *time.Time `json:"revokedAt"`
}

type IssuedEndpoint struct {
	Endpoint Endpoint `json:"endpoint"`
	Secret   string   `json:"secret"`
}

type CreateInput struct {
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
}

type Delivery struct {
	ID             uuid.UUID  `json:"id"`
	EndpointID     uuid.UUID  `json:"endpointId"`
	SessionEventID uuid.UUID  `json:"sessionEventId"`
	EventType      string     `json:"eventType"`
	SessionID      uuid.UUID  `json:"sessionId"`
	ExecutionID    *uuid.UUID `json:"executionId"`
	Sequence       int64      `json:"sequence"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	AvailableAt    time.Time  `json:"availableAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	PublishedAt    *time.Time `json:"publishedAt"`
	DeadLetteredAt *time.Time `json:"deadLetteredAt"`
	LastError      *string    `json:"lastError"`
}

func NewService(db *gorm.DB, cipher *credentialkms.EnvelopeCipher, options Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("developer webhook database is required")
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if timeout < time.Second || timeout > 30*time.Second {
		return nil, errors.New("developer webhook timeout must be between 1 and 30 seconds")
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	client := options.Client
	if client == nil {
		client = safeWebhookHTTPClient(timeout, options.AllowPrivateNetworks)
	} else {
		copy := *client
		copy.Timeout = timeout
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), cipher: cipher, client: client,
		allowInsecureHTTP: options.AllowInsecureHTTP, allowPrivateNetworks: options.AllowPrivateNetworks,
		now: now,
	}, nil
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Endpoint, error) {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxRead); err != nil {
		return nil, err
	}
	models := make([]persistence.DeveloperWebhookEndpoint, 0)
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC, id DESC").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "developer_webhooks_load_failed", "Webhook endpoints could not be loaded.", err)
	}
	items := make([]Endpoint, 0, len(models))
	for _, model := range models {
		items = append(items, toEndpoint(model))
	}
	return items, nil
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (IssuedEndpoint, error) {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxManage); err != nil {
		return IssuedEndpoint{}, err
	}
	name, endpointURL, eventTypes, err := s.normalizeInput(input)
	if err != nil {
		return IssuedEndpoint{}, err
	}
	if s.cipher == nil {
		return IssuedEndpoint{}, problem.New(503, "developer_webhook_kms_unavailable", "Webhook secret encryption is unavailable.")
	}
	id := uuid.New()
	secret, err := newSecret()
	if err != nil {
		return IssuedEndpoint{}, problem.Wrap(500, "developer_webhook_secret_failed", "Webhook secret could not be generated.", err)
	}
	envelope, err := s.cipher.Encrypt(ctx, []byte(secret), endpointSecretAAD(tenantID, id, 1))
	if err != nil {
		return IssuedEndpoint{}, problem.Wrap(503, "developer_webhook_kms_unavailable", "Webhook secret could not be encrypted.", err)
	}
	now := s.now().UTC()
	model := persistence.DeveloperWebhookEndpoint{
		ID: id, TenantID: tenantID, Name: name, URL: endpointURL, Status: "active", EventTypes: eventTypes,
		SecretVersion: 1, EncryptedSecret: envelope.EncryptedPayload, EncryptedDataKey: envelope.EncryptedDataKey,
		KMSProvider: envelope.KMSProvider, KMSKeyID: envelope.KMSKeyID,
		CreatedBy: principal.UserID, CreatedAt: now, UpdatedAt: now,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return problem.New(409, "developer_webhook_name_conflict", "A Webhook endpoint with this name already exists.")
			}
			return problem.Wrap(500, "developer_webhook_create_failed", "Webhook endpoint could not be created.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "developer_webhook.created", ResourceType: "developer_webhook", ResourceID: &id,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"name": name, "eventTypes": eventTypes},
		})
	})
	if err != nil {
		return IssuedEndpoint{}, err
	}
	return IssuedEndpoint{Endpoint: toEndpoint(model), Secret: secret}, nil
}

func (s *Service) RotateSecret(
	ctx context.Context,
	principal identity.Principal,
	tenantID, endpointID uuid.UUID,
	requestID, ipAddress string,
) (IssuedEndpoint, error) {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxManage); err != nil {
		return IssuedEndpoint{}, err
	}
	if s.cipher == nil {
		return IssuedEndpoint{}, problem.New(503, "developer_webhook_kms_unavailable", "Webhook secret encryption is unavailable.")
	}
	secret, err := newSecret()
	if err != nil {
		return IssuedEndpoint{}, problem.Wrap(500, "developer_webhook_secret_failed", "Webhook secret could not be generated.", err)
	}
	var model persistence.DeveloperWebhookEndpoint
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where(
			"tenant_id = ? AND id = ? AND status <> ?", tenantID, endpointID, "revoked",
		).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "developer_webhook_not_found", "Webhook endpoint not found.")
		} else if err != nil {
			return problem.Wrap(500, "developer_webhook_load_failed", "Webhook endpoint could not be loaded.", err)
		}
		nextVersion := model.SecretVersion + 1
		envelope, err := s.cipher.Encrypt(ctx, []byte(secret), endpointSecretAAD(tenantID, endpointID, nextVersion))
		if err != nil {
			return problem.Wrap(503, "developer_webhook_kms_unavailable", "Webhook secret could not be encrypted.", err)
		}
		now := s.now().UTC()
		if err := tx.Model(&persistence.DeveloperWebhookEndpoint{}).Where(
			"tenant_id = ? AND id = ? AND secret_version = ?", tenantID, endpointID, model.SecretVersion,
		).Updates(map[string]any{
			"secret_version": nextVersion, "encrypted_secret": envelope.EncryptedPayload,
			"encrypted_data_key": envelope.EncryptedDataKey, "kms_provider": envelope.KMSProvider,
			"kms_key_id": envelope.KMSKeyID, "updated_at": now,
		}).Error; err != nil {
			return problem.Wrap(500, "developer_webhook_rotate_failed", "Webhook secret could not be rotated.", err)
		}
		model.SecretVersion = nextVersion
		model.EncryptedSecret = envelope.EncryptedPayload
		model.EncryptedDataKey = envelope.EncryptedDataKey
		model.KMSProvider = envelope.KMSProvider
		model.KMSKeyID = envelope.KMSKeyID
		model.UpdatedAt = now
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "developer_webhook.secret_rotated", ResourceType: "developer_webhook", ResourceID: &endpointID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"secretVersion": nextVersion},
		})
	})
	if err != nil {
		return IssuedEndpoint{}, err
	}
	return IssuedEndpoint{Endpoint: toEndpoint(model), Secret: secret}, nil
}

func (s *Service) SetEnabled(
	ctx context.Context,
	principal identity.Principal,
	tenantID, endpointID uuid.UUID,
	enabled bool,
	requestID, ipAddress string,
) (Endpoint, error) {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxManage); err != nil {
		return Endpoint{}, err
	}
	status := "disabled"
	if enabled {
		status = "active"
	}
	var model persistence.DeveloperWebhookEndpoint
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&persistence.DeveloperWebhookEndpoint{}).Where(
			"tenant_id = ? AND id = ? AND status <> ?", tenantID, endpointID, "revoked",
		).Updates(map[string]any{"status": status, "updated_at": s.now().UTC()})
		if result.Error != nil {
			return problem.Wrap(500, "developer_webhook_update_failed", "Webhook endpoint could not be updated.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(404, "developer_webhook_not_found", "Webhook endpoint not found.")
		}
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, endpointID).Take(&model).Error; err != nil {
			return problem.Wrap(500, "developer_webhook_load_failed", "Webhook endpoint could not be loaded.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "developer_webhook." + status, ResourceType: "developer_webhook", ResourceID: &endpointID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	})
	return toEndpoint(model), err
}

func (s *Service) Revoke(
	ctx context.Context,
	principal identity.Principal,
	tenantID, endpointID uuid.UUID,
	requestID, ipAddress string,
) (Endpoint, error) {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxManage); err != nil {
		return Endpoint{}, err
	}
	var model persistence.DeveloperWebhookEndpoint
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.now().UTC()
		result := tx.Model(&persistence.DeveloperWebhookEndpoint{}).Where(
			"tenant_id = ? AND id = ? AND status <> ?", tenantID, endpointID, "revoked",
		).Updates(map[string]any{"status": "revoked", "revoked_at": now, "updated_at": now})
		if result.Error != nil {
			return problem.Wrap(500, "developer_webhook_revoke_failed", "Webhook endpoint could not be revoked.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(404, "developer_webhook_not_found", "Webhook endpoint not found.")
		}
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, endpointID).Take(&model).Error; err != nil {
			return problem.Wrap(500, "developer_webhook_load_failed", "Webhook endpoint could not be loaded.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "developer_webhook.revoked", ResourceType: "developer_webhook", ResourceID: &endpointID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	})
	return toEndpoint(model), err
}

func (s *Service) ListDeliveries(
	ctx context.Context,
	principal identity.Principal,
	tenantID, endpointID uuid.UUID,
	limit int,
) ([]Delivery, error) {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxRead); err != nil {
		return nil, err
	}
	type row struct {
		persistence.DeveloperWebhookDelivery
		Attempts       int
		AvailableAt    time.Time
		PublishedAt    *time.Time
		DeadLetteredAt *time.Time
		LastError      *string
	}
	rows := make([]row, 0)
	if err := s.db.WithContext(ctx).Table("developer_webhook_deliveries AS delivery").
		Select("delivery.*, message.attempts, message.available_at, message.published_at, message.dead_lettered_at, message.last_error").
		Joins("JOIN outbox_messages AS message ON message.id = delivery.outbox_message_id").
		Where("delivery.tenant_id = ? AND delivery.endpoint_id = ?", tenantID, endpointID).
		Order("delivery.created_at DESC, delivery.id DESC").Limit(persistence.NormalizeLimit(limit, 50, 200)).
		Scan(&rows).Error; err != nil {
		return nil, problem.Wrap(500, "developer_webhook_deliveries_load_failed", "Webhook deliveries could not be loaded.", err)
	}
	items := make([]Delivery, 0, len(rows))
	for _, item := range rows {
		status := "pending"
		if item.PublishedAt != nil {
			status = "published"
		} else if item.DeadLetteredAt != nil {
			status = "dead-letter"
		} else if item.Attempts > 0 {
			status = "retrying"
		}
		items = append(items, Delivery{
			ID: item.ID, EndpointID: item.EndpointID, SessionEventID: item.SessionEventID,
			EventType: item.EventType, SessionID: item.SessionID, ExecutionID: item.ExecutionID,
			Sequence: item.Sequence, Status: status, Attempts: item.Attempts,
			AvailableAt: item.AvailableAt, CreatedAt: item.CreatedAt,
			PublishedAt: item.PublishedAt, DeadLetteredAt: item.DeadLetteredAt, LastError: item.LastError,
		})
	}
	return items, nil
}

// AuthorizeDeliveryReplay verifies that a delivery belongs to the requested
// endpoint and tenant before the HTTP layer asks the shared Outbox service to
// replay its message. Delivery IDs intentionally match their Outbox message ID.
func (s *Service) AuthorizeDeliveryReplay(
	ctx context.Context,
	principal identity.Principal,
	tenantID, endpointID, deliveryID uuid.UUID,
) error {
	if err := s.require(ctx, principal, tenantID, authorization.OutboxManage); err != nil {
		return err
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&persistence.DeveloperWebhookDelivery{}).
		Where("tenant_id = ? AND endpoint_id = ? AND id = ? AND outbox_message_id = ?", tenantID, endpointID, deliveryID, deliveryID).
		Count(&count).Error; err != nil {
		return problem.Wrap(500, "developer_webhook_delivery_load_failed", "Webhook delivery could not be loaded.", err)
	}
	if count != 1 {
		return problem.New(404, "developer_webhook_delivery_not_found", "Webhook delivery not found.")
	}
	return nil
}

func (s *Service) require(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, permission authorization.Permission) error {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return err
	}
	_, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, permission)
	return err
}

func (s *Service) normalizeInput(input CreateInput) (string, string, []string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 120 || strings.ContainsAny(name, "\r\n\t") {
		return "", "", nil, problem.New(400, "invalid_developer_webhook_name", "name must contain between 1 and 120 safe characters.")
	}
	parsed, err := validateEndpointURL(input.URL, s.allowInsecureHTTP, s.allowPrivateNetworks)
	if err != nil {
		return "", "", nil, problem.New(400, "invalid_developer_webhook_url", err.Error())
	}
	eventTypes, err := normalizeEventTypes(input.EventTypes)
	if err != nil {
		return "", "", nil, err
	}
	return name, parsed.String(), eventTypes, nil
}

func normalizeEventTypes(values []string) ([]string, error) {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, ok := supportedEventTypes[value]; !ok {
			return nil, problem.New(400, "invalid_developer_webhook_event_type", "eventTypes contains an unsupported Event type.")
		}
		unique[value] = struct{}{}
	}
	if len(unique) == 0 || len(unique) > 20 {
		return nil, problem.New(400, "invalid_developer_webhook_event_types", "eventTypes must contain between 1 and 20 unique Event types.")
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validateEndpointURL(raw string, allowHTTP, allowPrivate bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("url must be a credential-free HTTPS URL without query or fragment")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return nil, errors.New("url must use HTTPS")
	}
	if len(parsed.String()) > 2048 {
		return nil, errors.New("url must not exceed 2048 characters")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && !allowPrivate && forbiddenWebhookIP(ip) {
		return nil, errors.New("url must not target a private, loopback, link-local, or reserved address")
	}
	return parsed, nil
}

func newSecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(value), nil
}

func endpointSecretAAD(tenantID, endpointID uuid.UUID, version int64) []byte {
	return []byte("developer-webhook:" + tenantID.String() + ":" + endpointID.String() + ":" + strconv.FormatInt(version, 10))
}

func toEndpoint(model persistence.DeveloperWebhookEndpoint) Endpoint {
	return Endpoint{
		ID: model.ID, TenantID: model.TenantID, Name: model.Name, URL: model.URL, Status: model.Status,
		EventTypes: append([]string(nil), model.EventTypes...), SecretVersion: model.SecretVersion,
		LastDeliveredAt: model.LastDeliveredAt, LastFailedAt: model.LastFailedAt,
		LastFailureSummary: model.LastFailureSummary, CreatedBy: model.CreatedBy,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt, RevokedAt: model.RevokedAt,
	}
}

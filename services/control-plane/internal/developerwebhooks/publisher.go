package developerwebhooks

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
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func (s *Service) Publish(ctx context.Context, message outbox.Message) error {
	if message.Topic != OutboxTopic {
		return fmt.Errorf("developer webhook publisher refuses topic %q", message.Topic)
	}
	endpointID, err := webhookEndpointID(message)
	if err != nil {
		return err
	}
	var endpoint persistence.DeveloperWebhookEndpoint
	if err := s.db.WithContext(ctx).Where("id = ?", endpointID).Take(&endpoint).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return errors.New("load developer webhook endpoint")
	}
	if endpoint.Status != "active" {
		return nil
	}
	if message.TenantID == nil || *message.TenantID != endpoint.TenantID {
		return errors.New("developer webhook message tenant does not match endpoint")
	}
	var deliveryCount int64
	if err := s.db.WithContext(ctx).Model(&persistence.DeveloperWebhookDelivery{}).Where(
		"tenant_id = ? AND endpoint_id = ? AND id = ? AND outbox_message_id = ?",
		endpoint.TenantID, endpoint.ID, message.ID, message.ID,
	).Count(&deliveryCount).Error; err != nil || deliveryCount != 1 {
		return errors.New("developer webhook delivery binding is invalid")
	}
	if _, err := validateEndpointURL(endpoint.URL, s.allowInsecureHTTP, s.allowPrivateNetworks); err != nil {
		return s.recordFailure(ctx, endpoint, "Webhook destination is no longer allowed.")
	}
	if s.cipher == nil {
		return s.recordFailure(ctx, endpoint, "Webhook secret decryption is unavailable.")
	}
	secret, err := s.cipher.Decrypt(ctx, credentialkms.Envelope{
		EncryptedPayload: endpoint.EncryptedSecret, EncryptedDataKey: endpoint.EncryptedDataKey,
		KMSProvider: endpoint.KMSProvider, KMSKeyID: endpoint.KMSKeyID,
	}, endpointSecretAAD(endpoint.TenantID, endpoint.ID, endpoint.SecretVersion))
	if err != nil {
		return s.recordFailure(ctx, endpoint, "Webhook secret could not be decrypted.")
	}
	defer clear(secret)
	body, err := json.Marshal(message.Payload)
	if err != nil {
		return s.recordFailure(ctx, endpoint, "Webhook payload could not be encoded.")
	}
	timestamp := strconv.FormatInt(s.now().UTC().Unix(), 10)
	signature := signWebhook(secret, timestamp, body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader(body))
	if err != nil {
		return s.recordFailure(ctx, endpoint, "Webhook request could not be created.")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", message.ID.String())
	request.Header.Set("X-Polaris-Webhook-Id", message.ID.String())
	request.Header.Set("X-Polaris-Webhook-Timestamp", timestamp)
	request.Header.Set("X-Polaris-Webhook-Signature", "v1="+signature)

	response, err := s.client.Do(request)
	if err != nil {
		return s.recordFailure(ctx, endpoint, "Webhook request failed.")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return s.recordFailure(ctx, endpoint, fmt.Sprintf("Webhook destination returned HTTP %d.", response.StatusCode))
	}
	now := s.now().UTC()
	if err := s.db.WithContext(ctx).Model(&persistence.DeveloperWebhookEndpoint{}).
		Where("tenant_id = ? AND id = ?", endpoint.TenantID, endpoint.ID).
		Updates(map[string]any{
			"last_delivered_at": now, "last_failed_at": nil, "last_failure_summary": nil,
		}).Error; err != nil {
		return errors.New("record developer webhook success")
	}
	return nil
}

func webhookEndpointID(message outbox.Message) (uuid.UUID, error) {
	value, ok := message.Headers["webhookEndpointId"]
	if !ok {
		return uuid.Nil, errors.New("developer webhook payload has no endpoint ID")
	}
	parsed, err := uuid.Parse(fmt.Sprint(value))
	if err != nil {
		return uuid.Nil, errors.New("developer webhook payload endpoint ID is invalid")
	}
	return parsed, nil
}

func (s *Service) recordFailure(ctx context.Context, endpoint persistence.DeveloperWebhookEndpoint, summary string) error {
	now := s.now().UTC()
	_ = s.db.WithContext(context.WithoutCancel(ctx)).Model(&persistence.DeveloperWebhookEndpoint{}).
		Where("tenant_id = ? AND id = ?", endpoint.TenantID, endpoint.ID).
		Updates(map[string]any{"last_failed_at": now, "last_failure_summary": summary}).Error
	return errors.New(summary)
}

func signWebhook(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func safeWebhookHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: min(timeout, 10*time.Second), KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid webhook destination address")
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, errors.New("resolve webhook destination")
			}
			for _, address := range addresses {
				if !allowPrivate && forbiddenWebhookIP(address.IP) {
					return nil, errors.New("webhook destination resolved to a non-public address")
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		},
		ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func forbiddenWebhookIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return true
	}
	for _, rawPrefix := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"100::/64", "2001:db8::/32",
	} {
		if netip.MustParsePrefix(rawPrefix).Contains(address) {
			return true
		}
	}
	return false
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

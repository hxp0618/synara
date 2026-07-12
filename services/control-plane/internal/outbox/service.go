package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const maxErrorSummaryBytes = 512

var (
	sensitiveAssignment = regexp.MustCompile(`(?i)\b(authorization|bearer|token|secret|password|credential|api[_-]?key|cookie|payload|prompt)\b\s*[:=]\s*("[^"]*"|'[^']*'|[^,;\s]+)`)
	bearerCredential    = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	urlQuery            = regexp.MustCompile(`https?://[^\s?]+\?[^\s]+`)
	highRiskToken       = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
)

type Config struct {
	InstanceID  string
	BatchSize   int
	ClaimTTL    time.Duration
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Now         func() time.Time
}

type Message struct {
	ID          uuid.UUID      `json:"id"`
	TenantID    *uuid.UUID     `json:"tenantId,omitempty"`
	Topic       string         `json:"topic"`
	MessageKey  string         `json:"messageKey"`
	Payload     map[string]any `json:"payload"`
	Headers     map[string]any `json:"headers"`
	Attempts    int            `json:"attempts"`
	AvailableAt time.Time      `json:"availableAt"`
	CreatedAt   time.Time      `json:"createdAt"`
}

type Stats struct {
	Pending       int64
	Retrying      int64
	DeadLettered  int64
	OldestPending time.Duration
}

type AdminMessage struct {
	ID             uuid.UUID  `json:"id"`
	Topic          string     `json:"topic"`
	MessageKey     string     `json:"messageKey"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	AvailableAt    time.Time  `json:"availableAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	ClaimedAt      *time.Time `json:"claimedAt,omitempty"`
	ClaimExpiresAt *time.Time `json:"claimExpiresAt,omitempty"`
	PublishedAt    *time.Time `json:"publishedAt,omitempty"`
	DeadLetteredAt *time.Time `json:"deadLetteredAt,omitempty"`
	LastError      *string    `json:"lastError,omitempty"`
}

type Service struct {
	db          *gorm.DB
	authorizer  *authorization.Authorizer
	instanceID  string
	batchSize   int
	claimTTL    time.Duration
	maxAttempts int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	now         func() time.Time
}

func NewService(db *gorm.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("outbox database is required")
	}
	instanceID := strings.TrimSpace(cfg.InstanceID)
	if instanceID == "" {
		instanceID = uuid.NewString()
	}
	if len(instanceID) > 160 || strings.ContainsAny(instanceID, "\r\n\t") {
		return nil, errors.New("outbox instance id must not exceed 160 characters or contain control whitespace")
	}
	if cfg.BatchSize <= 0 {
		return nil, errors.New("outbox batch size must be positive")
	}
	if cfg.ClaimTTL <= 0 {
		return nil, errors.New("outbox claim TTL must be positive")
	}
	if cfg.MaxAttempts <= 0 {
		return nil, errors.New("outbox max attempts must be positive")
	}
	if cfg.BaseBackoff <= 0 || cfg.MaxBackoff < cfg.BaseBackoff {
		return nil, errors.New("outbox backoff bounds are invalid")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), instanceID: instanceID,
		batchSize: cfg.BatchSize, claimTTL: cfg.ClaimTTL,
		maxAttempts: cfg.MaxAttempts, baseBackoff: cfg.BaseBackoff,
		maxBackoff: cfg.MaxBackoff, now: now,
	}, nil
}

func (s *Service) BatchSize() int { return s.batchSize }

func (s *Service) Claim(ctx context.Context) ([]Message, error) {
	now := s.now().UTC()
	claimExpiresAt := now.Add(s.claimTTL)
	models := make([]persistence.OutboxMessage, 0, s.batchSize)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.WithContext(ctx).
			Where("published_at IS NULL AND dead_lettered_at IS NULL").
			Where("available_at <= ?", now).
			Where("claimed_by IS NULL OR claim_expires_at <= ?", now).
			Order("available_at, created_at, id").
			Limit(s.batchSize)
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := query.Find(&models).Error; err != nil {
			return fmt.Errorf("load claimable outbox messages: %w", err)
		}
		if len(models) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, 0, len(models))
		for _, model := range models {
			ids = append(ids, model.ID)
		}
		result := tx.WithContext(ctx).Model(&persistence.OutboxMessage{}).
			Where("id IN ? AND published_at IS NULL AND dead_lettered_at IS NULL", ids).
			Where("claimed_by IS NULL OR claim_expires_at <= ?", now).
			Updates(map[string]any{
				"claimed_by": s.instanceID, "claimed_at": now, "claim_expires_at": claimExpiresAt,
			})
		if result.Error != nil {
			return fmt.Errorf("claim outbox messages: %w", result.Error)
		}
		if result.RowsAffected != int64(len(models)) {
			return fmt.Errorf("claim outbox messages: expected %d rows, updated %d", len(models), result.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(models))
	for _, model := range models {
		messages = append(messages, toMessage(model))
	}
	return messages, nil
}

func (s *Service) Acknowledge(ctx context.Context, messageID uuid.UUID) error {
	now := s.now().UTC()
	result := s.db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("id = ? AND claimed_by = ? AND published_at IS NULL AND dead_lettered_at IS NULL", messageID, s.instanceID).
		Updates(map[string]any{
			"published_at": now, "claimed_by": nil, "claimed_at": nil,
			"claim_expires_at": nil, "last_error": nil,
		})
	return expectClaimed(result, "acknowledge")
}

func (s *Service) Fail(ctx context.Context, message Message, publishErr error) error {
	now := s.now().UTC()
	attempts := message.Attempts + 1
	updates := map[string]any{
		"attempts": attempts, "last_error": errorSummary(publishErr),
		"claimed_by": nil, "claimed_at": nil, "claim_expires_at": nil,
	}
	if attempts >= s.maxAttempts {
		updates["dead_lettered_at"] = now
	} else {
		updates["available_at"] = now.Add(s.retryDelay(message.ID, attempts))
	}
	result := s.db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("id = ? AND claimed_by = ? AND published_at IS NULL AND dead_lettered_at IS NULL", message.ID, s.instanceID).
		Updates(updates)
	return expectClaimed(result, "record failure for")
}

func (s *Service) Release(ctx context.Context, messageID uuid.UUID) error {
	result := s.db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("id = ? AND claimed_by = ? AND published_at IS NULL AND dead_lettered_at IS NULL", messageID, s.instanceID).
		Updates(map[string]any{"claimed_by": nil, "claimed_at": nil, "claim_expires_at": nil})
	return expectClaimed(result, "release")
}

func (s *Service) Replay(ctx context.Context, tenantID, messageID uuid.UUID) (Message, error) {
	return s.replay(ctx, s.db, tenantID, messageID)
}

func (s *Service) replay(ctx context.Context, db *gorm.DB, tenantID, messageID uuid.UUID) (Message, error) {
	now := s.now().UTC()
	result := db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("id = ? AND tenant_id = ? AND dead_lettered_at IS NOT NULL", messageID, tenantID).
		Updates(map[string]any{
			"attempts": 0, "available_at": now, "claimed_by": nil, "claimed_at": nil,
			"claim_expires_at": nil, "published_at": nil, "dead_lettered_at": nil, "last_error": nil,
		})
	if result.Error != nil {
		return Message{}, problem.Wrap(500, "outbox_replay_failed", "The dead-letter message could not be replayed.", result.Error)
	}
	if result.RowsAffected != 1 {
		return Message{}, problem.New(404, "outbox_message_not_found", "Outbox message not found.")
	}
	var model persistence.OutboxMessage
	if err := db.WithContext(ctx).Where("id = ? AND tenant_id = ?", messageID, tenantID).Take(&model).Error; err != nil {
		return Message{}, problem.Wrap(500, "outbox_replay_load_failed", "The replayed outbox message could not be loaded.", err)
	}
	return toMessage(model), nil
}

func (s *Service) ListForTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	status string,
	limit int,
) ([]AdminMessage, error) {
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.OutboxRead); err != nil {
		return nil, err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	query := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	switch status {
	case "", "all":
	case "pending":
		query = query.Where("published_at IS NULL AND dead_lettered_at IS NULL AND attempts = 0")
	case "retrying":
		query = query.Where("published_at IS NULL AND dead_lettered_at IS NULL AND attempts > 0")
	case "dead-letter":
		query = query.Where("dead_lettered_at IS NOT NULL")
	case "published":
		query = query.Where("published_at IS NOT NULL")
	default:
		return nil, problem.New(400, "invalid_outbox_status", "Outbox status must be pending, retrying, dead-letter, published, or all.")
	}
	models := make([]persistence.OutboxMessage, 0)
	if err := query.Order("created_at DESC, id DESC").Limit(persistence.NormalizeLimit(limit, 50, 200)).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "outbox_list_failed", "Outbox messages could not be loaded.", err)
	}
	items := make([]AdminMessage, 0, len(models))
	for _, model := range models {
		items = append(items, toAdminMessage(model))
	}
	return items, nil
}

func (s *Service) ReplayAuthorized(
	ctx context.Context,
	principal identity.Principal,
	tenantID, messageID uuid.UUID,
	requestID, ipAddress string,
) (Message, error) {
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.OutboxManage); err != nil {
		return Message{}, err
	}
	var replayed Message
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		message, err := s.replay(ctx, tx, tenantID, messageID)
		if err != nil {
			return err
		}
		replayed = message
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "outbox.replayed", ResourceType: "outbox_message", ResourceID: &messageID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"topic": message.Topic, "messageKey": message.MessageKey},
		})
	})
	if err != nil {
		return Message{}, err
	}
	return replayed, nil
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	now := s.now().UTC()
	var stats Stats
	if err := s.db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("published_at IS NULL AND dead_lettered_at IS NULL").Count(&stats.Pending).Error; err != nil {
		return Stats{}, fmt.Errorf("count pending outbox messages: %w", err)
	}
	if err := s.db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("published_at IS NULL AND dead_lettered_at IS NULL AND attempts > 0").Count(&stats.Retrying).Error; err != nil {
		return Stats{}, fmt.Errorf("count retrying outbox messages: %w", err)
	}
	if err := s.db.WithContext(ctx).Model(&persistence.OutboxMessage{}).
		Where("dead_lettered_at IS NOT NULL").Count(&stats.DeadLettered).Error; err != nil {
		return Stats{}, fmt.Errorf("count dead-letter outbox messages: %w", err)
	}
	var oldest persistence.OutboxMessage
	err := s.db.WithContext(ctx).Where("published_at IS NULL AND dead_lettered_at IS NULL").Order("created_at, id").Take(&oldest).Error
	if err == nil && oldest.CreatedAt.Before(now) {
		stats.OldestPending = now.Sub(oldest.CreatedAt)
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return Stats{}, fmt.Errorf("load oldest outbox message: %w", err)
	}
	return stats, nil
}

func (s *Service) retryDelay(id uuid.UUID, attempts int) time.Duration {
	exponent := min(attempts-1, 30)
	base := float64(s.baseBackoff) * math.Pow(2, float64(exponent))
	if base > float64(s.maxBackoff) {
		base = float64(s.maxBackoff)
	}
	hash := sha256.Sum256(append(id[:], byte(attempts)))
	fraction := float64(binary.BigEndian.Uint64(hash[:8])) / float64(math.MaxUint64)
	delay := time.Duration(base + base*0.25*fraction)
	if delay > s.maxBackoff {
		return s.maxBackoff
	}
	return delay
}

func toMessage(model persistence.OutboxMessage) Message {
	return Message{
		ID: model.ID, TenantID: model.TenantID, Topic: model.Topic, MessageKey: model.MessageKey,
		Payload: model.Payload, Headers: model.Headers, Attempts: model.Attempts,
		AvailableAt: model.AvailableAt, CreatedAt: model.CreatedAt,
	}
}

func toAdminMessage(model persistence.OutboxMessage) AdminMessage {
	status := "pending"
	if model.PublishedAt != nil {
		status = "published"
	} else if model.DeadLetteredAt != nil {
		status = "dead-letter"
	} else if model.Attempts > 0 {
		status = "retrying"
	}
	return AdminMessage{
		ID: model.ID, Topic: model.Topic, MessageKey: model.MessageKey, Status: status,
		Attempts: model.Attempts, AvailableAt: model.AvailableAt, CreatedAt: model.CreatedAt,
		ClaimedAt: model.ClaimedAt, ClaimExpiresAt: model.ClaimExpiresAt,
		PublishedAt: model.PublishedAt, DeadLetteredAt: model.DeadLetteredAt, LastError: model.LastError,
	}
}

func expectClaimed(result *gorm.DB, operation string) error {
	if result.Error != nil {
		return fmt.Errorf("%s outbox message: %w", operation, result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("%s outbox message: claim is no longer owned", operation)
	}
	return nil
}

func errorSummary(err error) string {
	if err == nil {
		return "publisher failed without an error"
	}
	value := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(err.Error()))
	value = strings.Join(strings.Fields(value), " ")
	value = sensitiveAssignment.ReplaceAllString(value, "$1=[REDACTED]")
	value = bearerCredential.ReplaceAllString(value, "Bearer [REDACTED]")
	value = urlQuery.ReplaceAllStringFunc(value, func(raw string) string {
		return strings.SplitN(raw, "?", 2)[0] + "?[REDACTED]"
	})
	value = highRiskToken.ReplaceAllString(value, "[REDACTED]")
	if len(value) > maxErrorSummaryBytes {
		value = value[:maxErrorSummaryBytes]
	}
	return value
}

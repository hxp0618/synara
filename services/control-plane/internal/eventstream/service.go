package eventstream

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type Config struct {
	InstanceID              string
	LeaseTTL                time.Duration
	MaxConnectionsPerUser   int
	MaxConnectionsPerTenant int
	Now                     func() time.Time
}

type Lease struct {
	ID        uuid.UUID
	ExpiresAt time.Time
}

type Service struct {
	db                      *gorm.DB
	instanceID              string
	leaseTTL                time.Duration
	maxConnectionsPerUser   int
	maxConnectionsPerTenant int
	now                     func() time.Time
}

func New(db *gorm.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("event stream database is required")
	}
	instanceID := strings.TrimSpace(cfg.InstanceID)
	if instanceID == "" || len(instanceID) > 160 || strings.ContainsAny(instanceID, "\r\n\t") {
		return nil, errors.New("event stream instance id is invalid")
	}
	if cfg.LeaseTTL <= 0 {
		return nil, errors.New("event stream lease TTL must be positive")
	}
	if cfg.MaxConnectionsPerUser <= 0 || cfg.MaxConnectionsPerTenant <= 0 || cfg.MaxConnectionsPerUser > cfg.MaxConnectionsPerTenant {
		return nil, errors.New("event stream connection limits are invalid")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		db: db, instanceID: instanceID, leaseTTL: cfg.LeaseTTL,
		maxConnectionsPerUser:   cfg.MaxConnectionsPerUser,
		maxConnectionsPerTenant: cfg.MaxConnectionsPerTenant, now: now,
	}, nil
}

func (s *Service) Acquire(ctx context.Context, tenantID, userID, sessionID uuid.UUID) (Lease, error) {
	now := s.now().UTC()
	model := persistence.SSEConnectionLease{
		ID: uuid.New(), TenantID: tenantID, UserID: userID, SessionID: sessionID,
		InstanceID: s.instanceID, ConnectedAt: now, RenewedAt: now, ExpiresAt: now.Add(s.leaseTTL),
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		// Every acquisition for the same Tenant locks the same authoritative row first.
		// This keeps both Tenant and User limit checks exact across replicas without a
		// long-lived transaction or a process-local counter.
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
			return problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		if err := tx.WithContext(ctx).Where("tenant_id = ? AND expires_at <= ?", tenantID, now).
			Delete(&persistence.SSEConnectionLease{}).Error; err != nil {
			return problem.Wrap(500, "sse_lease_cleanup_failed", "Expired event stream leases could not be cleaned up.", err)
		}
		var tenantConnections int64
		if err := tx.WithContext(ctx).Model(&persistence.SSEConnectionLease{}).
			Where("tenant_id = ? AND expires_at > ?", tenantID, now).Count(&tenantConnections).Error; err != nil {
			return problem.Wrap(500, "sse_connection_count_failed", "Event stream connections could not be counted.", err)
		}
		if tenantConnections >= int64(s.maxConnectionsPerTenant) {
			return problem.New(429, "sse_tenant_connection_limit", "The active tenant has reached its event stream connection limit.")
		}
		var userConnections int64
		if err := tx.WithContext(ctx).Model(&persistence.SSEConnectionLease{}).
			Where("tenant_id = ? AND user_id = ? AND expires_at > ?", tenantID, userID, now).Count(&userConnections).Error; err != nil {
			return problem.Wrap(500, "sse_connection_count_failed", "Event stream connections could not be counted.", err)
		}
		if userConnections >= int64(s.maxConnectionsPerUser) {
			return problem.New(429, "sse_user_connection_limit", "The current user has reached the event stream connection limit.")
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "sse_lease_create_failed", "The event stream connection lease could not be created.", err)
		}
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return Lease{ID: model.ID, ExpiresAt: model.ExpiresAt}, nil
}

func (s *Service) Renew(ctx context.Context, leaseID uuid.UUID) (Lease, error) {
	now := s.now().UTC()
	expiresAt := now.Add(s.leaseTTL)
	result := s.db.WithContext(ctx).Model(&persistence.SSEConnectionLease{}).
		Where("id = ? AND instance_id = ? AND expires_at > ?", leaseID, s.instanceID, now).
		Updates(map[string]any{"renewed_at": now, "expires_at": expiresAt})
	if result.Error != nil {
		return Lease{}, problem.Wrap(500, "sse_lease_renew_failed", "The event stream connection lease could not be renewed.", result.Error)
	}
	if result.RowsAffected != 1 {
		return Lease{}, problem.New(409, "sse_lease_expired", "The event stream connection lease has expired.")
	}
	return Lease{ID: leaseID, ExpiresAt: expiresAt}, nil
}

func (s *Service) Release(ctx context.Context, leaseID uuid.UUID) error {
	result := s.db.WithContext(ctx).
		Where("id = ? AND instance_id = ?", leaseID, s.instanceID).
		Delete(&persistence.SSEConnectionLease{})
	if result.Error != nil {
		return problem.Wrap(500, "sse_lease_release_failed", "The event stream connection lease could not be released.", result.Error)
	}
	return nil
}

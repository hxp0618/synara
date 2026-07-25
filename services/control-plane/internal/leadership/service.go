package leadership

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const maxIdentifierLength = 160

var errAcquireRetry = errors.New("leadership acquire retry")

var ErrFenceInactive = errors.New("leadership fence is not active")

type Config struct {
	// HolderID must uniquely identify one process incarnation. Reusing the same
	// holder id across restarts weakens fencing semantics.
	HolderID string
	LeaseTTL time.Duration
}

type Lease struct {
	Name         string
	HolderID     string
	FencingToken int64
	AcquiredAt   time.Time
	RenewedAt    time.Time
	ExpiresAt    time.Time
}

type Service struct {
	db       *gorm.DB
	holderID string
	leaseTTL time.Duration
}

func New(db *gorm.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("leadership database is required")
	}
	if err := persistence.InstallTransactionWriteFence(db); err != nil {
		return nil, fmt.Errorf("install leadership write fence: %w", err)
	}
	holderID, err := normalizeIdentifier(cfg.HolderID, "leadership holder id")
	if err != nil {
		return nil, err
	}
	if cfg.LeaseTTL <= 0 {
		return nil, errors.New("leadership lease TTL must be positive")
	}
	return &Service{db: db, holderID: holderID, leaseTTL: cfg.LeaseTTL}, nil
}

// WithFence attaches this exact leadership epoch to authoritative GORM
// mutations. Create, update, and delete callbacks verify and lock the lease row
// in the same transaction as the write, closing the paused-old-leader gap.
func (s *Service) WithFence(ctx context.Context, lease Lease) context.Context {
	if s == nil {
		return ctx
	}
	return persistence.WithTransactionWriteFence(ctx, func(fenceCtx context.Context, tx *gorm.DB) error {
		_, err := AssertFenceInTransaction(
			fenceCtx,
			tx,
			lease.Name,
			s.holderID,
			lease.FencingToken,
		)
		return err
	})
}

func (s *Service) LeaseTTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.leaseTTL
}

func (s *Service) HolderID() string {
	if s == nil {
		return ""
	}
	return s.holderID
}

func ValidateLeaseName(value string) (string, error) {
	return normalizeIdentifier(value, "leadership lease name")
}

// AssertFenceInTransaction locks the lease epoch until the caller's database
// transaction commits. This closes the gap between a pre-sweep assertion and
// a fenced state mutation: a takeover cannot advance the token concurrently.
func AssertFenceInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	leaseName, holderID string,
	fencingToken int64,
) (Lease, error) {
	if tx == nil {
		return Lease{}, errors.New("leadership transaction is required")
	}
	name, err := normalizeIdentifier(leaseName, "leadership lease name")
	if err != nil {
		return Lease{}, err
	}
	holder, err := normalizeIdentifier(holderID, "leadership holder id")
	if err != nil {
		return Lease{}, err
	}
	if fencingToken <= 0 {
		return Lease{}, errors.New("leadership fencing token must be positive")
	}
	current, err := loadLeaseForUpdate(ctx, tx, name)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Lease{}, ErrFenceInactive
	}
	if err != nil {
		return Lease{}, fmt.Errorf("load leadership lease %q: %w", name, err)
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return Lease{}, err
	}
	lease := leaseFromModel(current)
	if current.HolderID != holder || current.FencingToken != fencingToken || !current.ExpiresAt.After(now) {
		return lease, ErrFenceInactive
	}
	return lease, nil
}

func (s *Service) Acquire(ctx context.Context, leaseName string) (Lease, bool, error) {
	name, err := normalizeIdentifier(leaseName, "leadership lease name")
	if err != nil {
		return Lease{}, false, err
	}

	type result struct {
		lease Lease
		held  bool
	}
	outcome := result{}
	for attempts := 0; attempts < 3; attempts++ {
		err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
			model, created, err := s.tryCreateLease(ctx, tx, name)
			if err != nil {
				return err
			}
			if created {
				outcome.lease = leaseFromModel(model)
				outcome.held = true
				return nil
			}

			current, err := loadLeaseForUpdate(ctx, tx, name)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errAcquireRetry
			}
			if err != nil {
				return fmt.Errorf("load leadership lease %q: %w", name, err)
			}

			now, err := currentTime(ctx, tx)
			if err != nil {
				return err
			}
			if !current.ExpiresAt.After(now) {
				// The matching controller cycle holds this same session-level
				// advisory lock while it can perform non-database side effects.
				// Refuse lease takeover until that in-flight cycle releases it.
				acquired, lockErr := persistence.TryTransactionAdvisoryLock(ctx, tx, name)
				if lockErr != nil {
					return fmt.Errorf("acquire leadership takeover guard %q: %w", name, lockErr)
				}
				if !acquired {
					outcome.lease = leaseFromModel(current)
					outcome.held = false
					return nil
				}
				current.HolderID = s.holderID
				current.FencingToken++
				current.AcquiredAt = now
				current.RenewedAt = now
				current.ExpiresAt = now.Add(s.leaseTTL)
				if err := updateLease(ctx, tx, current); err != nil {
					return err
				}
				outcome.lease = leaseFromModel(current)
				outcome.held = true
				return nil
			}
			if current.HolderID == s.holderID {
				current.RenewedAt = now
				current.ExpiresAt = now.Add(s.leaseTTL)
				if err := updateLease(ctx, tx, current); err != nil {
					return err
				}
				outcome.lease = leaseFromModel(current)
				outcome.held = true
				return nil
			}

			outcome.lease = leaseFromModel(current)
			outcome.held = false
			return nil
		})
		if err == nil {
			return outcome.lease, outcome.held, nil
		}
		if !errors.Is(err, errAcquireRetry) {
			return Lease{}, false, err
		}
	}
	return Lease{}, false, fmt.Errorf("acquire leadership lease %q: retry budget exhausted", name)
}

func (s *Service) Renew(ctx context.Context, leaseName string, fencingToken int64) (Lease, bool, error) {
	name, err := normalizeIdentifier(leaseName, "leadership lease name")
	if err != nil {
		return Lease{}, false, err
	}
	if fencingToken <= 0 {
		return Lease{}, false, errors.New("leadership fencing token must be positive")
	}

	var lease Lease
	var held bool
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		current, err := loadLeaseForUpdate(ctx, tx, name)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			lease = Lease{}
			held = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("load leadership lease %q: %w", name, err)
		}
		now, err := currentTime(ctx, tx)
		if err != nil {
			return err
		}

		lease = leaseFromModel(current)
		if current.HolderID != s.holderID || current.FencingToken != fencingToken || !current.ExpiresAt.After(now) {
			held = false
			return nil
		}

		current.RenewedAt = now
		current.ExpiresAt = now.Add(s.leaseTTL)
		if err := updateLease(ctx, tx, current); err != nil {
			return err
		}
		lease = leaseFromModel(current)
		held = true
		return nil
	})
	if err != nil {
		return Lease{}, false, err
	}
	return lease, held, nil
}

func (s *Service) Release(ctx context.Context, leaseName string, fencingToken int64) (bool, error) {
	name, err := normalizeIdentifier(leaseName, "leadership lease name")
	if err != nil {
		return false, err
	}
	if fencingToken <= 0 {
		return false, errors.New("leadership fencing token must be positive")
	}

	var released bool
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		current, err := loadLeaseForUpdate(ctx, tx, name)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			released = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("load leadership lease %q: %w", name, err)
		}
		now, err := currentTime(ctx, tx)
		if err != nil {
			return err
		}
		if current.HolderID != s.holderID || current.FencingToken != fencingToken || !current.ExpiresAt.After(now) {
			released = false
			return nil
		}

		current.RenewedAt = now
		current.ExpiresAt = now
		if err := updateLease(ctx, tx, current); err != nil {
			return err
		}
		released = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

func (s *Service) AssertActive(ctx context.Context, leaseName string, fencingToken int64) (Lease, bool, error) {
	name, err := normalizeIdentifier(leaseName, "leadership lease name")
	if err != nil {
		return Lease{}, false, err
	}
	if fencingToken <= 0 {
		return Lease{}, false, errors.New("leadership fencing token must be positive")
	}

	var lease Lease
	var held bool
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		current, err := loadLeaseForUpdate(ctx, tx, name)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			lease = Lease{}
			held = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("load leadership lease %q: %w", name, err)
		}
		now, err := currentTime(ctx, tx)
		if err != nil {
			return err
		}
		lease = leaseFromModel(current)
		held = current.HolderID == s.holderID && current.FencingToken == fencingToken && current.ExpiresAt.After(now)
		return nil
	})
	if err != nil {
		return Lease{}, false, err
	}
	return lease, held, nil
}

func (s *Service) tryCreateLease(
	ctx context.Context,
	tx *gorm.DB,
	leaseName string,
) (persistence.ReconcilerLease, bool, error) {
	now, err := currentTime(ctx, tx)
	if err != nil {
		return persistence.ReconcilerLease{}, false, err
	}
	model := persistence.ReconcilerLease{
		LeaseName:    leaseName,
		HolderID:     s.holderID,
		FencingToken: 1,
		AcquiredAt:   now,
		RenewedAt:    now,
		ExpiresAt:    now.Add(s.leaseTTL),
	}
	result := tx.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model)
	if result.Error != nil {
		return persistence.ReconcilerLease{}, false, fmt.Errorf("create leadership lease %q: %w", leaseName, result.Error)
	}
	return model, result.RowsAffected == 1, nil
}

func loadLeaseForUpdate(ctx context.Context, tx *gorm.DB, leaseName string) (persistence.ReconcilerLease, error) {
	var model persistence.ReconcilerLease
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Table("reconciler_leases").
		Model(&persistence.ReconcilerLease{}).
		Where("lease_name = ?", leaseName).
		Take(&model).Error
	return model, err
}

func updateLease(ctx context.Context, tx *gorm.DB, model persistence.ReconcilerLease) error {
	result := tx.WithContext(ctx).Model(&persistence.ReconcilerLease{}).
		Where("lease_name = ?", model.LeaseName).
		Updates(map[string]any{
			"holder_id":     model.HolderID,
			"fencing_token": model.FencingToken,
			"acquired_at":   model.AcquiredAt,
			"renewed_at":    model.RenewedAt,
			"expires_at":    model.ExpiresAt,
		})
	if result.Error != nil {
		return fmt.Errorf("update leadership lease %q: %w", model.LeaseName, result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("update leadership lease %q: concurrent mutation", model.LeaseName)
	}
	return nil
}

func currentTime(ctx context.Context, tx *gorm.DB) (time.Time, error) {
	switch tx.Dialector.Name() {
	case "postgres":
		var now time.Time
		if err := tx.WithContext(ctx).Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return time.Time{}, fmt.Errorf("read leadership time from PostgreSQL: %w", err)
		}
		return now.UTC(), nil
	case "sqlite":
		var raw struct {
			Value string `gorm:"column:value"`
		}
		if err := tx.WithContext(ctx).
			Raw("SELECT STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now') AS value").
			Scan(&raw).Error; err != nil {
			return time.Time{}, fmt.Errorf("read leadership time from SQLite: %w", err)
		}
		now, err := time.Parse("2006-01-02T15:04:05.000Z", raw.Value)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse leadership time from SQLite: %w", err)
		}
		return now.UTC(), nil
	default:
		var now time.Time
		if err := tx.WithContext(ctx).Raw("SELECT CURRENT_TIMESTAMP").Scan(&now).Error; err != nil {
			return time.Time{}, fmt.Errorf("read leadership time from database: %w", err)
		}
		return now.UTC(), nil
	}
}

func leaseFromModel(model persistence.ReconcilerLease) Lease {
	return Lease{
		Name:         model.LeaseName,
		HolderID:     model.HolderID,
		FencingToken: model.FencingToken,
		AcquiredAt:   model.AcquiredAt.UTC(),
		RenewedAt:    model.RenewedAt.UTC(),
		ExpiresAt:    model.ExpiresAt.UTC(),
	}
}

func normalizeIdentifier(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "", fmt.Errorf("%s is required", label)
	case len(value) > maxIdentifierLength:
		return "", fmt.Errorf("%s must be at most %d characters", label, maxIdentifierLength)
	case strings.ContainsAny(value, "\r\n\t"):
		return "", fmt.Errorf("%s is invalid", label)
	default:
		return value, nil
	}
}

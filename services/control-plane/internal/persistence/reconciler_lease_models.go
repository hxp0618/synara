package persistence

import "time"

type ReconcilerLease struct {
	LeaseName    string    `gorm:"column:lease_name;primaryKey;index:idx_reconciler_leases_expiry,priority:2"`
	HolderID     string    `gorm:"column:holder_id;not null"`
	FencingToken int64     `gorm:"column:fencing_token;not null"`
	AcquiredAt   time.Time `gorm:"column:acquired_at;not null"`
	RenewedAt    time.Time `gorm:"column:renewed_at;not null"`
	ExpiresAt    time.Time `gorm:"column:expires_at;not null;index:idx_reconciler_leases_expiry,priority:1"`
}

func (ReconcilerLease) TableName() string { return "reconciler_leases" }

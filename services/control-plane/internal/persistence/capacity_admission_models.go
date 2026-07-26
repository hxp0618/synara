package persistence

import (
	"time"

	"github.com/google/uuid"
)

// ExecutionCapacityAdmission freezes the capacity authority evaluated just
// before an Execution is inserted. It is immutable evidence separate from the
// routing candidate graph because fixed Targets use the same admission path.
type ExecutionCapacityAdmission struct {
	TenantID                          uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ExecutionID                       uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey"`
	ExecutionTargetID                 uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null"`
	AdmissionMode                     string     `gorm:"column:admission_mode;not null"`
	HealthVersion                     *int64     `gorm:"column:health_version"`
	HealthSource                      *string    `gorm:"column:health_source"`
	HealthObservedAt                  *time.Time `gorm:"column:health_observed_at"`
	HealthExpiresAt                   *time.Time `gorm:"column:health_expires_at"`
	CapacityStatus                    *string    `gorm:"column:capacity_status"`
	CapacityCeilingUnits              *int       `gorm:"column:capacity_ceiling_units"`
	AllocatedCapacityUnits            *int       `gorm:"column:allocated_capacity_units"`
	ReservationAuthorityMode          *string    `gorm:"column:reservation_authority_mode"`
	ReservationAcknowledgedUnits      *int       `gorm:"column:reservation_acknowledged_units"`
	ReservationAcknowledgementsSHA256 *string    `gorm:"column:reservation_acknowledgements_sha256"`
	ActiveReservationUnits            int64      `gorm:"column:active_reservation_units;not null"`
	UnacknowledgedReservationUnits    *int64     `gorm:"column:unacknowledged_reservation_units"`
	StrictCapacityUsedUnits           *int64     `gorm:"column:strict_capacity_used_units"`
	AdmittedAt                        time.Time  `gorm:"column:admitted_at;not null"`
	SnapshotSHA256                    string     `gorm:"column:snapshot_sha256;not null"`
}

func (ExecutionCapacityAdmission) TableName() string {
	return "execution_capacity_admissions"
}

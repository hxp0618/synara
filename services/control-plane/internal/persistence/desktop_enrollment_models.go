package persistence

import (
	"time"

	"github.com/google/uuid"
)

type DesktopDevice struct {
	ID                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ControlPlaneOrigin    string     `gorm:"column:control_plane_origin"`
	UserID                uuid.UUID  `gorm:"column:user_id;type:uuid"`
	DefaultTenantID       uuid.UUID  `gorm:"column:default_tenant_id;type:uuid"`
	DefaultOrganizationID *uuid.UUID `gorm:"column:default_organization_id;type:uuid"`
	PublicKey             []byte     `gorm:"column:public_key"`
	PublicKeySHA256       []byte     `gorm:"column:public_key_sha256"`
	Platform              string     `gorm:"column:platform"`
	AppVersion            string     `gorm:"column:app_version"`
	DeviceLabel           string     `gorm:"column:device_label"`
	Status                string     `gorm:"column:status"`
	Version               int64      `gorm:"column:version;not null;default:1"`
	LastSeenAt            time.Time  `gorm:"column:last_seen_at"`
	RevokedBy             *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
	RevocationReason      *string    `gorm:"column:revocation_reason"`
	RevokedAt             *time.Time `gorm:"column:revoked_at"`
	CreatedAt             time.Time  `gorm:"column:created_at"`
	UpdatedAt             time.Time  `gorm:"column:updated_at"`
}

func (DesktopDevice) TableName() string { return "desktop_devices" }

type DesktopEnrollment struct {
	ID                        uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	SecretHash                []byte     `gorm:"column:secret_hash"`
	Status                    string     `gorm:"column:status"`
	Version                   int64      `gorm:"column:version;not null;default:1"`
	Mode                      string     `gorm:"column:mode"`
	Authority                 string     `gorm:"column:authority"`
	ControlPlaneOrigin        string     `gorm:"column:control_plane_origin"`
	IssuedByUserID            uuid.UUID  `gorm:"column:issued_by_user_id;type:uuid"`
	IssuedBySessionID         uuid.UUID  `gorm:"column:issued_by_session_id;type:uuid"`
	SubjectUserID             uuid.UUID  `gorm:"column:subject_user_id;type:uuid"`
	TenantID                  uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID            *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	MembershipVersionSnapshot string     `gorm:"column:membership_version_snapshot"`
	Reason                    string     `gorm:"column:reason"`
	ExpiresAt                 time.Time  `gorm:"column:expires_at"`
	OpenedAt                  *time.Time `gorm:"column:opened_at"`
	RedeemedAt                *time.Time `gorm:"column:redeemed_at"`
	RedeemedDeviceID          *uuid.UUID `gorm:"column:redeemed_device_id;type:uuid"`
	RedeemedNonceHash         []byte     `gorm:"column:redeemed_nonce_hash"`
	RevokedBy                 *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
	RevokedAt                 *time.Time `gorm:"column:revoked_at"`
	TerminalReason            *string    `gorm:"column:terminal_reason"`
	FailedAttempts            int        `gorm:"column:failed_attempts"`
	LastFailureAt             *time.Time `gorm:"column:last_failure_at"`
	CreatedAt                 time.Time  `gorm:"column:created_at"`
	UpdatedAt                 time.Time  `gorm:"column:updated_at"`
}

func (DesktopEnrollment) TableName() string { return "desktop_enrollments" }

package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ProviderCredential struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID   *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	Name             string     `gorm:"column:name"`
	Purpose          string     `gorm:"column:purpose;default:provider"`
	Provider         string     `gorm:"column:provider"`
	CredentialType   string     `gorm:"column:credential_type"`
	EncryptedPayload []byte     `gorm:"column:encrypted_payload"`
	EncryptedDataKey []byte     `gorm:"column:encrypted_data_key"`
	KMSProvider      string     `gorm:"column:kms_provider"`
	KMSKeyID         string     `gorm:"column:kms_key_id"`
	Version          int        `gorm:"column:version"`
	CreatedBy        uuid.UUID  `gorm:"column:created_by;type:uuid"`
	UpdatedBy        uuid.UUID  `gorm:"column:updated_by;type:uuid"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
	ExpiresAt        *time.Time `gorm:"column:expires_at"`
	RevokedAt        *time.Time `gorm:"column:revoked_at"`
	RevokedBy        *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
}

func (ProviderCredential) TableName() string { return "provider_credentials" }

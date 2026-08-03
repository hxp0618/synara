package persistence

import (
	"time"

	"github.com/google/uuid"
)

// KMSRewrapRun records the immutable intent of one online envelope-key
// rotation. Completion is represented separately by KMSRewrapReceipt so a
// missing receipt is an unambiguous interrupted run that can be resumed.
type KMSRewrapRun struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	PrimaryProvider   string    `gorm:"column:primary_provider"`
	PrimaryKeyID      string    `gorm:"column:primary_key_id"`
	OperatorReference string    `gorm:"column:operator_reference"`
	StartedAt         time.Time `gorm:"column:started_at"`
}

func (KMSRewrapRun) TableName() string { return "kms_rewrap_runs" }

// KMSRewrapEntry is both the authorization token for one envelope-only row
// update and its durable audit evidence. It deliberately stores wrapped data
// keys, never plaintext data keys or decrypted business payloads.
type KMSRewrapEntry struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	RunID               uuid.UUID `gorm:"column:run_id;type:uuid"`
	TenantID            uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	ResourceType        string    `gorm:"column:resource_type"`
	ResourceID          uuid.UUID `gorm:"column:resource_id;type:uuid"`
	OldKMSProvider      string    `gorm:"column:old_kms_provider"`
	OldKMSKeyID         string    `gorm:"column:old_kms_key_id"`
	OldEncryptedDataKey []byte    `gorm:"column:old_encrypted_data_key"`
	NewKMSProvider      string    `gorm:"column:new_kms_provider"`
	NewKMSKeyID         string    `gorm:"column:new_kms_key_id"`
	NewEncryptedDataKey []byte    `gorm:"column:new_encrypted_data_key"`
	CreatedAt           time.Time `gorm:"column:created_at"`
}

func (KMSRewrapEntry) TableName() string { return "kms_rewrap_entries" }

type KMSRewrapReceipt struct {
	RunID                     uuid.UUID `gorm:"column:run_id;type:uuid;primaryKey"`
	ProviderCredentialCount   int64     `gorm:"column:provider_credential_count"`
	IdentityConnectionCount   int64     `gorm:"column:identity_connection_count"`
	IdentityLoginAttemptCount int64     `gorm:"column:identity_login_attempt_count"`
	EntryDigest               []byte    `gorm:"column:entry_digest"`
	CompletedAt               time.Time `gorm:"column:completed_at"`
}

func (KMSRewrapReceipt) TableName() string { return "kms_rewrap_receipts" }

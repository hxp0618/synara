package persistence

import (
	"time"

	"github.com/google/uuid"
)

type RuntimeSecretRekeyRun struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	PrimaryKeyID      string    `gorm:"column:primary_key_id"`
	OperatorReference string    `gorm:"column:operator_reference"`
	StartedAt         time.Time `gorm:"column:started_at"`
}

func (RuntimeSecretRekeyRun) TableName() string { return "runtime_secret_rekey_runs" }

type RuntimeSecretRekeyEntry struct {
	ID                  uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	RunID               uuid.UUID  `gorm:"column:run_id;type:uuid"`
	TenantID            *uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	ResourceType        string     `gorm:"column:resource_type"`
	ResourceID          uuid.UUID  `gorm:"column:resource_id;type:uuid"`
	OldStoredKeyID      *string    `gorm:"column:old_stored_key_id"`
	DecryptKeyID        string     `gorm:"column:decrypt_key_id"`
	NewKeyID            string     `gorm:"column:new_key_id"`
	OldCiphertextSHA256 []byte     `gorm:"column:old_ciphertext_sha256"`
	NewCiphertextSHA256 []byte     `gorm:"column:new_ciphertext_sha256"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
}

func (RuntimeSecretRekeyEntry) TableName() string { return "runtime_secret_rekey_entries" }

type RuntimeSecretRekeyReceipt struct {
	RunID                     uuid.UUID `gorm:"column:run_id;type:uuid;primaryKey"`
	ExecutionTargetCount      int64     `gorm:"column:execution_target_count"`
	ProviderResumeCursorCount int64     `gorm:"column:provider_resume_cursor_count"`
	EntryDigest               []byte    `gorm:"column:entry_digest"`
	CompletedAt               time.Time `gorm:"column:completed_at"`
}

func (RuntimeSecretRekeyReceipt) TableName() string { return "runtime_secret_rekey_receipts" }

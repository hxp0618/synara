package persistence

import (
	"time"

	"github.com/google/uuid"
)

type Artifact struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID  uuid.UUID  `gorm:"column:organization_id;type:uuid"`
	ProjectID       uuid.UUID  `gorm:"column:project_id;type:uuid"`
	SessionID       uuid.UUID  `gorm:"column:session_id;type:uuid"`
	ExecutionID     *uuid.UUID `gorm:"column:execution_id;type:uuid"`
	Kind            string     `gorm:"column:kind"`
	Status          string     `gorm:"column:status"`
	OriginalName    *string    `gorm:"column:original_name"`
	Bucket          string     `gorm:"column:bucket"`
	ObjectKey       string     `gorm:"column:object_key"`
	UploadObjectKey *string    `gorm:"column:upload_object_key"`
	ObjectVersion   *string    `gorm:"column:object_version"`
	ContentType     *string    `gorm:"column:content_type"`
	SizeBytes       *int64     `gorm:"column:size_bytes"`
	SHA256          *string    `gorm:"column:sha256"`
	EncryptionKeyID *string    `gorm:"column:encryption_key_id"`
	CreatedByType   string     `gorm:"column:created_by_type"`
	CreatedByID     uuid.UUID  `gorm:"column:created_by_id;type:uuid"`
	UploadTokenHash []byte     `gorm:"column:upload_token_hash"`
	UploadExpiresAt *time.Time `gorm:"column:upload_expires_at"`
	ReadyAt         *time.Time `gorm:"column:ready_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	ExpiresAt       *time.Time `gorm:"column:expires_at"`
	DeletedAt       *time.Time `gorm:"column:deleted_at"`
}

func (Artifact) TableName() string { return "artifacts" }

type ArtifactPayloadMigration struct {
	ArtifactID    uuid.UUID `gorm:"column:artifact_id;type:uuid;primaryKey"`
	Destination   string    `gorm:"column:destination;primaryKey"`
	SourceSHA256  string    `gorm:"column:source_sha256"`
	ObjectVersion *string   `gorm:"column:object_version"`
	MigratedAt    time.Time `gorm:"column:migrated_at"`
}

func (ArtifactPayloadMigration) TableName() string { return "artifact_payload_migrations" }

type ArtifactAccessToken struct {
	ID         uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ArtifactID uuid.UUID `gorm:"column:artifact_id;type:uuid"`
	TokenHash  []byte    `gorm:"column:token_hash"`
	Purpose    string    `gorm:"column:purpose"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	ExpiresAt  time.Time `gorm:"column:expires_at"`
}

func (ArtifactAccessToken) TableName() string { return "artifact_access_tokens" }

package persistence

import (
	"time"

	"github.com/google/uuid"
)

type TenantDataExport struct {
	ID            uuid.UUID      `gorm:"column:id;type:uuid;primaryKey" json:"id"`
	TenantID      uuid.UUID      `gorm:"column:tenant_id;type:uuid" json:"tenantId"`
	RequestedBy   uuid.UUID      `gorm:"column:requested_by;type:uuid" json:"requestedBy"`
	SchemaVersion string         `gorm:"column:schema_version" json:"schemaVersion"`
	DigestSHA256  string         `gorm:"column:digest_sha256" json:"digestSha256"`
	ByteCount     int64          `gorm:"column:byte_count" json:"byteCount"`
	RowCounts     map[string]any `gorm:"column:row_counts;serializer:json" json:"rowCounts"`
	CreatedAt     time.Time      `gorm:"column:created_at" json:"createdAt"`
}

func (TenantDataExport) TableName() string { return "tenant_data_exports" }

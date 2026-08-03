package persistence

import (
	"time"

	"github.com/google/uuid"
)

type PrivacyRequest struct {
	ID                   uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID             uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	SubjectUserID        uuid.UUID      `gorm:"column:subject_user_id;type:uuid"`
	RequestType          string         `gorm:"column:request_type"`
	Status               string         `gorm:"column:status"`
	Version              int64          `gorm:"column:version;not null;default:1"`
	RequestedBy          uuid.UUID      `gorm:"column:requested_by;type:uuid"`
	IntakeReason         string         `gorm:"column:intake_reason"`
	DueAt                time.Time      `gorm:"column:due_at"`
	LastTransitionBy     uuid.UUID      `gorm:"column:last_transition_by;type:uuid"`
	LastTransitionReason string         `gorm:"column:last_transition_reason"`
	CompletedAt          *time.Time     `gorm:"column:completed_at"`
	ResultSummary        map[string]any `gorm:"column:result_summary;serializer:json"`
	ResultDigestSHA256   *string        `gorm:"column:result_digest_sha256"`
	CreatedAt            time.Time      `gorm:"column:created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at"`
}

func (PrivacyRequest) TableName() string { return "privacy_requests" }

type PrivacyRequestEvent struct {
	ID               uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID         uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	PrivacyRequestID uuid.UUID      `gorm:"column:privacy_request_id;type:uuid"`
	Version          int64          `gorm:"column:version"`
	FromStatus       *string        `gorm:"column:from_status"`
	ToStatus         string         `gorm:"column:to_status"`
	ActorUserID      uuid.UUID      `gorm:"column:actor_user_id;type:uuid"`
	Reason           string         `gorm:"column:reason"`
	Metadata         map[string]any `gorm:"column:metadata;serializer:json"`
	OccurredAt       time.Time      `gorm:"column:occurred_at"`
}

func (PrivacyRequestEvent) TableName() string { return "privacy_request_events" }

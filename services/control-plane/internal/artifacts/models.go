package artifacts

import (
	"time"

	"github.com/google/uuid"
)

type Artifact struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenantId"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	ProjectID      uuid.UUID  `json:"projectId"`
	SessionID      uuid.UUID  `json:"sessionId"`
	ExecutionID    *uuid.UUID `json:"executionId"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	OriginalName   *string    `json:"originalName"`
	ContentType    *string    `json:"contentType"`
	SizeBytes      *int64     `json:"sizeBytes"`
	SHA256         *string    `json:"sha256"`
	CreatedByType  string     `json:"createdByType"`
	CreatedByID    uuid.UUID  `json:"createdById"`
	ReadyAt        *time.Time `json:"readyAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      *time.Time `json:"expiresAt"`
	DeletedAt      *time.Time `json:"deletedAt"`
}

type CreateInput struct {
	Kind         string     `json:"kind"`
	OriginalName *string    `json:"originalName"`
	ExecutionID  *uuid.UUID `json:"executionId"`
	ExpiresAt    *time.Time `json:"expiresAt"`
}

type CompleteInput struct {
	SizeBytes   int64  `json:"sizeBytes"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"contentType"`
}

type WorkerCreateInput struct {
	TenantID     uuid.UUID  `json:"tenantId"`
	Generation   int64      `json:"generation"`
	LeaseToken   string     `json:"leaseToken"`
	CheckpointID *uuid.UUID `json:"checkpointId,omitempty"`
	Kind         string     `json:"kind"`
	OriginalName *string    `json:"originalName"`
	ExpiresAt    *time.Time `json:"expiresAt"`
}

type WorkerCompleteInput struct {
	TenantID   uuid.UUID `json:"tenantId"`
	Generation int64     `json:"generation"`
	LeaseToken string    `json:"leaseToken"`
	CompleteInput
}

type UploadGrant struct {
	Artifact       Artifact          `json:"artifact"`
	UploadRequired bool              `json:"uploadRequired"`
	Method         string            `json:"method,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	ExpiresAt      time.Time         `json:"expiresAt,omitempty"`
}

type DownloadGrant struct {
	Artifact  Artifact  `json:"artifact"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

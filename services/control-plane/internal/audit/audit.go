package audit

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

type Entry struct {
	TenantID       uuid.UUID
	ActorType      string
	ActorID        *uuid.UUID
	Action         string
	ResourceType   string
	ResourceID     *uuid.UUID
	OrganizationID *uuid.UUID
	RequestID      string
	IPAddress      string
	Metadata       map[string]any
}

func Record(ctx context.Context, tx *gorm.DB, entry Entry) error {
	metadata := entry.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	var ipAddress *string
	if entry.IPAddress != "" {
		ipAddress = &entry.IPAddress
	}
	return tx.WithContext(ctx).Create(&persistence.AuditLog{
		EventID: uuid.New(), TenantID: entry.TenantID, ActorType: entry.ActorType,
		ActorID: entry.ActorID, Action: entry.Action, ResourceType: entry.ResourceType,
		ResourceID: entry.ResourceID, OrganizationID: entry.OrganizationID,
		RequestID: entry.RequestID, IPAddress: ipAddress, Metadata: metadata,
		OccurredAt: time.Now().UTC(),
	}).Error
}

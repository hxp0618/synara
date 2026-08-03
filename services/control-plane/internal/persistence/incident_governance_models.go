package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6Incident is the Platform authority for an incident's operational
// identity, assigned roles, broad internal component scope and lifecycle. The
// internal Status Board remains the employee-facing communication authority.
type Stage6Incident struct {
	ID                                   uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID                     uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	IncidentKey                          string     `gorm:"column:incident_key;not null;uniqueIndex"`
	Severity                             string     `gorm:"column:severity;not null"`
	State                                string     `gorm:"column:state;not null"`
	Title                                string     `gorm:"column:title;not null"`
	InternalImpactSummary                string     `gorm:"column:customer_impact_summary;not null"`
	BroadInternalImpact                  bool       `gorm:"column:public_impact;not null"`
	SecurityPrivacyImpact                bool       `gorm:"column:security_privacy_impact;not null"`
	InternalStatusBoardOrigin            *string    `gorm:"column:status_page_origin"`
	InternalStatusBoardIncidentReference *string    `gorm:"column:status_page_incident_reference"`
	AffectedComponents                   []string   `gorm:"column:affected_components;serializer:json;not null;default:'[]'"`
	AffectedRegions                      []string   `gorm:"column:affected_regions;serializer:json;not null;default:'[]'"`
	IncidentCommanderUserID              uuid.UUID  `gorm:"column:incident_commander_user_id;type:uuid;not null"`
	CommunicationsLeadUserID             uuid.UUID  `gorm:"column:communications_lead_user_id;type:uuid;not null"`
	SecurityPrivacyLeadUserID            *uuid.UUID `gorm:"column:security_privacy_lead_user_id;type:uuid"`
	StartedAt                            time.Time  `gorm:"column:started_at;not null"`
	ImpactConfirmedAt                    time.Time  `gorm:"column:impact_confirmed_at;not null"`
	FirstInternalUpdateDueAt             *time.Time `gorm:"column:first_public_update_due_at"`
	NextInternalUpdateDueAt              *time.Time `gorm:"column:next_public_update_due_at"`
	ResolvedAt                           *time.Time `gorm:"column:resolved_at"`
	CancelledAt                          *time.Time `gorm:"column:cancelled_at"`
	Version                              int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                            uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	CreatedAt                            time.Time  `gorm:"column:created_at"`
	UpdatedAt                            time.Time  `gorm:"column:updated_at"`
}

func (Stage6Incident) TableName() string { return "stage6_incidents" }

// Stage6IncidentUpdate mirrors one employee-facing internal Status Board update.
// It stores no private incident log or Tenant-specific payload and is append-only.
type Stage6IncidentUpdate struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	IncidentID        uuid.UUID `gorm:"column:incident_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Kind              string    `gorm:"column:update_kind;not null"`
	Summary           string    `gorm:"column:summary;not null"`
	PublishedAt       time.Time `gorm:"column:published_at;not null"`
	EvidenceReference string    `gorm:"column:external_reference;not null"`
	CreatedBy         uuid.UUID `gorm:"column:created_by;type:uuid;not null"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (Stage6IncidentUpdate) TableName() string { return "stage6_incident_updates" }

// Stage6IncidentResolutionApproval is the immutable Security/Privacy decision
// required before resolving a security/privacy-impacting incident.
type Stage6IncidentResolutionApproval struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	IncidentID        uuid.UUID  `gorm:"column:incident_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Decision          string     `gorm:"column:decision;not null"`
	Reason            string     `gorm:"column:reason;not null"`
	EvidenceReference string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string    `gorm:"column:evidence_sha256"`
	SupersededAt      *time.Time `gorm:"column:superseded_at"`
	SupersededReason  *string    `gorm:"column:superseded_reason"`
	ApproverUserID    uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
}

func (Stage6IncidentResolutionApproval) TableName() string {
	return "stage6_incident_resolution_approvals"
}

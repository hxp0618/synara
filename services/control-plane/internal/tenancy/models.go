package tenancy

import (
	"time"

	"github.com/google/uuid"
)

type Tenant struct {
	ID        uuid.UUID      `json:"id"`
	Slug      string         `json:"slug"`
	Name      string         `json:"name"`
	Status    string         `json:"status"`
	PlanCode  string         `json:"planCode"`
	Region    string         `json:"region"`
	Settings  map[string]any `json:"settings"`
	Role      string         `json:"role"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

type TenantMember struct {
	TenantID    uuid.UUID  `json:"tenantId"`
	UserID      uuid.UUID  `json:"userId"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	JoinedAt    *time.Time `json:"joinedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type Organization struct {
	ID                   uuid.UUID      `json:"id"`
	TenantID             uuid.UUID      `json:"tenantId"`
	ParentOrganizationID *uuid.UUID     `json:"parentOrganizationId"`
	Slug                 string         `json:"slug"`
	Name                 string         `json:"name"`
	Kind                 string         `json:"kind"`
	Status               string         `json:"status"`
	Settings             map[string]any `json:"settings"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
	ArchivedAt           *time.Time     `json:"archivedAt"`
}

type OrganizationMember struct {
	TenantID       uuid.UUID `json:"tenantId"`
	OrganizationID uuid.UUID `json:"organizationId"`
	UserID         uuid.UUID `json:"userId"`
	Email          string    `json:"email"`
	DisplayName    string    `json:"displayName"`
	Role           string    `json:"role"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type CreateTenantInput struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	PlanCode string `json:"planCode"`
}

type UpdateTenantInput struct {
	Name     *string         `json:"name"`
	Status   *string         `json:"status"`
	Region   *string         `json:"region"`
	Settings *map[string]any `json:"settings"`
}

type InviteTenantMemberInput struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type Invitation struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenantId"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateTenantMemberInput struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

type CreateOrganizationInput struct {
	ParentOrganizationID *uuid.UUID     `json:"parentOrganizationId"`
	Slug                 string         `json:"slug"`
	Name                 string         `json:"name"`
	Kind                 string         `json:"kind"`
	Settings             map[string]any `json:"settings"`
}

type UpdateOrganizationInput struct {
	Name     *string         `json:"name"`
	Status   *string         `json:"status"`
	Settings *map[string]any `json:"settings"`
}

type PutOrganizationMemberInput struct {
	UserID uuid.UUID `json:"userId"`
	Role   string    `json:"role"`
	Status string    `json:"status"`
}

type UpdateOrganizationMemberInput struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

type AuditLogEntry struct {
	EventID        uuid.UUID      `json:"eventId"`
	TenantID       uuid.UUID      `json:"tenantId"`
	ActorType      string         `json:"actorType"`
	ActorID        *uuid.UUID     `json:"actorId"`
	Action         string         `json:"action"`
	ResourceType   string         `json:"resourceType"`
	ResourceID     *uuid.UUID     `json:"resourceId"`
	OrganizationID *uuid.UUID     `json:"organizationId"`
	RequestID      string         `json:"requestId"`
	Metadata       map[string]any `json:"metadata"`
	OccurredAt     time.Time      `json:"occurredAt"`
}

type AuditLogQuery struct {
	Limit          int
	Cursor         string
	Action         string
	ActorType      string
	ResourceType   string
	OrganizationID *uuid.UUID
	OccurredAfter  *time.Time
	OccurredBefore *time.Time
}

type AuditLogPage struct {
	Items      []AuditLogEntry `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

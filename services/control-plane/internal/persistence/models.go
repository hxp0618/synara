package persistence

import (
	"time"

	"github.com/google/uuid"
)

type PlatformInstallation struct {
	Key            string    `gorm:"column:key;primaryKey"`
	InstallationID string    `gorm:"column:installation_id;uniqueIndex"`
	Profile        string    `gorm:"column:profile"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (PlatformInstallation) TableName() string { return "platform_installations" }

type MetadataImport struct {
	ManifestID string    `gorm:"column:manifest_id;primaryKey"`
	Checksum   string    `gorm:"column:checksum"`
	ImportedAt time.Time `gorm:"column:imported_at"`
}

func (MetadataImport) TableName() string { return "metadata_imports" }

type UserIdentity struct {
	ID           uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	UserID       uuid.UUID      `gorm:"column:user_id;type:uuid"`
	ConnectionID *uuid.UUID     `gorm:"column:connection_id;type:uuid"`
	Provider     string         `gorm:"column:provider"`
	Issuer       string         `gorm:"column:issuer;uniqueIndex:uq_user_identities_issuer_subject"`
	Subject      string         `gorm:"column:subject;uniqueIndex:uq_user_identities_issuer_subject"`
	Profile      map[string]any `gorm:"column:profile;serializer:json"`
	CreatedAt    time.Time      `gorm:"column:created_at"`
	LastLoginAt  *time.Time     `gorm:"column:last_login_at"`
}

func (UserIdentity) TableName() string { return "user_identities" }

type User struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Email           string     `gorm:"column:email"`
	DisplayName     string     `gorm:"column:display_name"`
	AvatarURL       *string    `gorm:"column:avatar_url"`
	Status          string     `gorm:"column:status"`
	EmailVerifiedAt *time.Time `gorm:"column:email_verified_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
	DeletedAt       *time.Time `gorm:"column:deleted_at"`
}

func (User) TableName() string { return "users" }

type LoginSession struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID           uuid.UUID  `gorm:"column:user_id;type:uuid"`
	ActiveTenantID   *uuid.UUID `gorm:"column:active_tenant_id;type:uuid"`
	RefreshTokenHash []byte     `gorm:"column:refresh_token_hash"`
	IPAddress        *string    `gorm:"column:ip_address;type:inet"`
	UserAgent        *string    `gorm:"column:user_agent"`
	ExpiresAt        time.Time  `gorm:"column:expires_at"`
	LastSeenAt       time.Time  `gorm:"column:last_seen_at"`
	RevokedAt        *time.Time `gorm:"column:revoked_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
}

func (LoginSession) TableName() string { return "login_sessions" }

type Tenant struct {
	ID        uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	Slug      string         `gorm:"column:slug"`
	Name      string         `gorm:"column:name"`
	Status    string         `gorm:"column:status"`
	PlanCode  string         `gorm:"column:plan_code"`
	Region    string         `gorm:"column:region"`
	Settings  map[string]any `gorm:"column:settings;serializer:json"`
	CreatedBy uuid.UUID      `gorm:"column:created_by;type:uuid"`
	CreatedAt time.Time      `gorm:"column:created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at"`
	DeletedAt *time.Time     `gorm:"column:deleted_at"`
}

func (Tenant) TableName() string { return "tenants" }

type TenantMembership struct {
	TenantID  uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	UserID    uuid.UUID  `gorm:"column:user_id;type:uuid;primaryKey"`
	Role      string     `gorm:"column:role"`
	Status    string     `gorm:"column:status"`
	InvitedBy *uuid.UUID `gorm:"column:invited_by;type:uuid"`
	JoinedAt  *time.Time `gorm:"column:joined_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

func (TenantMembership) TableName() string { return "tenant_memberships" }

type Organization struct {
	ID                   uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID             uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	ParentOrganizationID *uuid.UUID     `gorm:"column:parent_organization_id;type:uuid"`
	Slug                 string         `gorm:"column:slug"`
	Name                 string         `gorm:"column:name"`
	Kind                 string         `gorm:"column:kind"`
	Status               string         `gorm:"column:status"`
	Settings             map[string]any `gorm:"column:settings;serializer:json"`
	CreatedBy            uuid.UUID      `gorm:"column:created_by;type:uuid"`
	CreatedAt            time.Time      `gorm:"column:created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at"`
	ArchivedAt           *time.Time     `gorm:"column:archived_at"`
}

func (Organization) TableName() string { return "organizations" }

type OrganizationMembership struct {
	TenantID       uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	OrganizationID uuid.UUID `gorm:"column:organization_id;type:uuid;primaryKey"`
	UserID         uuid.UUID `gorm:"column:user_id;type:uuid;primaryKey"`
	Role           string    `gorm:"column:role"`
	Status         string    `gorm:"column:status"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (OrganizationMembership) TableName() string { return "organization_memberships" }

type TenantInvitation struct {
	ID         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID   uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	Email      string     `gorm:"column:email"`
	Role       string     `gorm:"column:role"`
	TokenHash  []byte     `gorm:"column:token_hash"`
	InvitedBy  uuid.UUID  `gorm:"column:invited_by;type:uuid"`
	ExpiresAt  time.Time  `gorm:"column:expires_at"`
	AcceptedBy *uuid.UUID `gorm:"column:accepted_by;type:uuid"`
	AcceptedAt *time.Time `gorm:"column:accepted_at"`
	RevokedAt  *time.Time `gorm:"column:revoked_at"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

func (TenantInvitation) TableName() string { return "tenant_invitations" }

type AuditLog struct {
	EventID        uuid.UUID      `gorm:"column:event_id;type:uuid;primaryKey"`
	TenantID       uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	ActorType      string         `gorm:"column:actor_type"`
	ActorID        *uuid.UUID     `gorm:"column:actor_id;type:uuid"`
	Action         string         `gorm:"column:action"`
	ResourceType   string         `gorm:"column:resource_type"`
	ResourceID     *uuid.UUID     `gorm:"column:resource_id;type:uuid"`
	OrganizationID *uuid.UUID     `gorm:"column:organization_id;type:uuid"`
	RequestID      string         `gorm:"column:request_id"`
	IPAddress      *string        `gorm:"column:ip_address;type:inet"`
	Metadata       map[string]any `gorm:"column:metadata;serializer:json"`
	OccurredAt     time.Time      `gorm:"column:occurred_at"`
}

type OutboxMessage struct {
	ID          uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID    *uuid.UUID     `gorm:"column:tenant_id;type:uuid"`
	Topic       string         `gorm:"column:topic"`
	MessageKey  string         `gorm:"column:message_key"`
	Payload     map[string]any `gorm:"column:payload;serializer:json"`
	Headers     map[string]any `gorm:"column:headers;serializer:json"`
	Attempts    int            `gorm:"column:attempts"`
	AvailableAt time.Time      `gorm:"column:available_at"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	PublishedAt *time.Time     `gorm:"column:published_at"`
	LastError   *string        `gorm:"column:last_error"`
}

func (OutboxMessage) TableName() string { return "outbox_messages" }

func (AuditLog) TableName() string { return "audit_logs" }

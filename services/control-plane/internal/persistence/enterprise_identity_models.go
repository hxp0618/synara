package persistence

import (
	"time"

	"github.com/google/uuid"
)

type IdentityConnection struct {
	ID               uuid.UUID      `gorm:"column:id;type:uuid;primaryKey"`
	TenantID         uuid.UUID      `gorm:"column:tenant_id;type:uuid"`
	Kind             string         `gorm:"column:kind"`
	Name             string         `gorm:"column:name"`
	Status           string         `gorm:"column:status"`
	Issuer           string         `gorm:"column:issuer"`
	ClientID         *string        `gorm:"column:client_id"`
	EncryptedSecret  []byte         `gorm:"column:encrypted_secret"`
	EncryptedDataKey []byte         `gorm:"column:encrypted_data_key"`
	KMSProvider      *string        `gorm:"column:kms_provider"`
	KMSKeyID         *string        `gorm:"column:kms_key_id"`
	Configuration    map[string]any `gorm:"column:configuration;serializer:json"`
	CreatedBy        uuid.UUID      `gorm:"column:created_by;type:uuid"`
	UpdatedBy        uuid.UUID      `gorm:"column:updated_by;type:uuid"`
	CreatedAt        time.Time      `gorm:"column:created_at"`
	UpdatedAt        time.Time      `gorm:"column:updated_at"`
}

func (IdentityConnection) TableName() string { return "identity_connections" }

type IdentityLoginAttempt struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	ConnectionID     uuid.UUID  `gorm:"column:connection_id;type:uuid"`
	StateHash        []byte     `gorm:"column:state_hash"`
	EncryptedPayload []byte     `gorm:"column:encrypted_payload"`
	EncryptedDataKey []byte     `gorm:"column:encrypted_data_key"`
	KMSProvider      string     `gorm:"column:kms_provider"`
	KMSKeyID         string     `gorm:"column:kms_key_id"`
	ReturnTo         string     `gorm:"column:return_to"`
	ExpiresAt        time.Time  `gorm:"column:expires_at"`
	ConsumedAt       *time.Time `gorm:"column:consumed_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
}

func (IdentityLoginAttempt) TableName() string { return "identity_login_attempts" }

type ServiceAccount struct {
	ID             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	Name           string     `gorm:"column:name"`
	Description    string     `gorm:"column:description"`
	Status         string     `gorm:"column:status"`
	Scopes         []string   `gorm:"column:scopes;serializer:json"`
	CreatedBy      uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
	RevokedAt      *time.Time `gorm:"column:revoked_at"`
}

func (ServiceAccount) TableName() string { return "service_accounts" }

type ServiceAccountToken struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	ServiceAccountID uuid.UUID  `gorm:"column:service_account_id;type:uuid"`
	TokenHash        []byte     `gorm:"column:token_hash"`
	ExpiresAt        *time.Time `gorm:"column:expires_at"`
	LastUsedAt       *time.Time `gorm:"column:last_used_at"`
	RevokedAt        *time.Time `gorm:"column:revoked_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
}

func (ServiceAccountToken) TableName() string { return "service_account_tokens" }

type IdentityGroup struct {
	ID          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	ExternalID  *string    `gorm:"column:external_id"`
	DisplayName string     `gorm:"column:display_name"`
	Status      string     `gorm:"column:status"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
}

func (IdentityGroup) TableName() string { return "identity_groups" }

type IdentityGroupMember struct {
	TenantID  uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	GroupID   uuid.UUID `gorm:"column:group_id;type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"column:user_id;type:uuid;primaryKey"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (IdentityGroupMember) TableName() string { return "identity_group_members" }

type IdentityGroupMapping struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	ConnectionID     uuid.UUID  `gorm:"column:connection_id;type:uuid"`
	ExternalGroup    string     `gorm:"column:external_group"`
	TenantRole       *string    `gorm:"column:tenant_role"`
	OrganizationID   *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	OrganizationRole *string    `gorm:"column:organization_role"`
	CreatedBy        uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
}

func (IdentityGroupMapping) TableName() string { return "identity_group_mappings" }

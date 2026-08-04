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

type TenantDomain struct {
	ID                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID              uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	Domain                string     `gorm:"column:domain"`
	Status                string     `gorm:"column:status"`
	VerificationTokenHash []byte     `gorm:"column:verification_token_hash"`
	VerificationExpiresAt time.Time  `gorm:"column:verification_expires_at"`
	VerifiedAt            *time.Time `gorm:"column:verified_at"`
	VerifiedBy            *uuid.UUID `gorm:"column:verified_by;type:uuid"`
	RevokedAt             *time.Time `gorm:"column:revoked_at"`
	RevokedBy             *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
	CreatedBy             uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt             time.Time  `gorm:"column:created_at"`
	UpdatedAt             time.Time  `gorm:"column:updated_at"`
}

func (TenantDomain) TableName() string { return "tenant_domains" }

type TenantIdentityPolicy struct {
	TenantID         uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	SSOEnforcement   string     `gorm:"column:sso_enforcement"`
	Version          int64      `gorm:"column:version;not null;default:1"`
	RecoveryUserID   *uuid.UUID `gorm:"column:recovery_user_id;type:uuid"`
	EnforcementSetAt *time.Time `gorm:"column:enforcement_set_at"`
	EnforcementSetBy *uuid.UUID `gorm:"column:enforcement_set_by;type:uuid"`
	UpdatedBy        uuid.UUID  `gorm:"column:updated_by;type:uuid"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

func (TenantIdentityPolicy) TableName() string { return "tenant_identity_policies" }

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
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID     *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	Name               string     `gorm:"column:name"`
	Description        string     `gorm:"column:description"`
	Status             string     `gorm:"column:status"`
	Role               string     `gorm:"column:role"`
	Scopes             []string   `gorm:"column:scopes;serializer:json"`
	RateLimitPerMinute int        `gorm:"column:rate_limit_per_minute"`
	CreatedBy          uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
	RevokedAt          *time.Time `gorm:"column:revoked_at"`
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

type ServiceAccountAPIUsageWindow struct {
	TenantID         uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ServiceAccountID uuid.UUID  `gorm:"column:service_account_id;type:uuid;primaryKey"`
	WindowStartedAt  time.Time  `gorm:"column:window_started_at;primaryKey"`
	RoutePattern     string     `gorm:"column:route_pattern;primaryKey"`
	OrganizationID   *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	AdmittedCount    int64      `gorm:"column:admitted_count"`
	RateLimitedCount int64      `gorm:"column:rate_limited_count"`
	RequestCount     int64      `gorm:"column:request_count"`
	SuccessCount     int64      `gorm:"column:success_count"`
	ClientErrorCount int64      `gorm:"column:client_error_count"`
	ServerErrorCount int64      `gorm:"column:server_error_count"`
	TotalDurationMS  int64      `gorm:"column:total_duration_ms"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

func (ServiceAccountAPIUsageWindow) TableName() string {
	return "service_account_api_usage_windows"
}

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

package persistence

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ProviderCredential struct {
	ID                     uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID               uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID         *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	Scope                  string     `gorm:"column:scope;default:tenant"`
	ScopeUserID            *uuid.UUID `gorm:"column:scope_user_id;type:uuid"`
	SelectorOrganizationID *uuid.UUID `gorm:"column:selector_organization_id;type:uuid"`
	SelectorModel          *string    `gorm:"column:selector_model"`
	AutoSelectEnabled      bool       `gorm:"column:auto_select_enabled;default:false"`
	Name                   string     `gorm:"column:name"`
	Purpose                string     `gorm:"column:purpose;default:provider"`
	Provider               string     `gorm:"column:provider"`
	CredentialType         string     `gorm:"column:credential_type"`
	EncryptedPayload       []byte     `gorm:"column:encrypted_payload"`
	EncryptedDataKey       []byte     `gorm:"column:encrypted_data_key"`
	KMSProvider            string     `gorm:"column:kms_provider"`
	KMSKeyID               string     `gorm:"column:kms_key_id"`
	AADVersion             int        `gorm:"column:aad_version;default:3"`
	Version                int        `gorm:"column:version"`
	CreatedBy              uuid.UUID  `gorm:"column:created_by;type:uuid"`
	UpdatedBy              uuid.UUID  `gorm:"column:updated_by;type:uuid"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
	ExpiresAt              *time.Time `gorm:"column:expires_at"`
	RevokedAt              *time.Time `gorm:"column:revoked_at"`
	RevokedBy              *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
}

func (ProviderCredential) TableName() string { return "provider_credentials" }

// BeforeCreate preserves the scope shape of GORM fixtures and import paths
// that predate explicit Credential scopes. Legacy AAD versions are assigned
// only by the forward migration or an explicit compatibility import.
func (model *ProviderCredential) BeforeCreate(_ *gorm.DB) error {
	if model.Scope == "" {
		if model.OrganizationID != nil {
			model.Scope = "organization"
		} else {
			model.Scope = "tenant"
		}
	}
	return nil
}

type ProviderCredentialScopePolicy struct {
	TenantID                     uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	PlatformCredentialsEnabled   bool      `gorm:"column:platform_credentials_enabled;default:false"`
	PlatformCredentialAutoSelect bool      `gorm:"column:platform_credential_auto_select;default:false"`
	UpdatedBy                    uuid.UUID `gorm:"column:updated_by;type:uuid"`
	CreatedAt                    time.Time `gorm:"column:created_at"`
	UpdatedAt                    time.Time `gorm:"column:updated_at"`
}

func (ProviderCredentialScopePolicy) TableName() string {
	return "provider_credential_scope_policies"
}

type CredentialBinding struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id;type:uuid"`
	OrganizationID    *uuid.UUID `gorm:"column:organization_id;type:uuid"`
	ProjectID         *uuid.UUID `gorm:"column:project_id;type:uuid"`
	ExecutionTargetID *uuid.UUID `gorm:"column:execution_target_id;type:uuid"`
	CredentialID      uuid.UUID  `gorm:"column:credential_id;type:uuid"`
	BindingKind       string     `gorm:"column:binding_kind"`
	SelectorValue     string     `gorm:"column:selector_value"`
	CreatedBy         uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	DisabledAt        *time.Time `gorm:"column:disabled_at"`
	DisabledBy        *uuid.UUID `gorm:"column:disabled_by;type:uuid"`
}

func (CredentialBinding) TableName() string { return "credential_bindings" }

type ExecutionCredentialGrant struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TenantID          uuid.UUID `gorm:"column:tenant_id;type:uuid"`
	ExecutionID       uuid.UUID `gorm:"column:execution_id;type:uuid"`
	Generation        int64     `gorm:"column:generation"`
	BindingID         uuid.UUID `gorm:"column:binding_id;type:uuid"`
	CredentialID      uuid.UUID `gorm:"column:credential_id;type:uuid"`
	CredentialVersion int       `gorm:"column:credential_version"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (ExecutionCredentialGrant) TableName() string { return "execution_credential_grants" }

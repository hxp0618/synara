package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ProviderCommercialAuthorization struct {
	ID                          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID            uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	AuthorizationKey            string     `gorm:"column:authorization_key;not null;uniqueIndex"`
	Provider                    string     `gorm:"column:provider;not null"`
	ProviderProduct             string     `gorm:"column:provider_product;not null"`
	AccountType                 string     `gorm:"column:account_type;not null"`
	ContractingEntity           string     `gorm:"column:contracting_entity;not null"`
	CredentialMode              string     `gorm:"column:credential_mode;not null"`
	AllowedCredentialScopes     []string   `gorm:"column:allowed_credential_scopes;serializer:json;not null"`
	AllowedRegions              []string   `gorm:"column:allowed_regions;serializer:json;not null"`
	DataUsePolicy               string     `gorm:"column:data_use_policy;not null"`
	RetentionPolicy             string     `gorm:"column:retention_policy;not null"`
	TermsEffectiveAt            time.Time  `gorm:"column:terms_effective_at;not null"`
	TermsReference              string     `gorm:"column:terms_reference;not null"`
	TermsSHA256                 *string    `gorm:"column:terms_sha256"`
	AgreementReference          string     `gorm:"column:agreement_reference;not null"`
	AgreementSHA256             *string    `gorm:"column:agreement_sha256"`
	DPAReference                string     `gorm:"column:dpa_reference;not null"`
	DPASHA256                   *string    `gorm:"column:dpa_sha256"`
	ProhibitedUseSummary        string     `gorm:"column:prohibited_use_summary;not null"`
	TerminationRunbookReference string     `gorm:"column:termination_runbook_reference;not null"`
	TerminationRunbookSHA256    *string    `gorm:"column:termination_runbook_sha256"`
	ReviewExpiresAt             time.Time  `gorm:"column:review_expires_at;not null"`
	State                       string     `gorm:"column:state;not null"`
	Version                     int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                   uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	ActivatedAt                 *time.Time `gorm:"column:activated_at"`
	RejectedAt                  *time.Time `gorm:"column:rejected_at"`
	RevokedAt                   *time.Time `gorm:"column:revoked_at"`
	CreatedAt                   time.Time  `gorm:"column:created_at"`
	UpdatedAt                   time.Time  `gorm:"column:updated_at"`
}

func (ProviderCommercialAuthorization) TableName() string {
	return "provider_commercial_authorizations"
}

type ProviderCommercialAuthorizationApproval struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	AuthorizationID   uuid.UUID `gorm:"column:authorization_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	ApprovalRole      string    `gorm:"column:approval_role;not null"`
	Decision          string    `gorm:"column:decision;not null"`
	ApproverUserID    uuid.UUID `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason            string    `gorm:"column:reason;not null"`
	EvidenceReference string    `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string   `gorm:"column:evidence_sha256"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (ProviderCommercialAuthorizationApproval) TableName() string {
	return "provider_commercial_authorization_approvals"
}

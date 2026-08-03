package persistence

import (
	"time"

	"github.com/google/uuid"
)

type Stage6GovernanceAuthorityGrant struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID  uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	UserID            uuid.UUID  `gorm:"column:user_id;type:uuid;not null"`
	AuthorityKey      string     `gorm:"column:authority_key;not null"`
	Status            string     `gorm:"column:status;not null"`
	Version           int64      `gorm:"column:version;not null;default:1"`
	ExpiresAt         time.Time  `gorm:"column:expires_at;not null"`
	GrantedBy         uuid.UUID  `gorm:"column:granted_by;type:uuid;not null"`
	Reason            string     `gorm:"column:reason;not null"`
	EvidenceReference string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string    `gorm:"column:evidence_sha256"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
	RevokedBy         *uuid.UUID `gorm:"column:revoked_by;type:uuid"`
	RevocationReason  *string    `gorm:"column:revocation_reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (Stage6GovernanceAuthorityGrant) TableName() string {
	return "stage6_governance_authority_grants"
}

package governanceauthority

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	ReleaseEngineering       = "release.engineering"
	ReleaseOperations        = "release.operations"
	ReleaseSecurity          = "release.security"
	ReleaseProduct           = "release.product"
	ReleasePrivacyLegal      = "release.privacy_legal"
	ComplianceSecurity       = "compliance.security"
	ComplianceOperations     = "compliance.operations"
	ComplianceLegalPrivacy   = "compliance.legal_privacy"
	ComplianceExecutive      = "compliance.executive"
	ComplianceEvidencePrefix = "compliance.evidence."
	ProviderCommercialPrefix = "provider_commercial."
	RecoveryPrefix           = "recovery."
	PenetrationPrefix        = "penetration."
	CapacityPrefix           = "capacity."
	IncidentExercisePrefix   = "incident_exercise."
	OperationsExercisePrefix = "operations_exercise."
	InternalCostPrefix       = "internal_cost."
)

var AuthorityKeys = []string{
	ReleaseEngineering, ReleaseOperations, ReleaseSecurity, ReleaseProduct, ReleasePrivacyLegal,
	ComplianceSecurity, ComplianceOperations, ComplianceLegalPrivacy, ComplianceExecutive,
	ComplianceEvidencePrefix + "security", ComplianceEvidencePrefix + "operations",
	ComplianceEvidencePrefix + "legal_privacy", ComplianceEvidencePrefix + "auditor",
	ProviderCommercialPrefix + "legal", ProviderCommercialPrefix + "privacy",
	ProviderCommercialPrefix + "security", ProviderCommercialPrefix + "product",
	RecoveryPrefix + "database", RecoveryPrefix + "kms", RecoveryPrefix + "operations",
	RecoveryPrefix + "security", RecoveryPrefix + "storage",
	PenetrationPrefix + "engineering", PenetrationPrefix + "product", PenetrationPrefix + "security",
	CapacityPrefix + "engineering", CapacityPrefix + "operations",
	IncidentExercisePrefix + "operations", IncidentExercisePrefix + "communications",
	OperationsExercisePrefix + "operations", OperationsExercisePrefix + "security",
	InternalCostPrefix + "operations", InternalCostPrefix + "owner",
}

type Grant struct {
	ID                uuid.UUID  `json:"id"`
	UserID            uuid.UUID  `json:"userId"`
	UserEmail         string     `json:"userEmail"`
	UserDisplayName   string     `json:"userDisplayName"`
	AuthorityKey      string     `json:"authorityKey"`
	Status            string     `json:"status"`
	Version           int64      `json:"version"`
	ExpiresAt         time.Time  `json:"expiresAt"`
	GrantedBy         uuid.UUID  `json:"grantedBy"`
	Reason            string     `json:"reason"`
	EvidenceReference string     `json:"evidenceReference"`
	EvidenceSHA256    *string    `json:"evidenceSha256"`
	RevokedAt         *time.Time `json:"revokedAt"`
	RevokedBy         *uuid.UUID `json:"revokedBy"`
	RevocationReason  *string    `json:"revocationReason"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type Operator struct {
	UserID      uuid.UUID `json:"userId"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
}

type grantRow struct {
	persistence.Stage6GovernanceAuthorityGrant
	UserEmail       string `gorm:"column:user_email"`
	UserDisplayName string `gorm:"column:user_display_name"`
}

type CreateInput struct {
	UserID            uuid.UUID `json:"userId"`
	AuthorityKey      string    `json:"authorityKey"`
	ExpiresAt         time.Time `json:"expiresAt"`
	Reason            string    `json:"reason"`
	EvidenceReference string    `json:"evidenceReference"`
	EvidenceSHA256    string    `json:"evidenceSha256"`
}

type RevokeInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	Reason          string `json:"reason"`
}

type Service struct {
	db               *gorm.DB
	operatorTenantID uuid.UUID
	now              func() time.Time
}

var sha256ReferencePattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func NewService(db *gorm.DB, operatorTenantID uuid.UUID) *Service {
	return &Service{db: db, operatorTenantID: operatorTenantID, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) List(ctx context.Context) ([]Grant, error) {
	var rows []grantRow
	err := s.db.WithContext(ctx).Table("stage6_governance_authority_grants AS authority").
		Select("authority.*, governed_user.email AS user_email, governed_user.display_name AS user_display_name").
		Joins("JOIN users AS governed_user ON governed_user.id = authority.user_id").
		Where("authority.operator_tenant_id = ?", s.operatorTenantID).
		Order("authority.created_at DESC, authority.id DESC").Limit(500).Scan(&rows).Error
	if err != nil {
		return nil, problem.Wrap(500, "governance_authority_load_failed", "Governance authorities could not be loaded.", err)
	}
	items := make([]Grant, 0, len(rows))
	for _, row := range rows {
		items = append(items, view(row.Stage6GovernanceAuthorityGrant, row.UserEmail, row.UserDisplayName))
	}
	return items, nil
}

func (s *Service) ListEligibleOperators(ctx context.Context) ([]Operator, error) {
	var items []Operator
	err := s.db.WithContext(ctx).Table("tenant_memberships AS membership").
		Select("membership.user_id, operator_user.email, operator_user.display_name, membership.role").
		Joins("JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id").
		Joins("JOIN users AS operator_user ON operator_user.id = membership.user_id").
		Where("membership.tenant_id = ? AND membership.status = ? AND membership.role IN ?", s.operatorTenantID, "active", []string{"owner", "admin", "security_admin"}).
		Where("operator_tenant.status = ? AND operator_tenant.deleted_at IS NULL", "active").
		Where("operator_user.status = ? AND operator_user.deleted_at IS NULL", "active").
		Order("operator_user.display_name ASC, operator_user.email ASC, membership.user_id ASC").Scan(&items).Error
	if err != nil {
		return nil, problem.Wrap(500, "governance_authority_operators_load_failed", "Eligible Platform operators could not be loaded.", err)
	}
	return items, nil
}

func (s *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID, ipAddress string) (Grant, error) {
	input.AuthorityKey = strings.ToLower(strings.TrimSpace(input.AuthorityKey))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	now := s.now()
	if s.operatorTenantID == uuid.Nil || actorID == uuid.Nil || input.UserID == uuid.Nil || actorID == input.UserID ||
		!slices.Contains(AuthorityKeys, input.AuthorityKey) || len(input.Reason) < 10 || len(input.Reason) > 2000 ||
		!validHTTPSReference(input.EvidenceReference) || !validEvidenceSHA256(input.EvidenceSHA256) ||
		!input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(366*24*time.Hour)) {
		return Grant{}, problem.New(400, "governance_authority_invalid", "Governance authority requires a distinct operator, valid key, bounded expiry, reason, HTTPS evidence and non-zero SHA-256.")
	}
	model := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: s.operatorTenantID, UserID: input.UserID,
		AuthorityKey: input.AuthorityKey, Status: "active", Version: 1, ExpiresAt: input.ExpiresAt.UTC(),
		GrantedBy: actorID, Reason: input.Reason, EvidenceReference: input.EvidenceReference,
		EvidenceSHA256: &input.EvidenceSHA256,
		CreatedAt:      now, UpdatedAt: now,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOperatorRole(ctx, tx, s.operatorTenantID, actorID, []string{"owner"}); err != nil {
			return err
		}
		if err := requireOperatorRole(ctx, tx, s.operatorTenantID, input.UserID, []string{"owner", "admin", "security_admin"}); err != nil {
			return problem.New(409, "governance_authority_subject_ineligible", "The governed user is not an active Platform operator.")
		}
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "governance_authority_conflict", "This user already has an active grant for the authority.")
		} else if err != nil {
			return problem.Wrap(500, "governance_authority_create_failed", "Governance authority could not be created.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "governance.authority_granted", ResourceType: "stage6_governance_authority", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"userId": model.UserID, "authorityKey": model.AuthorityKey, "expiresAt": model.ExpiresAt,
				"evidenceReference": model.EvidenceReference, "evidenceSha256": input.EvidenceSHA256,
			},
		})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) Revoke(ctx context.Context, actorID, grantID uuid.UUID, input RevokeInput, requestID, ipAddress string) (Grant, error) {
	input.Reason = strings.TrimSpace(input.Reason)
	if actorID == uuid.Nil || input.ExpectedVersion <= 0 || len(input.Reason) < 10 || len(input.Reason) > 2000 {
		return Grant{}, problem.New(400, "governance_authority_revocation_invalid", "Authority revocation requires the current version and bounded reason.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOperatorRole(ctx, tx, s.operatorTenantID, actorID, []string{"owner"}); err != nil {
			return err
		}
		var model persistence.Stage6GovernanceAuthorityGrant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND operator_tenant_id = ?", grantID, s.operatorTenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "governance_authority_not_found", "Governance authority was not found.")
		} else if err != nil {
			return problem.Wrap(500, "governance_authority_load_failed", "Governance authority could not be loaded.", err)
		}
		if model.UserID == actorID {
			return problem.New(409, "governance_authority_self_revocation_forbidden", "The governed user cannot revoke their own authority record.")
		}
		if model.Status != "active" || model.Version != input.ExpectedVersion {
			return problem.New(409, "governance_authority_version_conflict", "Governance authority state or version changed.")
		}
		now := s.now()
		result := tx.Model(&persistence.Stage6GovernanceAuthorityGrant{}).
			Where("id = ? AND operator_tenant_id = ? AND status = ? AND version = ?", model.ID, s.operatorTenantID, "active", model.Version).
			Updates(map[string]any{"status": "revoked", "version": model.Version + 1, "revoked_at": now, "revoked_by": actorID, "revocation_reason": input.Reason, "updated_at": now})
		if result.Error != nil {
			return problem.Wrap(500, "governance_authority_revoke_failed", "Governance authority could not be revoked.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "governance_authority_version_conflict", "Governance authority changed before revocation.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID,
			Action: "governance.authority_revoked", ResourceType: "stage6_governance_authority", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"userId": model.UserID, "authorityKey": model.AuthorityKey, "reason": input.Reason},
		})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.get(ctx, grantID)
}

func (s *Service) Require(ctx context.Context, userID uuid.UUID, authorityKey string) error {
	return s.RequireWithDB(ctx, s.db, userID, authorityKey)
}

func (s *Service) RequireWithDB(ctx context.Context, db *gorm.DB, userID uuid.UUID, authorityKey string) error {
	if userID == uuid.Nil || !slices.Contains(AuthorityKeys, authorityKey) {
		return problem.New(403, "governance_authority_required", "An active matching Stage 6 governance authority is required.")
	}
	var count int64
	err := db.WithContext(ctx).Table("stage6_governance_authority_grants AS authority").
		Joins("JOIN tenant_memberships AS membership ON membership.tenant_id = authority.operator_tenant_id AND membership.user_id = authority.user_id").
		Joins("JOIN tenants AS operator_tenant ON operator_tenant.id = authority.operator_tenant_id").
		Joins("JOIN users AS governed_user ON governed_user.id = authority.user_id").
		Where("authority.operator_tenant_id = ? AND authority.user_id = ? AND authority.authority_key = ? AND authority.status = ? AND authority.expires_at > ?", s.operatorTenantID, userID, authorityKey, "active", s.now()).
		Where("authority.evidence_sha256 IS NOT NULL AND authority.evidence_sha256 <> ?", "sha256:"+strings.Repeat("0", 64)).
		Where("membership.status = ? AND membership.role IN ?", "active", []string{"owner", "admin", "security_admin"}).
		Where("operator_tenant.status = ? AND operator_tenant.deleted_at IS NULL", "active").
		Where("governed_user.status = ? AND governed_user.deleted_at IS NULL", "active").Count(&count).Error
	if err != nil {
		return problem.Wrap(500, "governance_authority_check_failed", "Governance authority could not be verified.", err)
	}
	if count != 1 {
		return problem.New(403, "governance_authority_required", "An active matching Stage 6 governance authority is required.")
	}
	return nil
}

func (s *Service) get(ctx context.Context, grantID uuid.UUID) (Grant, error) {
	var row grantRow
	err := s.db.WithContext(ctx).Table("stage6_governance_authority_grants AS authority").
		Select("authority.*, governed_user.email AS user_email, governed_user.display_name AS user_display_name").
		Joins("JOIN users AS governed_user ON governed_user.id = authority.user_id").
		Where("authority.id = ? AND authority.operator_tenant_id = ?", grantID, s.operatorTenantID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Grant{}, problem.New(404, "governance_authority_not_found", "Governance authority was not found.")
	}
	if err != nil {
		return Grant{}, problem.Wrap(500, "governance_authority_load_failed", "Governance authority could not be loaded.", err)
	}
	return view(row.Stage6GovernanceAuthorityGrant, row.UserEmail, row.UserDisplayName), nil
}

func requireOperatorRole(ctx context.Context, db *gorm.DB, tenantID, userID uuid.UUID, roles []string) error {
	var count int64
	err := db.WithContext(ctx).Table("tenant_memberships AS membership").
		Joins("JOIN tenants AS tenant ON tenant.id = membership.tenant_id").
		Joins("JOIN users AS operator_user ON operator_user.id = membership.user_id").
		Where("membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ? AND membership.role IN ?", tenantID, userID, "active", roles).
		Where("tenant.status = ? AND tenant.deleted_at IS NULL AND operator_user.status = ? AND operator_user.deleted_at IS NULL", "active", "active").Count(&count).Error
	if err != nil {
		return problem.Wrap(500, "governance_authority_principal_check_failed", "Governance authority principal could not be verified.", err)
	}
	if count != 1 {
		return problem.New(403, "governance_authority_manage_forbidden", "Platform Owner authority is required to manage governance authorities.")
	}
	return nil
}

func validHTTPSReference(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validEvidenceSHA256(value string) bool {
	return sha256ReferencePattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}

func view(model persistence.Stage6GovernanceAuthorityGrant, email, displayName string) Grant {
	return Grant{
		ID: model.ID, UserID: model.UserID, UserEmail: email, UserDisplayName: displayName,
		AuthorityKey: model.AuthorityKey, Status: model.Status, Version: model.Version, ExpiresAt: model.ExpiresAt,
		GrantedBy: model.GrantedBy, Reason: model.Reason, EvidenceReference: model.EvidenceReference,
		EvidenceSHA256: model.EvidenceSHA256,
		RevokedAt:      model.RevokedAt, RevokedBy: model.RevokedBy, RevocationReason: model.RevocationReason,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

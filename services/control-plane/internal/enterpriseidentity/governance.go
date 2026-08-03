package enterpriseidentity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const (
	domainVerificationPrefix = "synara-domain-verification="
	domainVerificationTTL    = 72 * time.Hour
)

var verifiedDomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

type Domain struct {
	ID                     uuid.UUID  `json:"id"`
	TenantID               uuid.UUID  `json:"tenantId"`
	Domain                 string     `json:"domain"`
	Status                 string     `json:"status"`
	VerificationRecordName string     `json:"verificationRecordName"`
	VerificationExpiresAt  time.Time  `json:"verificationExpiresAt"`
	VerifiedAt             *time.Time `json:"verifiedAt"`
	RevokedAt              *time.Time `json:"revokedAt"`
	CreatedAt              time.Time  `json:"createdAt"`
	UpdatedAt              time.Time  `json:"updatedAt"`
}

type DomainChallenge struct {
	Domain
	VerificationRecordValue string `json:"verificationRecordValue"`
}

type CreateDomainInput struct {
	Domain string `json:"domain"`
}

type IdentityPolicy struct {
	TenantID         uuid.UUID  `json:"tenantId"`
	SSOEnforcement   string     `json:"ssoEnforcement"`
	Version          int64      `json:"version"`
	RecoveryUserID   *uuid.UUID `json:"recoveryUserId"`
	EnforcementSetAt *time.Time `json:"enforcementSetAt"`
	UpdatedAt        *time.Time `json:"updatedAt"`
}

type UpdateIdentityPolicyInput struct {
	SSOEnforcement  string     `json:"ssoEnforcement"`
	ExpectedVersion int64      `json:"expectedVersion"`
	RecoveryUserID  *uuid.UUID `json:"recoveryUserId"`
}

func (s *Service) ListDomains(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Domain, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityRead); err != nil {
		return nil, err
	}
	var models []persistence.TenantDomain
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("status, domain, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "tenant_domains_load_failed", "Tenant domains could not be loaded.", err)
	}
	items := make([]Domain, 0, len(models))
	for _, model := range models {
		items = append(items, toDomain(model))
	}
	return items, nil
}

func (s *Service) CreateDomain(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateDomainInput,
	requestID, ipAddress string,
) (DomainChallenge, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return DomainChallenge{}, err
	}
	domain, err := normalizeVerifiedDomain(input.Domain)
	if err != nil {
		return DomainChallenge{}, err
	}
	token, _, err := secret.NewToken()
	if err != nil {
		return DomainChallenge{}, problem.Wrap(500, "domain_verification_token_failed", "Domain verification challenge could not be generated.", err)
	}
	tokenHash := sha256.Sum256([]byte(token))
	now := s.now()
	model := persistence.TenantDomain{
		ID: uuid.New(), TenantID: tenantID, Domain: domain, Status: "pending",
		VerificationTokenHash: tokenHash[:], VerificationExpiresAt: now.Add(domainVerificationTTL),
		CreatedBy: principal.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "tenant_domain_conflict", "This domain already has an active verification claim.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "identity_domain.challenge_created", ResourceType: "tenant_domain", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"domain": domain},
		})
	}); err != nil {
		return DomainChallenge{}, err
	}
	return DomainChallenge{Domain: toDomain(model), VerificationRecordValue: domainVerificationPrefix + token}, nil
}

func (s *Service) VerifyDomain(
	ctx context.Context,
	principal identity.Principal,
	tenantID, domainID uuid.UUID,
	requestID, ipAddress string,
) (Domain, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return Domain{}, err
	}
	var candidate persistence.TenantDomain
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, domainID, "pending").Take(&candidate).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Domain{}, problem.New(404, "tenant_domain_not_found", "Pending Tenant domain not found.")
	} else if err != nil {
		return Domain{}, problem.Wrap(500, "tenant_domain_load_failed", "Tenant domain could not be loaded.", err)
	}
	if !candidate.VerificationExpiresAt.After(s.now()) {
		return Domain{}, problem.New(409, "domain_verification_expired", "Domain verification challenge has expired.")
	}
	records, err := s.lookupTXT(ctx, verificationRecordName(candidate.Domain))
	if err != nil {
		return Domain{}, problem.Wrap(502, "domain_verification_dns_failed", "Domain verification DNS lookup failed.", err)
	}
	verified := false
	for _, record := range records {
		value := strings.TrimSpace(record)
		if !strings.HasPrefix(value, domainVerificationPrefix) {
			continue
		}
		hash := sha256.Sum256([]byte(strings.TrimPrefix(value, domainVerificationPrefix)))
		if subtle.ConstantTimeCompare(hash[:], candidate.VerificationTokenHash) == 1 {
			verified = true
			break
		}
	}
	if !verified {
		return Domain{}, problem.New(409, "domain_verification_record_missing", "The expected DNS TXT verification record was not found.")
	}

	now := s.now()
	if err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.TenantDomain
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ? AND status = ?", tenantID, domainID, "pending").Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "domain_verification_changed", "Domain verification state changed; reload it before retrying.")
		} else if err != nil {
			return err
		}
		if !current.VerificationExpiresAt.After(now) || subtle.ConstantTimeCompare(current.VerificationTokenHash, candidate.VerificationTokenHash) != 1 {
			return problem.New(409, "domain_verification_changed", "Domain verification state changed; reload it before retrying.")
		}
		result := tx.Model(&persistence.TenantDomain{}).
			Where("tenant_id = ? AND id = ? AND status = ?", tenantID, domainID, "pending").
			Updates(map[string]any{"status": "verified", "verified_at": now, "verified_by": principal.UserID})
		if result.Error != nil {
			return problem.Wrap(409, "domain_verification_conflict", "Domain verification could not be completed.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "domain_verification_changed", "Domain verification state changed; reload it before retrying.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "identity_domain.verified", ResourceType: "tenant_domain", ResourceID: &domainID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"domain": current.Domain},
		})
	}); err != nil {
		return Domain{}, err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, domainID).Take(&candidate).Error; err != nil {
		return Domain{}, err
	}
	return toDomain(candidate), nil
}

func (s *Service) RevokeDomain(
	ctx context.Context,
	principal identity.Principal,
	tenantID, domainID uuid.UUID,
	requestID, ipAddress string,
) error {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.TenantDomain
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND id = ? AND status <> ?", tenantID, domainID, "revoked").Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "tenant_domain_not_found", "Tenant domain not found.")
		} else if err != nil {
			return err
		}
		if current.Status == "verified" {
			var policy persistence.TenantIdentityPolicy
			policyErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ?", tenantID).Take(&policy).Error
			if policyErr != nil && !errors.Is(policyErr, gorm.ErrRecordNotFound) {
				return policyErr
			}
			if policyErr == nil && policy.SSOEnforcement == "required" {
				var otherVerified int64
				if err := tx.Model(&persistence.TenantDomain{}).Where("tenant_id = ? AND id <> ? AND status = ?", tenantID, domainID, "verified").Count(&otherVerified).Error; err != nil {
					return err
				}
				if otherVerified == 0 {
					return problem.New(409, "sso_enforcement_requires_verified_domain", "Disable SSO enforcement before revoking the last verified domain.")
				}
			}
		}
		now := s.now()
		if err := tx.Model(&persistence.TenantDomain{}).Where("tenant_id = ? AND id = ?", tenantID, domainID).
			Updates(map[string]any{"status": "revoked", "revoked_at": now, "revoked_by": principal.UserID}).Error; err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "identity_domain.revoked", ResourceType: "tenant_domain", ResourceID: &domainID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"domain": current.Domain},
		})
	})
}

func (s *Service) GetIdentityPolicy(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (IdentityPolicy, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityRead); err != nil {
		return IdentityPolicy{}, err
	}
	var model persistence.TenantIdentityPolicy
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IdentityPolicy{TenantID: tenantID, SSOEnforcement: "optional", Version: 0}, nil
	}
	if err != nil {
		return IdentityPolicy{}, problem.Wrap(500, "tenant_identity_policy_load_failed", "Tenant identity policy could not be loaded.", err)
	}
	return toIdentityPolicy(model), nil
}

func (s *Service) UpdateIdentityPolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdateIdentityPolicyInput,
	requestID, ipAddress string,
) (IdentityPolicy, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return IdentityPolicy{}, err
	}
	mode := strings.ToLower(strings.TrimSpace(input.SSOEnforcement))
	if mode != "optional" && mode != "required" {
		return IdentityPolicy{}, problem.New(400, "invalid_sso_enforcement", "ssoEnforcement must be optional or required.")
	}
	if input.ExpectedVersion < 0 {
		return IdentityPolicy{}, problem.New(400, "invalid_identity_policy_version", "expectedVersion must not be negative.")
	}
	if mode == "required" && input.RecoveryUserID == nil {
		return IdentityPolicy{}, problem.New(400, "sso_recovery_user_required", "Required SSO enforcement needs a recovery owner.")
	}
	if mode == "optional" && input.RecoveryUserID != nil {
		return IdentityPolicy{}, problem.New(400, "unexpected_sso_recovery_user", "recoveryUserId is only valid when SSO enforcement is required.")
	}

	now := s.now()
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.TenantIdentityPolicy
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ?", tenantID).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return problem.New(409, "identity_policy_version_conflict", "Tenant identity policy changed; reload it before retrying.")
			}
			current = persistence.TenantIdentityPolicy{TenantID: tenantID, SSOEnforcement: "optional", Version: 0}
		} else if loadErr != nil {
			return loadErr
		} else if current.Version != input.ExpectedVersion {
			return problem.New(409, "identity_policy_version_conflict", "Tenant identity policy changed; reload it before retrying.")
		}
		if mode == "required" {
			var verifiedDomains, activeConnections int64
			if err := tx.Model(&persistence.TenantDomain{}).Where("tenant_id = ? AND status = ?", tenantID, "verified").Count(&verifiedDomains).Error; err != nil {
				return err
			}
			if verifiedDomains == 0 {
				return problem.New(409, "sso_enforcement_requires_verified_domain", "Verify at least one Tenant domain before requiring SSO.")
			}
			if err := tx.Model(&persistence.IdentityConnection{}).Where("tenant_id = ? AND status = ?", tenantID, "active").Count(&activeConnections).Error; err != nil {
				return err
			}
			if activeConnections == 0 {
				return problem.New(409, "sso_enforcement_requires_connection", "Configure an active Identity Connection before requiring SSO.")
			}
			var recoveryMembership persistence.TenantMembership
			if err := tx.Where("tenant_id = ? AND user_id = ? AND role = ? AND status = ?", tenantID, *input.RecoveryUserID, "owner", "active").Take(&recoveryMembership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.New(409, "sso_recovery_owner_invalid", "The recovery user must be an active Tenant owner.")
			} else if err != nil {
				return err
			}
		}

		nextVersion := current.Version + 1
		model := persistence.TenantIdentityPolicy{
			TenantID: tenantID, SSOEnforcement: mode, Version: nextVersion,
			RecoveryUserID: input.RecoveryUserID, UpdatedBy: principal.UserID,
		}
		if mode == "required" {
			model.EnforcementSetAt = &now
			model.EnforcementSetBy = &principal.UserID
		}
		if current.Version == 0 {
			if err := tx.Create(&model).Error; err != nil {
				return problem.Wrap(409, "identity_policy_version_conflict", "Tenant identity policy changed; reload it before retrying.", err)
			}
		} else {
			result := tx.Model(&persistence.TenantIdentityPolicy{}).
				Where("tenant_id = ? AND version = ?", tenantID, current.Version).
				Updates(map[string]any{
					"sso_enforcement": mode, "version": nextVersion,
					"recovery_user_id": model.RecoveryUserID, "enforcement_set_at": model.EnforcementSetAt,
					"enforcement_set_by": model.EnforcementSetBy, "updated_by": principal.UserID,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return problem.New(409, "identity_policy_version_conflict", "Tenant identity policy changed; reload it before retrying.")
			}
		}
		if mode == "required" {
			if err := tx.Model(&persistence.LoginSession{}).
				Where("active_tenant_id = ? AND revoked_at IS NULL AND user_id <> ? AND (auth_method <> ? OR identity_connection_id IS NULL)", tenantID, *input.RecoveryUserID, "sso").
				Update("revoked_at", now).Error; err != nil {
				return problem.Wrap(500, "sso_enforcement_session_revoke_failed", "Non-SSO Sessions could not be revoked.", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "identity_policy.updated", ResourceType: "tenant_identity_policy", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromEnforcement": current.SSOEnforcement, "toEnforcement": mode,
				"fromVersion": current.Version, "toVersion": nextVersion,
			},
		})
	})
	if err != nil {
		return IdentityPolicy{}, err
	}
	return s.GetIdentityPolicy(ctx, principal, tenantID)
}

func normalizeVerifiedDomain(value string) (string, error) {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if len(domain) < 3 || len(domain) > 253 || !verifiedDomainPattern.MatchString(domain) {
		return "", problem.New(400, "invalid_tenant_domain", "Tenant domain must be a valid lowercase DNS domain name.")
	}
	return domain, nil
}

func verificationRecordName(domain string) string { return "_synara-verification." + domain }

func toDomain(model persistence.TenantDomain) Domain {
	return Domain{
		ID: model.ID, TenantID: model.TenantID, Domain: model.Domain, Status: model.Status,
		VerificationRecordName: verificationRecordName(model.Domain), VerificationExpiresAt: model.VerificationExpiresAt,
		VerifiedAt: model.VerifiedAt, RevokedAt: model.RevokedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func toIdentityPolicy(model persistence.TenantIdentityPolicy) IdentityPolicy {
	updatedAt := model.UpdatedAt
	return IdentityPolicy{
		TenantID: model.TenantID, SSOEnforcement: model.SSOEnforcement, Version: model.Version,
		RecoveryUserID: model.RecoveryUserID, EnforcementSetAt: model.EnforcementSetAt, UpdatedAt: &updatedAt,
	}
}

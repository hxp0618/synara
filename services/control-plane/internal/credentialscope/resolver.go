package credentialscope

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	ScopeUser         = "user"
	ScopeOrganization = "organization"
	ScopeTenant       = "tenant"
	ScopePlatform     = "platform"
)

var automaticScopeOrder = [...]string{
	ScopeUser,
	ScopeOrganization,
	ScopeTenant,
	ScopePlatform,
}

type Request struct {
	TenantID             uuid.UUID
	OrganizationID       uuid.UUID
	SessionOwnerUserID   uuid.UUID
	Provider             string
	Model                *string
	ExplicitCredentialID *uuid.UUID
	Now                  time.Time
}

type Selection struct {
	Credential persistence.ProviderCredential
	Scope      string
	Explicit   bool
}

// Resolve selects one Tenant-owned Provider Credential. An explicit Session
// binding is authoritative and never falls back. Automatic selection is
// deterministic by scope and fails closed when one priority level contains
// more than one eligible Credential.
func Resolve(ctx context.Context, db *gorm.DB, request Request) (*Selection, error) {
	request.Provider = strings.TrimSpace(request.Provider)
	request.Now = request.Now.UTC()
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	if request.TenantID == uuid.Nil || request.OrganizationID == uuid.Nil ||
		request.SessionOwnerUserID == uuid.Nil || request.Provider == "" {
		return nil, problem.New(500, "credential_scope_request_invalid", "Provider Credential scope request is invalid.")
	}

	if request.ExplicitCredentialID != nil && *request.ExplicitCredentialID != uuid.Nil {
		credential, err := loadExplicit(ctx, db, request)
		if err != nil {
			return nil, err
		}
		eligible, err := eligibleForSession(ctx, db, credential, request, false)
		if err != nil {
			return nil, err
		}
		if !eligible {
			return nil, problem.New(404, "credential_not_found", "Provider Credential not found.")
		}
		return &Selection{Credential: credential, Scope: credential.Scope, Explicit: true}, nil
	}

	var candidates []persistence.ProviderCredential
	err := persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND purpose = ? AND provider = ?", request.TenantID, "provider", request.Provider).
		Where("auto_select_enabled = ?", true).
		Where("revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", request.Now).
		Order("id ASC").
		Find(&candidates).Error
	if err != nil {
		return nil, problem.Wrap(500, "credentials_load_failed", "Provider Credentials could not be loaded.", err)
	}
	for _, candidate := range candidates {
		if !knownScope(candidate.Scope) {
			return nil, problem.New(500, "credential_scope_invalid", "Provider Credential scope is invalid.")
		}
	}

	for _, scope := range automaticScopeOrder {
		matching := make([]persistence.ProviderCredential, 0, 1)
		for _, candidate := range candidates {
			if candidate.Scope != scope {
				continue
			}
			eligible, eligibilityErr := eligibleForSession(ctx, db, candidate, request, true)
			if eligibilityErr != nil {
				return nil, eligibilityErr
			}
			if eligible {
				matching = append(matching, candidate)
			}
		}
		if len(matching) > 1 {
			return nil, problem.New(
				409,
				"credential_scope_ambiguous",
				"Multiple Provider Credentials match the same scope; bind one Credential explicitly.",
			)
		}
		if len(matching) == 1 {
			return &Selection{Credential: matching[0], Scope: scope, Explicit: false}, nil
		}
	}
	return nil, nil
}

func loadExplicit(ctx context.Context, db *gorm.DB, request Request) (persistence.ProviderCredential, error) {
	var credential persistence.ProviderCredential
	err := persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND id = ?", request.TenantID, *request.ExplicitCredentialID).
		Take(&credential).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ProviderCredential{}, problem.New(404, "credential_not_found", "Provider Credential not found.")
	}
	if err != nil {
		return persistence.ProviderCredential{}, problem.Wrap(500, "credential_load_failed", "Provider Credential could not be loaded.", err)
	}
	if credential.Purpose != "provider" {
		return persistence.ProviderCredential{}, problem.New(409, "credential_purpose_mismatch", "Agent Session requires a Provider Credential.")
	}
	if credential.Provider != request.Provider {
		return persistence.ProviderCredential{}, problem.New(409, "credential_provider_mismatch", "Provider Credential does not match the Agent Session provider.")
	}
	if credential.RevokedAt != nil || (credential.ExpiresAt != nil && !credential.ExpiresAt.After(request.Now)) {
		return persistence.ProviderCredential{}, problem.New(409, "credential_unavailable", "Provider Credential is revoked or expired.")
	}
	return credential, nil
}

func eligibleForSession(
	ctx context.Context,
	db *gorm.DB,
	credential persistence.ProviderCredential,
	request Request,
	automatic bool,
) (bool, error) {
	switch credential.Scope {
	case ScopeUser:
		if credential.ScopeUserID == nil || *credential.ScopeUserID != request.SessionOwnerUserID {
			return false, nil
		}
		return activeTenantUser(ctx, db, request.TenantID, request.SessionOwnerUserID)
	case ScopeOrganization:
		return credential.OrganizationID != nil && *credential.OrganizationID == request.OrganizationID, nil
	case ScopeTenant:
		if credential.ScopeUserID != nil || credential.OrganizationID != nil {
			return false, nil
		}
		if credential.SelectorOrganizationID != nil && *credential.SelectorOrganizationID != request.OrganizationID {
			return false, nil
		}
		if credential.SelectorModel != nil && !sameModel(credential.SelectorModel, request.Model) {
			return false, nil
		}
		return true, nil
	case ScopePlatform:
		if credential.ScopeUserID != nil || credential.OrganizationID != nil ||
			credential.SelectorOrganizationID != nil || credential.SelectorModel != nil {
			return false, nil
		}
		if automatic && !credential.AutoSelectEnabled {
			return false, nil
		}
		access, err := loadPlatformAccess(ctx, db, request.TenantID)
		if err != nil {
			return false, err
		}
		if !access.enabled {
			if !automatic {
				return false, problem.New(403, "platform_credential_not_entitled", "Platform Credentials require an enterprise entitlement and explicit Tenant policy.")
			}
			return false, nil
		}
		if automatic && !access.autoSelect {
			return false, nil
		}
		return true, nil
	default:
		return false, problem.New(500, "credential_scope_invalid", "Provider Credential scope is invalid.")
	}
}

func activeTenantUser(ctx context.Context, db *gorm.DB, tenantID, userID uuid.UUID) (bool, error) {
	var membership persistence.TenantMembership
	err := persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "active").
		Take(&membership).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "credential_scope_user_lookup_failed", "Credential scope User could not be loaded.", err)
	}
	return true, nil
}

type platformAccess struct {
	enabled    bool
	autoSelect bool
}

type PlatformAccess struct {
	Enabled    bool
	AutoSelect bool
}

func LoadPlatformAccess(ctx context.Context, db *gorm.DB, tenantID uuid.UUID) (PlatformAccess, error) {
	access, err := loadPlatformAccess(ctx, db, tenantID)
	if err != nil {
		return PlatformAccess{}, err
	}
	return PlatformAccess{Enabled: access.enabled, AutoSelect: access.autoSelect}, nil
}

func loadPlatformAccess(ctx context.Context, db *gorm.DB, tenantID uuid.UUID) (platformAccess, error) {
	entitled, err := LoadPlatformEntitlement(ctx, db, tenantID)
	if err != nil || !entitled {
		return platformAccess{}, err
	}

	var policy persistence.ProviderCredentialScopePolicy
	err = persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ?", tenantID).
		Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return platformAccess{}, nil
	}
	if err != nil {
		return platformAccess{}, problem.Wrap(500, "credential_scope_policy_load_failed", "Provider Credential scope policy could not be loaded.", err)
	}
	return platformAccess{
		enabled:    policy.PlatformCredentialsEnabled,
		autoSelect: policy.PlatformCredentialAutoSelect,
	}, nil
}

func LoadPlatformEntitlement(ctx context.Context, db *gorm.DB, tenantID uuid.UUID) (bool, error) {
	var tenant persistence.Tenant
	err := persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Select("id", "plan_code", "deleted_at").
		Where("id = ? AND deleted_at IS NULL", tenantID).
		Take(&tenant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "credential_platform_entitlement_load_failed", "Platform Credential entitlement could not be loaded.", err)
	}
	if tenant.PlanCode != "enterprise" {
		return false, nil
	}
	var installation persistence.PlatformInstallation
	err = persistence.WithLocking(db.WithContext(ctx), "SHARE", "").
		Where("key = ?", "control-plane").
		Take(&installation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, problem.Wrap(500, "credential_platform_entitlement_load_failed", "Platform Credential entitlement could not be loaded.", err)
	}
	if installation.Profile != "enterprise" {
		return false, nil
	}
	return true, nil
}

func knownScope(scope string) bool {
	switch scope {
	case ScopeUser, ScopeOrganization, ScopeTenant, ScopePlatform:
		return true
	default:
		return false
	}
}

func sameModel(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(*left) == strings.TrimSpace(*right)
}

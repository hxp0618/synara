package serviceaccounts

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
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
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

const tokenPrefix = "syna_sa_"

var allowedScopes = map[string]struct{}{
	"scim.read": {}, "scim.write": {}, "identity.read": {}, "identity.manage": {},
}

type Account struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenantId"`
	OrganizationID *uuid.UUID `json:"organizationId"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Status         string     `json:"status"`
	Scopes         []string   `json:"scopes"`
	CreatedBy      uuid.UUID  `json:"createdBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	RevokedAt      *time.Time `json:"revokedAt"`
}

type Principal struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	OrganizationID *uuid.UUID
	Name           string
	Scopes         map[string]struct{}
}

func (p Principal) Allows(scope string) bool {
	_, allowed := p.Scopes[scope]
	return allowed
}

type CreateInput struct {
	OrganizationID *uuid.UUID `json:"organizationId"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Scopes         []string   `json:"scopes"`
	ExpiresAt      *time.Time `json:"expiresAt"`
}

type IssuedAccount struct {
	Account Account `json:"account"`
	Token   string  `json:"token"`
}

type IssuedToken struct {
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Account, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.ServiceAccountsRead); err != nil {
		return nil, err
	}
	var models []persistence.ServiceAccount
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("status, LOWER(name), id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "service_accounts_load_failed", "Service Accounts could not be loaded.", err)
	}
	items := make([]Account, 0, len(models))
	for _, model := range models {
		items = append(items, toAccount(model))
	}
	return items, nil
}

func (s *Service) Create(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, input CreateInput, requestID, ipAddress string) (IssuedAccount, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return IssuedAccount{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.ServiceAccountsManage); err != nil {
		return IssuedAccount{}, err
	}
	normalized, err := s.normalizeInput(ctx, principal, tenantID, input)
	if err != nil {
		return IssuedAccount{}, err
	}
	accountID := uuid.New()
	model := persistence.ServiceAccount{
		ID: accountID, TenantID: tenantID, OrganizationID: normalized.OrganizationID,
		Name: normalized.Name, Description: normalized.Description, Status: "active",
		Scopes: normalized.Scopes, CreatedBy: principal.UserID,
	}
	plainToken, tokenModel, err := s.newToken(tenantID, accountID, normalized.ExpiresAt)
	if err != nil {
		return IssuedAccount{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "service_account_create_rejected", "Service Account creation was rejected.", err)
		}
		if err := tx.Create(&tokenModel).Error; err != nil {
			return problem.Wrap(409, "service_account_token_create_rejected", "Service Account token creation was rejected.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "service_account.created", ResourceType: "service_account", ResourceID: &accountID,
			OrganizationID: normalized.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"name": normalized.Name, "scopes": normalized.Scopes},
		})
	})
	if err != nil {
		return IssuedAccount{}, err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, accountID).Take(&model).Error; err != nil {
		return IssuedAccount{}, err
	}
	return IssuedAccount{Account: toAccount(model), Token: plainToken}, nil
}

func (s *Service) RotateToken(ctx context.Context, principal identity.Principal, tenantID, accountID uuid.UUID, expiresAt *time.Time, requestID, ipAddress string) (IssuedToken, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return IssuedToken{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.ServiceAccountsManage); err != nil {
		return IssuedToken{}, err
	}
	if expiresAt != nil && !expiresAt.After(s.now()) {
		return IssuedToken{}, problem.New(400, "invalid_service_account_token_expiry", "Service Account token expiry must be in the future.")
	}
	plainToken, tokenModel, err := s.newToken(tenantID, accountID, expiresAt)
	if err != nil {
		return IssuedToken{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var account persistence.ServiceAccount
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND id = ? AND status = ?", tenantID, accountID, "active").Take(&account).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "service_account_not_found", "Active Service Account not found.")
		} else if err != nil {
			return err
		}
		now := s.now()
		if err := tx.Model(&persistence.ServiceAccountToken{}).Where("tenant_id = ? AND service_account_id = ? AND revoked_at IS NULL", tenantID, accountID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Create(&tokenModel).Error; err != nil {
			return problem.Wrap(409, "service_account_token_create_rejected", "Service Account token creation was rejected.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "service_account.token_rotated", ResourceType: "service_account", ResourceID: &accountID,
			OrganizationID: account.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return IssuedToken{}, err
	}
	return IssuedToken{Token: plainToken, ExpiresAt: expiresAt}, nil
}

func (s *Service) Revoke(ctx context.Context, principal identity.Principal, tenantID, accountID uuid.UUID, requestID, ipAddress string) error {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.ServiceAccountsManage); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var account persistence.ServiceAccount
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND id = ?", tenantID, accountID).Take(&account).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "service_account_not_found", "Service Account not found.")
		} else if err != nil {
			return err
		}
		if account.Status == "revoked" {
			return nil
		}
		now := s.now()
		if err := tx.Model(&account).Updates(map[string]any{"status": "revoked", "revoked_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.ServiceAccountToken{}).Where("tenant_id = ? AND service_account_id = ? AND revoked_at IS NULL", tenantID, accountID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "service_account.revoked", ResourceType: "service_account", ResourceID: &accountID,
			OrganizationID: account.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, tokenPrefix) {
		return Principal{}, problem.New(401, "invalid_service_account_token", "Service Account authentication failed.")
	}
	hash := sha256.Sum256([]byte(token))
	type row struct {
		persistence.ServiceAccount
		TokenID uuid.UUID `gorm:"column:token_id"`
	}
	var matched row
	err := s.db.WithContext(ctx).Table("service_account_tokens AS sat").
		Select("sa.*, sat.id AS token_id").Joins("JOIN service_accounts AS sa ON sa.tenant_id = sat.tenant_id AND sa.id = sat.service_account_id").
		Where("sat.token_hash = ? AND sat.revoked_at IS NULL AND (sat.expires_at IS NULL OR sat.expires_at > ?)", hash[:], s.now()).
		Where("sa.status = ?", "active").Take(&matched).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Principal{}, problem.New(401, "invalid_service_account_token", "Service Account authentication failed.")
	}
	if err != nil {
		return Principal{}, problem.Wrap(500, "service_account_authentication_failed", "Service Account authentication failed.", err)
	}
	_ = s.db.WithContext(ctx).Model(&persistence.ServiceAccountToken{}).
		Where("id = ? AND (last_used_at IS NULL OR last_used_at < ?)", matched.TokenID, s.now().Add(-5*time.Minute)).
		Update("last_used_at", s.now()).Error
	scopes := make(map[string]struct{}, len(matched.Scopes))
	for _, scope := range matched.Scopes {
		scopes[scope] = struct{}{}
	}
	return Principal{ID: matched.ID, TenantID: matched.TenantID, OrganizationID: matched.OrganizationID, Name: matched.Name, Scopes: scopes}, nil
}

func (s *Service) normalizeInput(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, input CreateInput) (CreateInput, error) {
	var err error
	input.Name, err = validation.Name(input.Name, "invalid_service_account_name", "Service Account name", 160)
	if err != nil {
		return CreateInput{}, err
	}
	input.Description = strings.TrimSpace(input.Description)
	if len(input.Description) > 1000 {
		return CreateInput{}, problem.New(400, "invalid_service_account_description", "Service Account description must not exceed 1000 characters.")
	}
	input.Scopes, err = normalizeScopes(input.Scopes)
	if err != nil {
		return CreateInput{}, err
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(s.now()) {
		return CreateInput{}, problem.New(400, "invalid_service_account_token_expiry", "Service Account token expiry must be in the future.")
	}
	if input.OrganizationID != nil {
		if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, *input.OrganizationID, authorization.OrganizationRead); err != nil {
			return CreateInput{}, err
		}
	}
	return input, nil
}

func (s *Service) newToken(tenantID, accountID uuid.UUID, expiresAt *time.Time) (string, persistence.ServiceAccountToken, error) {
	plain, _, err := secret.NewToken()
	if err != nil {
		return "", persistence.ServiceAccountToken{}, problem.Wrap(500, "service_account_token_generation_failed", "Service Account token could not be generated.", err)
	}
	plain = tokenPrefix + plain
	hash := sha256.Sum256([]byte(plain))
	return plain, persistence.ServiceAccountToken{
		ID: uuid.New(), TenantID: tenantID, ServiceAccountID: accountID,
		TokenHash: hash[:], ExpiresAt: expiresAt,
	}, nil
}

func normalizeScopes(scopes []string) ([]string, error) {
	unique := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if _, allowed := allowedScopes[scope]; !allowed {
			return nil, problem.New(400, "invalid_service_account_scope", "Service Account scope is not supported.")
		}
		unique[scope] = struct{}{}
	}
	if len(unique) == 0 {
		return nil, problem.New(400, "invalid_service_account_scope", "At least one Service Account scope is required.")
	}
	result := make([]string, 0, len(unique))
	for scope := range unique {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result, nil
}

func toAccount(model persistence.ServiceAccount) Account {
	return Account{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Name: model.Name, Description: model.Description, Status: model.Status,
		Scopes: append([]string(nil), model.Scopes...), CreatedBy: model.CreatedBy,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt, RevokedAt: model.RevokedAt,
	}
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

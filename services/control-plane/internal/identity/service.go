package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Service struct {
	db             *gorm.DB
	authorizer     *authorization.Authorizer
	sessionTTL     time.Duration
	sessionIdleTTL time.Duration
	personal       *PersonalDomain
	now            func() time.Time
}

type PersonalDomain struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
}

type Principal struct {
	UserID         uuid.UUID  `json:"userId"`
	SessionID      uuid.UUID  `json:"sessionId"`
	ActiveTenantID *uuid.UUID `json:"activeTenantId"`
	Email          string     `json:"email"`
	DisplayName    string     `json:"displayName"`
}

type TenantAccess struct {
	ID       uuid.UUID `json:"id"`
	Slug     string    `json:"slug"`
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	PlanCode string    `json:"planCode"`
	Region   string    `json:"region"`
	Role     string    `json:"role"`
}

type SessionState struct {
	Authenticated bool           `json:"authenticated"`
	User          Principal      `json:"user"`
	Tenants       []TenantAccess `json:"tenants"`
}

type DevLoginInput struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type IssuedSession struct {
	Token string
	State SessionState
}

type OrganizationGrant struct {
	OrganizationID uuid.UUID
	Role           string
}

type ExternalLoginInput struct {
	ConnectionID       uuid.UUID
	Provider           string
	Issuer             string
	Subject            string
	Email              string
	DisplayName        string
	Profile            map[string]any
	TenantID           uuid.UUID
	TenantRole         string
	OrganizationGrants []OrganizationGrant
}

func NewService(db *gorm.DB, sessionTTL, sessionIdleTTL time.Duration, personal ...PersonalDomain) *Service {
	if sessionIdleTTL <= 0 || sessionIdleTTL > sessionTTL {
		sessionIdleTTL = sessionTTL
	}
	service := &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), sessionTTL: sessionTTL, sessionIdleTTL: sessionIdleTTL,
		now: func() time.Time { return time.Now().UTC() },
	}
	if len(personal) > 0 {
		service.personal = &personal[0]
	}
	return service
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if strings.TrimSpace(token) == "" {
		return Principal{}, problem.New(401, "authentication_required", "Authentication is required.")
	}
	hash := sha256.Sum256([]byte(token))
	now := s.now()
	var principal Principal
	err := s.db.WithContext(ctx).
		Table("login_sessions AS ls").
		Select("ls.user_id, ls.id AS session_id, ls.active_tenant_id, u.email, u.display_name").
		Joins("JOIN users AS u ON u.id = ls.user_id").
		Where("ls.refresh_token_hash = ?", hash[:]).
		Where("ls.revoked_at IS NULL AND ls.expires_at > ? AND ls.last_seen_at > ?", now, now.Add(-s.sessionIdleTTL)).
		Where("u.status = ? AND u.deleted_at IS NULL", "active").
		Take(&principal).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Principal{}, problem.New(401, "invalid_session", "The login session is invalid or expired.")
	}
	if err != nil {
		return Principal{}, problem.Wrap(500, "session_lookup_failed", "Failed to load the login session.", err)
	}

	refreshInterval := 5 * time.Minute
	if halfIdleTTL := s.sessionIdleTTL / 2; halfIdleTTL < refreshInterval {
		refreshInterval = halfIdleTTL
	}
	_ = s.db.WithContext(ctx).Model(&persistence.LoginSession{}).
		Where("id = ? AND revoked_at IS NULL AND expires_at > ? AND last_seen_at < ?", principal.SessionID, now, now.Add(-refreshInterval)).
		Update("last_seen_at", now).Error
	return principal, nil
}

func (s *Service) GetSessionState(ctx context.Context, principal Principal) (SessionState, error) {
	tenants, err := s.listTenantAccess(ctx, principal.UserID)
	if err != nil {
		return SessionState{}, err
	}
	return SessionState{Authenticated: true, User: principal, Tenants: tenants}, nil
}

func (s *Service) DevLogin(
	ctx context.Context,
	input DevLoginInput,
	ipAddress string,
	userAgent string,
	requestID string,
) (IssuedSession, error) {
	email, err := validation.Email(input.Email)
	if err != nil {
		return IssuedSession{}, err
	}
	displayName, err := validation.Name(input.DisplayName, "invalid_display_name", "Display name", 160)
	if err != nil {
		return IssuedSession{}, err
	}

	var userID, activeTenantID, sessionID uuid.UUID
	var token string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if s.personal != nil {
			userID = s.personal.UserID
			activeTenantID = s.personal.TenantID
			now := s.now()
			result := tx.WithContext(ctx).Model(&persistence.User{}).
				Where("id = ? AND deleted_at IS NULL", userID).
				Updates(map[string]any{
					"email": email, "display_name": displayName, "status": "active", "email_verified_at": now,
				})
			if result.Error != nil {
				return problem.Wrap(409, "personal_owner_update_failed", "The local owner identity could not be updated.", result.Error)
			}
			if result.RowsAffected != 1 {
				return problem.New(500, "personal_owner_missing", "The bootstrapped local owner identity is missing.")
			}
		} else {
			userID, err = upsertDevUser(ctx, tx, email, displayName)
			if err != nil {
				return err
			}
			activeTenantID, err = ensurePersonalTenant(ctx, tx, userID, displayName, requestID)
			if err != nil {
				return err
			}
		}
		var tokenHash []byte
		token, tokenHash, err = secret.NewToken()
		if err != nil {
			return problem.Wrap(500, "login_failed", "Failed to generate a login session.", err)
		}
		sessionID = uuid.New()
		now := s.now()
		return tx.Create(&persistence.LoginSession{
			ID: sessionID, UserID: userID, ActiveTenantID: &activeTenantID,
			RefreshTokenHash: tokenHash, IPAddress: optionalString(ipAddress),
			UserAgent: optionalString(userAgent), ExpiresAt: now.Add(s.sessionTTL), LastSeenAt: now,
		}).Error
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		var apiError *problem.Error
		if errors.As(err, &apiError) {
			return IssuedSession{}, err
		}
		return IssuedSession{}, problem.Wrap(500, "login_failed", "Failed to complete the login transaction.", err)
	}

	principal := Principal{
		UserID: userID, SessionID: sessionID, ActiveTenantID: &activeTenantID,
		Email: email, DisplayName: displayName,
	}
	state, err := s.GetSessionState(ctx, principal)
	if err != nil {
		return IssuedSession{}, err
	}
	return IssuedSession{Token: token, State: state}, nil
}

func (s *Service) CompleteExternalLogin(
	ctx context.Context,
	input ExternalLoginInput,
	ipAddress string,
	userAgent string,
	requestID string,
) (IssuedSession, error) {
	email, err := validation.Email(input.Email)
	if err != nil {
		return IssuedSession{}, err
	}
	displayName, err := validation.Name(input.DisplayName, "invalid_display_name", "Display name", 160)
	if err != nil {
		return IssuedSession{}, err
	}
	if input.ConnectionID == uuid.Nil || input.TenantID == uuid.Nil || strings.TrimSpace(input.Issuer) == "" || strings.TrimSpace(input.Subject) == "" {
		return IssuedSession{}, problem.New(400, "invalid_external_identity", "External identity is incomplete.")
	}
	if input.TenantRole == "" {
		input.TenantRole = "member"
	}
	if !validTenantRole(input.TenantRole) {
		return IssuedSession{}, problem.New(400, "invalid_external_identity_role", "External identity Tenant role is invalid.")
	}

	var userID, sessionID uuid.UUID
	var token string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var connection persistence.IdentityConnection
		if err := tx.Where("id = ? AND tenant_id = ? AND status = ?", input.ConnectionID, input.TenantID, "active").Take(&connection).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(403, "identity_connection_unavailable", "Identity connection is unavailable.")
		} else if err != nil {
			return err
		}
		var external persistence.UserIdentity
		lookupErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("issuer = ? AND subject = ?", strings.TrimSpace(input.Issuer), strings.TrimSpace(input.Subject)).Take(&external).Error
		if lookupErr == nil {
			userID = external.UserID
		} else if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			var user persistence.User
			userErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("LOWER(email) = ? AND deleted_at IS NULL", email).Take(&user).Error
			now := s.now()
			if errors.Is(userErr, gorm.ErrRecordNotFound) {
				user = persistence.User{ID: uuid.New(), Email: email, DisplayName: displayName, Status: "active", EmailVerifiedAt: &now}
				if err := tx.Create(&user).Error; err != nil {
					return problem.Wrap(409, "external_user_create_rejected", "External user creation was rejected.", err)
				}
			} else if userErr != nil {
				return userErr
			}
			userID = user.ID
			external = persistence.UserIdentity{
				ID: uuid.New(), UserID: userID, ConnectionID: &input.ConnectionID,
				Provider: input.Provider, Issuer: strings.TrimSpace(input.Issuer), Subject: strings.TrimSpace(input.Subject),
				Profile: input.Profile, LastLoginAt: &now,
			}
			if err := tx.Create(&external).Error; err != nil {
				return problem.Wrap(409, "external_identity_link_rejected", "External identity could not be linked.", err)
			}
		} else {
			return lookupErr
		}
		now := s.now()
		if err := tx.Model(&persistence.User{}).Where("id = ? AND deleted_at IS NULL", userID).Updates(map[string]any{
			"email": email, "display_name": displayName, "status": "active", "email_verified_at": now,
		}).Error; err != nil {
			return problem.Wrap(409, "external_user_update_rejected", "External user profile update was rejected.", err)
		}
		if err := tx.Model(&persistence.UserIdentity{}).Where("id = ?", external.ID).Updates(persistence.UserIdentity{
			ConnectionID: &input.ConnectionID,
			Provider:     input.Provider,
			Profile:      input.Profile,
			LastLoginAt:  &now,
		}).Error; err != nil {
			return err
		}
		var current persistence.TenantMembership
		membershipErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND user_id = ?", input.TenantID, userID).Take(&current).Error
		tenantRole := input.TenantRole
		if membershipErr == nil {
			tenantRole = strongerTenantRole(current.Role, tenantRole)
		} else if !errors.Is(membershipErr, gorm.ErrRecordNotFound) {
			return membershipErr
		}
		membership := persistence.TenantMembership{TenantID: input.TenantID, UserID: userID, Role: tenantRole, Status: "active", JoinedAt: &now}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{"role": tenantRole, "status": "active", "joined_at": now}),
		}).Create(&membership).Error; err != nil {
			return problem.Wrap(409, "external_membership_update_rejected", "External Tenant membership update was rejected.", err)
		}
		for _, grant := range input.OrganizationGrants {
			if grant.OrganizationID == uuid.Nil || !validOrganizationRole(grant.Role) {
				return problem.New(400, "invalid_external_identity_role", "External identity Organization role is invalid.")
			}
			organizationMembership := persistence.OrganizationMembership{TenantID: input.TenantID, OrganizationID: grant.OrganizationID, UserID: userID, Role: grant.Role, Status: "active"}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "organization_id"}, {Name: "user_id"}},
				DoUpdates: clause.Assignments(map[string]any{"role": grant.Role, "status": "active"}),
			}).Create(&organizationMembership).Error; err != nil {
				return problem.Wrap(409, "external_organization_membership_update_rejected", "External Organization membership update was rejected.", err)
			}
		}
		var tokenHash []byte
		token, tokenHash, err = secret.NewToken()
		if err != nil {
			return problem.Wrap(500, "login_failed", "Failed to generate a login session.", err)
		}
		sessionID = uuid.New()
		if err := tx.Create(&persistence.LoginSession{
			ID: sessionID, UserID: userID, ActiveTenantID: &input.TenantID, RefreshTokenHash: tokenHash,
			IPAddress: optionalString(ipAddress), UserAgent: optionalString(userAgent),
			ExpiresAt: now.Add(s.sessionTTL), LastSeenAt: now,
		}).Error; err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: input.TenantID, ActorType: "user", ActorID: &userID,
			Action: "identity.sso_login", ResourceType: "identity_connection", ResourceID: &input.ConnectionID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"provider": input.Provider},
		})
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return IssuedSession{}, err
	}
	principal := Principal{UserID: userID, SessionID: sessionID, ActiveTenantID: &input.TenantID, Email: email, DisplayName: displayName}
	state, err := s.GetSessionState(ctx, principal)
	if err != nil {
		return IssuedSession{}, err
	}
	return IssuedSession{Token: token, State: state}, nil
}

func (s *Service) Revoke(ctx context.Context, principal Principal) error {
	result := s.db.WithContext(ctx).Model(&persistence.LoginSession{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", principal.SessionID, principal.UserID).
		Update("revoked_at", s.now())
	if result.Error != nil {
		return problem.Wrap(500, "logout_failed", "Failed to revoke the login session.", result.Error)
	}
	return nil
}

func (s *Service) RevokeTenantUserSessions(
	ctx context.Context,
	principal Principal,
	tenantID, userID uuid.UUID,
	requestID, ipAddress string,
) (int64, error) {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return 0, problem.New(409, "active_tenant_mismatch", "The requested tenant must be the active tenant.")
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.IdentitySessionsRevoke); err != nil {
		return 0, err
	}
	var membership persistence.TenantMembership
	err := s.db.WithContext(ctx).
		Select("tenant_id", "user_id").
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Take(&membership).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, problem.New(404, "tenant_member_not_found", "Tenant member not found.")
	}
	if err != nil {
		return 0, problem.Wrap(500, "tenant_member_load_failed", "Tenant member could not be loaded.", err)
	}

	var revoked int64
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := s.now()
		result := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
			Where("user_id = ? AND active_tenant_id = ? AND revoked_at IS NULL", userID, tenantID).
			Update("revoked_at", now)
		if result.Error != nil {
			return problem.Wrap(500, "session_revoke_failed", "Login Sessions could not be revoked.", result.Error)
		}
		revoked = result.RowsAffected
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "identity.sessions_revoked", ResourceType: "user", ResourceID: &userID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"revokedCount": revoked},
		})
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

func (s *Service) SetActiveTenant(ctx context.Context, principal Principal, tenantID uuid.UUID) (Principal, error) {
	var membership persistence.TenantMembership
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, principal.UserID, "active").
		Take(&membership).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Principal{}, problem.New(403, "tenant_forbidden", "You do not have access to this tenant.")
	}
	if err != nil {
		return Principal{}, problem.Wrap(500, "active_tenant_update_failed", "Failed to validate tenant access.", err)
	}
	var tenant persistence.Tenant
	if err := s.db.WithContext(ctx).
		Where("id = ? AND status = ? AND deleted_at IS NULL", tenantID, "active").
		Take(&tenant).Error; err != nil {
		return Principal{}, problem.New(403, "tenant_forbidden", "You do not have access to this tenant.")
	}
	result := s.db.WithContext(ctx).Model(&persistence.LoginSession{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", principal.SessionID, principal.UserID).
		Updates(map[string]any{"active_tenant_id": tenantID, "last_seen_at": s.now()})
	if result.Error != nil {
		return Principal{}, problem.Wrap(500, "active_tenant_update_failed", "Failed to update the active tenant.", result.Error)
	}
	if result.RowsAffected == 0 {
		return Principal{}, problem.New(401, "invalid_session", "The login session is invalid or expired.")
	}
	principal.ActiveTenantID = &tenantID
	return principal, nil
}

func (s *Service) listTenantAccess(ctx context.Context, userID uuid.UUID) ([]TenantAccess, error) {
	result := make([]TenantAccess, 0)
	err := s.db.WithContext(ctx).
		Table("tenant_memberships AS tm").
		Select("t.id, t.slug, t.name, t.status, t.plan_code, t.region, tm.role").
		Joins("JOIN tenants AS t ON t.id = tm.tenant_id").
		Where("tm.user_id = ? AND tm.status = ? AND t.deleted_at IS NULL", userID, "active").
		Order("LOWER(t.name), t.id").
		Scan(&result).Error
	if err != nil {
		return nil, problem.Wrap(500, "tenants_load_failed", "Failed to load tenant memberships.", err)
	}
	return result, nil
}

func upsertDevUser(ctx context.Context, tx *gorm.DB, email, displayName string) (uuid.UUID, error) {
	var user persistence.User
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("LOWER(email) = ? AND deleted_at IS NULL", email).Take(&user).Error
	now := time.Now().UTC()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		user = persistence.User{
			ID: uuid.New(), Email: email, DisplayName: displayName, Status: "active", EmailVerifiedAt: &now,
		}
		if err := tx.Create(&user).Error; err != nil {
			return uuid.Nil, problem.Wrap(500, "user_create_failed", "Failed to create the local SaaS user.", err)
		}
		return user.ID, nil
	}
	if err != nil {
		return uuid.Nil, problem.Wrap(500, "user_lookup_failed", "Failed to load the local SaaS user.", err)
	}
	updates := map[string]any{"display_name": displayName, "status": "active"}
	if user.EmailVerifiedAt == nil {
		updates["email_verified_at"] = now
	}
	if err := tx.Model(&persistence.User{}).Where("id = ?", user.ID).Updates(updates).Error; err != nil {
		return uuid.Nil, problem.Wrap(500, "user_update_failed", "Failed to update the local SaaS user.", err)
	}
	return user.ID, nil
}

func ensurePersonalTenant(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	displayName string,
	requestID string,
) (uuid.UUID, error) {
	var existing struct{ TenantID uuid.UUID }
	err := tx.WithContext(ctx).
		Table("tenant_memberships AS tm").
		Select("tm.tenant_id").
		Joins("JOIN tenants AS t ON t.id = tm.tenant_id").
		Where("tm.user_id = ? AND tm.status = ?", userID, "active").
		Where("t.status = ? AND t.deleted_at IS NULL", "active").
		Order("tm.joined_at, tm.tenant_id").
		Take(&existing).Error
	if err == nil {
		return existing.TenantID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, problem.Wrap(500, "tenant_lookup_failed", "Failed to load the personal tenant.", err)
	}

	tenantID := uuid.New()
	organizationID := uuid.New()
	slug := "personal-" + strings.ReplaceAll(userID.String(), "-", "")[:12]
	if err := tx.Create(&persistence.Tenant{
		ID: tenantID, Slug: slug, Name: displayName + "'s workspace", Status: "active",
		PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID,
	}).Error; err != nil {
		return uuid.Nil, problem.Wrap(500, "tenant_create_failed", "Failed to create the personal tenant.", err)
	}
	now := time.Now().UTC()
	if err := tx.Create(&persistence.TenantMembership{
		TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		return uuid.Nil, problem.Wrap(500, "tenant_create_failed", "Failed to create the owner membership.", err)
	}
	if err := tx.Create(&persistence.Organization{
		ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root organization",
		Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID,
	}).Error; err != nil {
		return uuid.Nil, problem.Wrap(500, "tenant_create_failed", "Failed to create the root organization.", err)
	}
	if err := tx.Create(&persistence.OrganizationMembership{
		TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "owner", Status: "active",
	}).Error; err != nil {
		return uuid.Nil, problem.Wrap(500, "tenant_create_failed", "Failed to create the root organization owner.", err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &userID,
		Action: "tenant.created", ResourceType: "tenant", ResourceID: &tenantID,
		OrganizationID: &organizationID, RequestID: requestID,
		Metadata: map[string]any{"source": "dev_bootstrap"},
	}); err != nil {
		return uuid.Nil, problem.Wrap(500, "tenant_create_failed", "Failed to write the tenant audit record.", err)
	}
	return tenantID, nil
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

var tenantRoleRank = map[string]int{
	"member": 1, "auditor": 2, "billing_admin": 3, "security_admin": 4, "admin": 5, "owner": 6,
}

func validTenantRole(role string) bool {
	_, valid := tenantRoleRank[role]
	return valid
}

func strongerTenantRole(current, requested string) string {
	if tenantRoleRank[current] >= tenantRoleRank[requested] {
		return current
	}
	return requested
}

func validOrganizationRole(role string) bool {
	switch role {
	case "owner", "admin", "agent_operator", "member", "viewer":
		return true
	default:
		return false
	}
}

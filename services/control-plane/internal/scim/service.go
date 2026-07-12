package scim

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

const (
	userSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	groupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
)

type Email struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

type User struct {
	Schemas     []string       `json:"schemas"`
	ID          string         `json:"id"`
	ExternalID  string         `json:"externalId,omitempty"`
	UserName    string         `json:"userName"`
	DisplayName string         `json:"displayName"`
	Active      bool           `json:"active"`
	Emails      []Email        `json:"emails"`
	Meta        map[string]any `json:"meta"`
}

type UserInput struct {
	Schemas     []string `json:"schemas"`
	ExternalID  string   `json:"externalId"`
	UserName    string   `json:"userName"`
	DisplayName string   `json:"displayName"`
	Active      *bool    `json:"active"`
	Emails      []Email  `json:"emails"`
}

type Member struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

type Group struct {
	Schemas     []string       `json:"schemas"`
	ID          string         `json:"id"`
	ExternalID  string         `json:"externalId,omitempty"`
	DisplayName string         `json:"displayName"`
	Members     []Member       `json:"members"`
	Meta        map[string]any `json:"meta"`
}

type GroupInput struct {
	Schemas     []string `json:"schemas"`
	ExternalID  string   `json:"externalId"`
	DisplayName string   `json:"displayName"`
	Members     []Member `json:"members"`
}

type ListResponse[T any] struct {
	Schemas      []string `json:"schemas"`
	TotalResults int64    `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []T      `json:"Resources"`
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) ListUsers(ctx context.Context, principal serviceaccounts.Principal, startIndex, count int) (ListResponse[User], error) {
	if err := requireScope(principal, "scim.read", "scim.write"); err != nil {
		return ListResponse[User]{}, err
	}
	startIndex, count = normalizePage(startIndex, count)
	var total int64
	base := s.db.WithContext(ctx).Table("tenant_memberships AS tm").
		Joins("JOIN users AS u ON u.id = tm.user_id").
		Where("tm.tenant_id = ? AND u.deleted_at IS NULL", principal.TenantID)
	if err := base.Count(&total).Error; err != nil {
		return ListResponse[User]{}, problem.Wrap(500, "scim_users_load_failed", "SCIM Users could not be loaded.", err)
	}
	type row struct {
		persistence.User
		MembershipStatus string  `gorm:"column:membership_status"`
		ExternalID       *string `gorm:"column:external_id"`
	}
	var rows []row
	err := base.Select("u.*, tm.status AS membership_status, ui.profile ->> 'externalId' AS external_id").
		Joins("LEFT JOIN user_identities AS ui ON ui.user_id = u.id AND ui.provider = ? AND ui.issuer = ?", "scim", scimIssuer(principal.TenantID)).
		Order("LOWER(u.email), u.id").Offset(startIndex - 1).Limit(count).Scan(&rows).Error
	if err != nil {
		return ListResponse[User]{}, problem.Wrap(500, "scim_users_load_failed", "SCIM Users could not be loaded.", err)
	}
	resources := make([]User, 0, len(rows))
	for _, row := range rows {
		resources = append(resources, toUser(row.User, row.MembershipStatus == "active", row.ExternalID))
	}
	return ListResponse[User]{Schemas: []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"}, TotalResults: total, StartIndex: startIndex, ItemsPerPage: len(resources), Resources: resources}, nil
}

func (s *Service) GetUser(ctx context.Context, principal serviceaccounts.Principal, userID uuid.UUID) (User, error) {
	if err := requireScope(principal, "scim.read", "scim.write"); err != nil {
		return User{}, err
	}
	type row struct {
		persistence.User
		MembershipStatus string  `gorm:"column:membership_status"`
		ExternalID       *string `gorm:"column:external_id"`
	}
	var result row
	err := s.db.WithContext(ctx).Table("tenant_memberships AS tm").
		Select("u.*, tm.status AS membership_status, ui.profile ->> 'externalId' AS external_id").
		Joins("JOIN users AS u ON u.id = tm.user_id").
		Joins("LEFT JOIN user_identities AS ui ON ui.user_id = u.id AND ui.provider = ? AND ui.issuer = ?", "scim", scimIssuer(principal.TenantID)).
		Where("tm.tenant_id = ? AND tm.user_id = ? AND u.deleted_at IS NULL", principal.TenantID, userID).Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, problem.New(404, "scim_user_not_found", "SCIM User not found.")
	}
	if err != nil {
		return User{}, problem.Wrap(500, "scim_user_load_failed", "SCIM User could not be loaded.", err)
	}
	return toUser(result.User, result.MembershipStatus == "active", result.ExternalID), nil
}

func (s *Service) CreateUser(ctx context.Context, principal serviceaccounts.Principal, input UserInput, requestID, ipAddress string) (User, error) {
	if err := requireScope(principal, "scim.write"); err != nil {
		return User{}, err
	}
	email, displayName, active, externalID, err := normalizeUserInput(input)
	if err != nil {
		return User{}, err
	}
	userID := uuid.New()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var user persistence.User
		loadErr := tx.Where("LOWER(email) = ? AND deleted_at IS NULL", email).Take(&user).Error
		switch {
		case errors.Is(loadErr, gorm.ErrRecordNotFound):
			now := s.now()
			user = persistence.User{ID: userID, Email: email, DisplayName: displayName, Status: "active", EmailVerifiedAt: &now}
			if err := tx.Create(&user).Error; err != nil {
				return problem.Wrap(409, "scim_user_create_rejected", "SCIM User creation was rejected.", err)
			}
		case loadErr != nil:
			return loadErr
		default:
			userID = user.ID
			if err := tx.Model(&user).Updates(map[string]any{"display_name": displayName}).Error; err != nil {
				return err
			}
		}
		now := s.now()
		membershipStatus := "suspended"
		var joinedAt *time.Time
		if active {
			membershipStatus = "active"
			joinedAt = &now
		}
		membership := persistence.TenantMembership{TenantID: principal.TenantID, UserID: userID, Role: "member", Status: membershipStatus, JoinedAt: joinedAt}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{"status": membershipStatus, "joined_at": joinedAt}),
		}).Create(&membership).Error; err != nil {
			return problem.Wrap(409, "scim_membership_create_rejected", "SCIM Tenant membership creation was rejected.", err)
		}
		identity := persistence.UserIdentity{ID: uuid.New(), UserID: userID, Provider: "scim", Issuer: scimIssuer(principal.TenantID), Subject: userID.String(), Profile: map[string]any{"externalId": externalID}}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "issuer"}, {Name: "subject"}},
			DoUpdates: clause.AssignmentColumns([]string{"profile"}),
		}).Create(&identity).Error; err != nil {
			return problem.Wrap(409, "scim_identity_create_rejected", "SCIM identity creation was rejected.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: principal.TenantID, ActorType: "service_account", ActorID: &principal.ID,
			Action: "scim.user.provisioned", ResourceType: "user", ResourceID: &userID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"active": active},
		})
	})
	if err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, principal, userID)
}

func (s *Service) ReplaceUser(ctx context.Context, principal serviceaccounts.Principal, userID uuid.UUID, input UserInput, requestID, ipAddress string) (User, error) {
	if err := requireScope(principal, "scim.write"); err != nil {
		return User{}, err
	}
	email, displayName, active, externalID, err := normalizeUserInput(input)
	if err != nil {
		return User{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var membership persistence.TenantMembership
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND user_id = ?", principal.TenantID, userID).Take(&membership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "scim_user_not_found", "SCIM User not found.")
		} else if err != nil {
			return err
		}
		if !active && membership.Role == "owner" {
			var ownerCount int64
			if err := tx.Model(&persistence.TenantMembership{}).Where("tenant_id = ? AND role = ? AND status = ?", principal.TenantID, "owner", "active").Count(&ownerCount).Error; err != nil {
				return err
			}
			if ownerCount <= 1 {
				return problem.New(409, "last_tenant_owner", "The final active Tenant Owner cannot be suspended through SCIM.")
			}
		}
		if err := tx.Model(&persistence.User{}).Where("id = ? AND deleted_at IS NULL", userID).Updates(map[string]any{"email": email, "display_name": displayName}).Error; err != nil {
			return problem.Wrap(409, "scim_user_update_rejected", "SCIM User update was rejected.", err)
		}
		status := "suspended"
		if active {
			status = "active"
		}
		if err := tx.Model(&membership).Update("status", status).Error; err != nil {
			return err
		}
		if !active {
			if err := tx.Model(&persistence.OrganizationMembership{}).Where("tenant_id = ? AND user_id = ?", principal.TenantID, userID).Update("status", "suspended").Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&persistence.UserIdentity{}).Where("user_id = ? AND provider = ? AND issuer = ?", userID, "scim", scimIssuer(principal.TenantID)).Update("profile", map[string]any{"externalId": externalID}).Error; err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: principal.TenantID, ActorType: "service_account", ActorID: &principal.ID,
			Action: "scim.user.updated", ResourceType: "user", ResourceID: &userID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"active": active},
		})
	})
	if err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, principal, userID)
}

func (s *Service) DeleteUser(ctx context.Context, principal serviceaccounts.Principal, userID uuid.UUID, requestID, ipAddress string) error {
	current, err := s.GetUser(ctx, principal, userID)
	if err != nil {
		return err
	}
	active := false
	_, err = s.ReplaceUser(ctx, principal, userID, UserInput{UserName: current.UserName, DisplayName: current.DisplayName, ExternalID: current.ExternalID, Active: &active}, requestID, ipAddress)
	return err
}

func (s *Service) ListGroups(ctx context.Context, principal serviceaccounts.Principal, startIndex, count int) (ListResponse[Group], error) {
	if err := requireScope(principal, "scim.read", "scim.write"); err != nil {
		return ListResponse[Group]{}, err
	}
	startIndex, count = normalizePage(startIndex, count)
	var total int64
	base := s.db.WithContext(ctx).Model(&persistence.IdentityGroup{}).Where("tenant_id = ? AND deleted_at IS NULL", principal.TenantID)
	if err := base.Count(&total).Error; err != nil {
		return ListResponse[Group]{}, problem.Wrap(500, "scim_groups_load_failed", "SCIM Groups could not be loaded.", err)
	}
	var models []persistence.IdentityGroup
	if err := base.Order("LOWER(display_name), id").Offset(startIndex - 1).Limit(count).Find(&models).Error; err != nil {
		return ListResponse[Group]{}, problem.Wrap(500, "scim_groups_load_failed", "SCIM Groups could not be loaded.", err)
	}
	resources := make([]Group, 0, len(models))
	for _, model := range models {
		item, err := s.group(ctx, model)
		if err != nil {
			return ListResponse[Group]{}, err
		}
		resources = append(resources, item)
	}
	return ListResponse[Group]{Schemas: []string{"urn:ietf:params:scim:api:messages:2.0:ListResponse"}, TotalResults: total, StartIndex: startIndex, ItemsPerPage: len(resources), Resources: resources}, nil
}

func (s *Service) GetGroup(ctx context.Context, principal serviceaccounts.Principal, groupID uuid.UUID) (Group, error) {
	if err := requireScope(principal, "scim.read", "scim.write"); err != nil {
		return Group{}, err
	}
	var model persistence.IdentityGroup
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", principal.TenantID, groupID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Group{}, problem.New(404, "scim_group_not_found", "SCIM Group not found.")
	} else if err != nil {
		return Group{}, problem.Wrap(500, "scim_group_load_failed", "SCIM Group could not be loaded.", err)
	}
	return s.group(ctx, model)
}

func (s *Service) CreateGroup(ctx context.Context, principal serviceaccounts.Principal, input GroupInput, requestID, ipAddress string) (Group, error) {
	if err := requireScope(principal, "scim.write"); err != nil {
		return Group{}, err
	}
	normalized, memberIDs, err := normalizeGroupInput(input)
	if err != nil {
		return Group{}, err
	}
	groupID := uuid.New()
	model := persistence.IdentityGroup{ID: groupID, TenantID: principal.TenantID, ExternalID: optional(normalized.ExternalID), DisplayName: normalized.DisplayName, Status: "active"}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "scim_group_create_rejected", "SCIM Group creation was rejected.", err)
		}
		if err := replaceMembers(ctx, tx, principal.TenantID, groupID, memberIDs); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: principal.TenantID, ActorType: "service_account", ActorID: &principal.ID, Action: "scim.group.provisioned", ResourceType: "identity_group", ResourceID: &groupID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"memberCount": len(memberIDs)}})
	})
	if err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, principal, groupID)
}

func (s *Service) ReplaceGroup(ctx context.Context, principal serviceaccounts.Principal, groupID uuid.UUID, input GroupInput, requestID, ipAddress string) (Group, error) {
	if err := requireScope(principal, "scim.write"); err != nil {
		return Group{}, err
	}
	normalized, memberIDs, err := normalizeGroupInput(input)
	if err != nil {
		return Group{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.IdentityGroup{}).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", principal.TenantID, groupID).Updates(map[string]any{"external_id": optional(normalized.ExternalID), "display_name": normalized.DisplayName})
		if result.Error != nil {
			return problem.Wrap(409, "scim_group_update_rejected", "SCIM Group update was rejected.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(404, "scim_group_not_found", "SCIM Group not found.")
		}
		if err := replaceMembers(ctx, tx, principal.TenantID, groupID, memberIDs); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: principal.TenantID, ActorType: "service_account", ActorID: &principal.ID, Action: "scim.group.updated", ResourceType: "identity_group", ResourceID: &groupID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"memberCount": len(memberIDs)}})
	})
	if err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, principal, groupID)
}

func (s *Service) DeleteGroup(ctx context.Context, principal serviceaccounts.Principal, groupID uuid.UUID, requestID, ipAddress string) error {
	if err := requireScope(principal, "scim.write"); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := s.now()
		result := tx.Model(&persistence.IdentityGroup{}).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", principal.TenantID, groupID).Updates(map[string]any{"status": "deleted", "deleted_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return problem.New(404, "scim_group_not_found", "SCIM Group not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: principal.TenantID, ActorType: "service_account", ActorID: &principal.ID, Action: "scim.group.deleted", ResourceType: "identity_group", ResourceID: &groupID, RequestID: requestID, IPAddress: ipAddress})
	})
}

func (s *Service) group(ctx context.Context, model persistence.IdentityGroup) (Group, error) {
	var members []Member
	if err := s.db.WithContext(ctx).Table("identity_group_members AS gm").Select("CAST(gm.user_id AS TEXT) AS value, u.display_name AS display").Joins("JOIN users AS u ON u.id = gm.user_id").Where("gm.tenant_id = ? AND gm.group_id = ?", model.TenantID, model.ID).Order("LOWER(u.display_name), gm.user_id").Scan(&members).Error; err != nil {
		return Group{}, problem.Wrap(500, "scim_group_members_load_failed", "SCIM Group members could not be loaded.", err)
	}
	externalID := ""
	if model.ExternalID != nil {
		externalID = *model.ExternalID
	}
	return Group{Schemas: []string{groupSchema}, ID: model.ID.String(), ExternalID: externalID, DisplayName: model.DisplayName, Members: members, Meta: resourceMeta("Group", model.CreatedAt, model.UpdatedAt)}, nil
}

func replaceMembers(ctx context.Context, tx *gorm.DB, tenantID, groupID uuid.UUID, memberIDs []uuid.UUID) error {
	if len(memberIDs) > 0 {
		var activeCount int64
		if err := tx.WithContext(ctx).Model(&persistence.TenantMembership{}).Where("tenant_id = ? AND user_id IN ? AND status = ?", tenantID, memberIDs, "active").Count(&activeCount).Error; err != nil {
			return err
		}
		if activeCount != int64(len(memberIDs)) {
			return problem.New(400, "invalid_scim_group_member", "Every SCIM Group member must be an active Tenant member.")
		}
	}
	if err := tx.WithContext(ctx).Where("tenant_id = ? AND group_id = ?", tenantID, groupID).Delete(&persistence.IdentityGroupMember{}).Error; err != nil {
		return err
	}
	for _, userID := range memberIDs {
		if err := tx.WithContext(ctx).Create(&persistence.IdentityGroupMember{TenantID: tenantID, GroupID: groupID, UserID: userID}).Error; err != nil {
			return problem.Wrap(409, "scim_group_members_update_rejected", "SCIM Group members could not be updated.", err)
		}
	}
	return nil
}

func normalizeUserInput(input UserInput) (string, string, bool, string, error) {
	email := strings.TrimSpace(input.UserName)
	if email == "" {
		for _, item := range input.Emails {
			if item.Primary || email == "" {
				email = item.Value
			}
		}
	}
	var err error
	email, err = validation.Email(email)
	if err != nil {
		return "", "", false, "", err
	}
	displayName, err := validation.Name(input.DisplayName, "invalid_scim_display_name", "SCIM displayName", 160)
	if err != nil {
		return "", "", false, "", err
	}
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	externalID := strings.TrimSpace(input.ExternalID)
	if len(externalID) > 500 {
		return "", "", false, "", problem.New(400, "invalid_scim_external_id", "SCIM externalId must not exceed 500 characters.")
	}
	return email, displayName, active, externalID, nil
}

func normalizeGroupInput(input GroupInput) (GroupInput, []uuid.UUID, error) {
	var err error
	input.DisplayName, err = validation.Name(input.DisplayName, "invalid_scim_group_name", "SCIM Group displayName", 160)
	if err != nil {
		return GroupInput{}, nil, err
	}
	input.ExternalID = strings.TrimSpace(input.ExternalID)
	if len(input.ExternalID) > 500 {
		return GroupInput{}, nil, problem.New(400, "invalid_scim_external_id", "SCIM externalId must not exceed 500 characters.")
	}
	unique := make(map[uuid.UUID]struct{}, len(input.Members))
	for _, member := range input.Members {
		id, err := uuid.Parse(strings.TrimSpace(member.Value))
		if err != nil {
			return GroupInput{}, nil, problem.New(400, "invalid_scim_group_member", "SCIM Group member value must be a User ID.")
		}
		unique[id] = struct{}{}
	}
	ids := make([]uuid.UUID, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	return input, ids, nil
}

func toUser(model persistence.User, active bool, externalID *string) User {
	external := ""
	if externalID != nil {
		external = *externalID
	}
	return User{Schemas: []string{userSchema}, ID: model.ID.String(), ExternalID: external, UserName: model.Email, DisplayName: model.DisplayName, Active: active, Emails: []Email{{Value: model.Email, Type: "work", Primary: true}}, Meta: resourceMeta("User", model.CreatedAt, model.UpdatedAt)}
}

func resourceMeta(resourceType string, createdAt, updatedAt time.Time) map[string]any {
	return map[string]any{"resourceType": resourceType, "created": createdAt, "lastModified": updatedAt}
}

func requireScope(principal serviceaccounts.Principal, scopes ...string) error {
	for _, scope := range scopes {
		if principal.Allows(scope) {
			return nil
		}
	}
	return problem.New(403, "insufficient_service_account_scope", "Service Account scope does not permit this SCIM operation.")
}

func normalizePage(startIndex, count int) (int, int) {
	if startIndex < 1 {
		startIndex = 1
	}
	if count < 1 {
		count = 100
	}
	if count > 200 {
		count = 200
	}
	return startIndex, count
}

func scimIssuer(tenantID uuid.UUID) string { return "urn:synara:scim:" + tenantID.String() }

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

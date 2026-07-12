package scim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
)

func TestSCIMProvisioningStaysTenantScopedAndProtectsFinalOwner(t *testing.T) {
	db, principal, ownerID := setupSCIMTest(t)
	service := NewService(db)
	active := true
	created, err := service.CreateUser(context.Background(), principal, UserInput{
		ExternalID: "directory-user-1", UserName: "member@example.com", DisplayName: "Directory Member", Active: &active,
	}, "scim-user-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.MustParse(created.ID)
	group, err := service.CreateGroup(context.Background(), principal, GroupInput{
		ExternalID: "directory-group-1", DisplayName: "Engineering", Members: []Member{{Value: memberID.String()}},
	}, "scim-group-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(group.Members) != 1 || group.Members[0].Value != memberID.String() {
		t.Fatalf("unexpected SCIM group membership: %#v", group)
	}
	var membership persistence.TenantMembership
	if err := db.Where("tenant_id = ? AND user_id = ?", principal.TenantID, memberID).Take(&membership).Error; err != nil || membership.Role != "member" || membership.Status != "active" {
		t.Fatalf("unexpected provisioned membership: %#v %v", membership, err)
	}
	inactive := false
	_, err = service.ReplaceUser(context.Background(), principal, ownerID, UserInput{UserName: "owner@example.com", DisplayName: "Owner", Active: &inactive}, "scim-owner-suspend", "127.0.0.1")
	if problemCode(err) != "last_tenant_owner" {
		t.Fatalf("SCIM suspended the final Tenant Owner: %v", err)
	}
}

func setupSCIMTest(t *testing.T) (*gorm.DB, serviceaccounts.Principal, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	ownerID, tenantID, serviceAccountID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	for _, model := range []any{
		&persistence.User{ID: ownerID, Email: "owner@example.com", DisplayName: "Owner", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "scim-test", Name: "SCIM Test", Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: ownerID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: ownerID, Role: "owner", Status: "active", JoinedAt: &now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, serviceaccounts.Principal{ID: serviceAccountID, TenantID: tenantID, Name: "SCIM", Scopes: map[string]struct{}{"scim.read": {}, "scim.write": {}}}, ownerID
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

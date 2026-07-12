package persistence_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestPhase2CompositeForeignKeysRejectCrossTenantAssociations(t *testing.T) {
	db := openIntegrationDB(t)

	rollback := errors.New("rollback phase 2 test")
	err := db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		firstUser := phase2User(uuid.NewString()+"@example.com", now)
		secondUser := phase2User(uuid.NewString()+"@example.com", now)
		if err := tx.Create(&[]persistence.User{firstUser, secondUser}).Error; err != nil {
			return err
		}
		firstTenant, firstOrganization, err := phase2Tenant(tx, firstUser, now)
		if err != nil {
			return err
		}
		secondTenant, secondOrganization, err := phase2Tenant(tx, secondUser, now)
		if err != nil {
			return err
		}
		firstTarget := persistence.ExecutionTarget{
			ID: uuid.New(), TenantID: &firstTenant, OrganizationID: &firstOrganization,
			Kind: "local", Name: "first-target", Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: map[string]any{},
		}
		if err := tx.Create(&firstTarget).Error; err != nil {
			return err
		}
		crossTenantTarget := persistence.ExecutionTarget{
			ID: uuid.New(), TenantID: &firstTenant, OrganizationID: &secondOrganization,
			Kind: "ssh", Name: "invalid-target", Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: map[string]any{},
		}
		if err := tx.Transaction(func(check *gorm.DB) error {
			return check.Create(&crossTenantTarget).Error
		}); err == nil {
			t.Fatal("cross-tenant execution target organization association was accepted")
		}
		secondProject := persistence.Project{
			ID: uuid.New(), TenantID: secondTenant, OrganizationID: secondOrganization,
			Name: "Second tenant project", DefaultBranch: "main", Visibility: "organization",
			CreatedBy: secondUser.ID,
		}
		if err := tx.Create(&secondProject).Error; err != nil {
			return err
		}

		crossTenantSession := persistence.AgentSession{
			ID: uuid.New(), TenantID: firstTenant, OrganizationID: firstOrganization,
			ProjectID: secondProject.ID, CreatedBy: firstUser.ID, Title: "Invalid session",
			Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: firstTarget.ID,
		}
		if err := tx.Create(&crossTenantSession).Error; err == nil {
			t.Fatal("cross-tenant project/session association was accepted")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("phase 2 isolation test: %v", err)
	}
}

func phase2User(email string, now time.Time) persistence.User {
	return persistence.User{
		ID: uuid.New(), Email: email, DisplayName: "Phase 2 user", Status: "active",
		EmailVerifiedAt: &now,
	}
}

func phase2Tenant(tx *gorm.DB, user persistence.User, now time.Time) (uuid.UUID, uuid.UUID, error) {
	tenantID := uuid.New()
	organizationID := uuid.New()
	slug := "test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if err := tx.Create(&persistence.Tenant{
		ID: tenantID, Slug: slug, Name: "Phase 2 tenant", Status: "active",
		PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: user.ID,
	}).Error; err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if err := tx.Create(&persistence.TenantMembership{
		TenantID: tenantID, UserID: user.ID, Role: "owner", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if err := tx.Create(&persistence.Organization{
		ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root organization",
		Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: user.ID,
	}).Error; err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if err := tx.Create(&persistence.OrganizationMembership{
		TenantID: tenantID, OrganizationID: organizationID, UserID: user.ID,
		Role: "owner", Status: "active",
	}).Error; err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return tenantID, organizationID, nil
}

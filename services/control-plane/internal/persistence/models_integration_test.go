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

func TestLoginSessionBuildsValidInsert(t *testing.T) {
	db := openIntegrationDB(t)
	now := time.Now().UTC()
	statement := db.Session(&gorm.Session{DryRun: true}).Create(&persistence.LoginSession{
		ID: uuid.New(), UserID: uuid.New(), RefreshTokenHash: []byte("hash"),
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Statement
	if statement.Error != nil {
		t.Fatal(statement.Error)
	}
	if statement.SQL.String() == "" {
		t.Fatal("GORM did not build an insert statement")
	}
}

func TestLoginSessionCanBeInserted(t *testing.T) {
	db := openIntegrationDB(t)
	rollback := errors.New("rollback test")
	err := db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		user := persistence.User{
			ID: uuid.New(), Email: uuid.NewString() + "@example.com", DisplayName: "Test user",
			Status: "active", EmailVerifiedAt: &now,
		}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		tenantID := uuid.New()
		organizationID := uuid.New()
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: "test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
			Name: "Test tenant", Status: "active", PlanCode: "free", Region: "default",
			Settings: map[string]any{}, CreatedBy: user.ID,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: user.ID, Role: "owner", Status: "active", JoinedAt: &now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.Organization{
			ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root organization",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: user.ID,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: organizationID, UserID: user.ID,
			Role: "owner", Status: "active",
		}).Error; err != nil {
			return err
		}
		ipAddress := "127.0.0.1"
		userAgent := "integration-test"
		if err := tx.Create(&persistence.LoginSession{
			ID: uuid.New(), UserID: user.ID, ActiveTenantID: &tenantID,
			RefreshTokenHash: []byte(uuid.NewString()),
			IPAddress:        &ipAddress, UserAgent: &userAgent,
			ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
		}).Error; err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("insert login session: %v", err)
	}
}

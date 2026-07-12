package bootstrap

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPersonalBootstrapIsDeterministicAndDevLoginReusesOwner(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	first, err := Ensure(ctx, store.DB(), platform.ProfilePersonal, "installation-test-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(ctx, store.DB(), platform.ProfilePersonal, "installation-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("bootstrap ids changed: first=%#v second=%#v", first, second)
	}

	identityService := identity.NewService(store.DB(), time.Hour, identity.PersonalDomain{
		UserID: first.UserID, TenantID: first.TenantID,
	})
	firstLogin, err := identityService.DevLogin(ctx, identity.DevLoginInput{
		Email: "first@example.com", DisplayName: "First Local Owner",
	}, "127.0.0.1", "test", "login-1")
	if err != nil {
		t.Fatal(err)
	}
	secondLogin, err := identityService.DevLogin(ctx, identity.DevLoginInput{
		Email: "second@example.com", DisplayName: "Second Local Owner",
	}, "127.0.0.1", "test", "login-2")
	if err != nil {
		t.Fatal(err)
	}
	if firstLogin.State.User.UserID != first.UserID || secondLogin.State.User.UserID != first.UserID {
		t.Fatal("personal dev login created another user")
	}
	if firstLogin.State.User.ActiveTenantID == nil || *firstLogin.State.User.ActiveTenantID != first.TenantID ||
		secondLogin.State.User.ActiveTenantID == nil || *secondLogin.State.User.ActiveTenantID != first.TenantID {
		t.Fatal("personal dev login created or selected another tenant")
	}

	assertCount(t, store.DB().Model(&persistence.User{}), 1)
	assertCount(t, store.DB().Model(&persistence.Tenant{}), 1)
	assertCount(t, store.DB().Model(&persistence.Organization{}), 1)
	assertCount(t, store.DB().Model(&persistence.ExecutionTarget{}).
		Where("tenant_id = ? AND organization_id = ?", first.TenantID, first.OrganizationID), 1)
}

func assertCount(t *testing.T, query *gorm.DB, expected int64) {
	t.Helper()
	var count int64
	if err := query.Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != expected {
		t.Fatalf("expected %d rows, got %d", expected, count)
	}
}

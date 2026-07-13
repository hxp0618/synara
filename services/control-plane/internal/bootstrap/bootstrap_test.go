package bootstrap

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
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
	legacyCapabilities := map[string]any{
		"workspaceModes": []string{"legacy-workspace"},
		"customPublic":   map[string]any{"enabled": true},
	}
	legacyTarget := persistence.ExecutionTarget{Capabilities: legacyCapabilities}
	if err := store.DB().Model(&persistence.ExecutionTarget{}).
		Where("id = ?", first.ExecutionTargetID).Select("capabilities").Updates(&legacyTarget).Error; err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(ctx, store.DB(), platform.ProfilePersonal, "installation-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("bootstrap ids changed: first=%#v second=%#v", first, second)
	}

	identityService := identity.NewService(store.DB(), time.Hour, 30*time.Minute, identity.PersonalDomain{
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
	assertBuiltInLocalTargetPolicy(t, store.DB(), first.ExecutionTargetID, "legacy-workspace")
}

func TestPlatformBootstrapBackfillsLocalTargetProviderPolicy(t *testing.T) {
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
	first, err := Ensure(ctx, store.DB(), platform.ProfileSingleNode, "installation-test-platform")
	if err != nil {
		t.Fatal(err)
	}
	legacyTarget := persistence.ExecutionTarget{Capabilities: map[string]any{
		"workspaceModes": []string{"platform-custom"}, "customPublic": "preserved",
	}}
	if err := store.DB().Model(&persistence.ExecutionTarget{}).
		Where("id = ?", first.ExecutionTargetID).Select("capabilities").Updates(&legacyTarget).Error; err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(ctx, store.DB(), platform.ProfileSingleNode, "installation-test-platform")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("platform bootstrap ids changed: first=%#v second=%#v", first, second)
	}
	assertBuiltInLocalTargetPolicy(t, store.DB(), first.ExecutionTargetID, "platform-custom")
}

func assertBuiltInLocalTargetPolicy(t *testing.T, db *gorm.DB, targetID uuid.UUID, workspaceMode string) {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	policy, err := executiontargets.ParseProviderPolicy(target.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.ExperimentalProviderEnabled("codex") || !policy.ExperimentalProviderEnabled("claudeAgent") ||
		len(policy.ExperimentalProviders) != 2 {
		t.Fatalf("built-in target Provider Policy = %#v", policy)
	}
	workspaceModes, ok := capabilityStringList(target.Capabilities["workspaceModes"])
	if !ok || len(workspaceModes) != 1 || workspaceModes[0] != workspaceMode {
		t.Fatalf("workspaceModes were not preserved: %#v", target.Capabilities["workspaceModes"])
	}
	if _, found := target.Capabilities["customPublic"]; !found {
		t.Fatalf("custom public capabilities were lost: %#v", target.Capabilities)
	}
}

func capabilityStringList(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return values, true
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			item, ok := value.(string)
			if !ok {
				return nil, false
			}
			result = append(result, item)
		}
		return result, true
	default:
		return nil, false
	}
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

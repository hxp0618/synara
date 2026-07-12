package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTargetAPIModelNeverExposesEncryptedConfiguration(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-test")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x23}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "ssh", Name: "build-host",
		Configuration: map[string]any{"privateKey": "secret-value", "host": "example.internal"},
		Capabilities:  map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("configuration")) || bytes.Contains(encoded, []byte("secret-value")) {
		t.Fatalf("safe target response leaked configuration: %s", encoded)
	}
	var persisted persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", created.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if len(persisted.ConfigurationEncrypted) == 0 || bytes.Contains(persisted.ConfigurationEncrypted, []byte("secret-value")) {
		t.Fatal("execution target configuration was not encrypted")
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "unsafe-capabilities",
		Capabilities: map[string]any{"accessToken": "leak"},
	}); err == nil {
		t.Fatal("secret-like public capabilities were accepted")
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		Kind: "local", Name: "tenant-wide-personal",
	}); err == nil {
		t.Fatal("personal execution target without organization ownership was accepted")
	}
}

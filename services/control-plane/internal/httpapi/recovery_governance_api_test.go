package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/recoverygovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformRecoveryGovernanceListRequiresPlatformOperatorAuthority(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "recovery-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{supportAccess: supportaccess.NewService(store.DB(), domain.TenantID), recoveryGovernance: recoverygovernance.NewService(store.DB(), domain.TenantID)}

	allowed := callRecoveryListHandler(server, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID})
	if allowed.Code != http.StatusOK {
		t.Fatalf("Recovery list status = %d, body = %s", allowed.Code, allowed.Body.String())
	}
	var payload struct {
		Items []recoverygovernance.Drill `json:"items"`
	}
	if err := json.Unmarshal(allowed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 0 {
		t.Fatalf("Recovery list = %#v", payload.Items)
	}

	otherTenant := uuid.New()
	denied := callRecoveryListHandler(server, identity.Principal{UserID: domain.UserID, ActiveTenantID: &otherTenant})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-context Recovery list status = %d, body = %s", denied.Code, denied.Body.String())
	}
}

func callRecoveryListHandler(server *Server, principal identity.Principal) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://control-plane.test/v1/platform/recovery-drills", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	server.listPlatformRecoveryDrills(response, request)
	return response
}

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
	"github.com/synara-ai/synara/services/control-plane/internal/incidentexercisegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformIncidentExerciseGovernanceListRequiresPlatformOperatorAuthority(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "incident-exercise-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		supportAccess:              supportaccess.NewService(store.DB(), domain.TenantID),
		incidentExerciseGovernance: incidentexercisegovernance.NewService(store.DB(), domain.TenantID),
	}

	allowed := callIncidentExerciseListHandler(server, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID})
	if allowed.Code != http.StatusOK {
		t.Fatalf("Incident exercise list status = %d, body = %s", allowed.Code, allowed.Body.String())
	}
	var payload struct {
		Items []incidentexercisegovernance.Exercise `json:"items"`
	}
	if err := json.Unmarshal(allowed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 0 {
		t.Fatalf("Incident exercise list = %#v", payload.Items)
	}

	otherTenant := uuid.New()
	denied := callIncidentExerciseListHandler(server, identity.Principal{UserID: domain.UserID, ActiveTenantID: &otherTenant})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-context Incident exercise list status = %d, body = %s", denied.Code, denied.Body.String())
	}
}

func callIncidentExerciseListHandler(server *Server, principal identity.Principal) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://control-plane.test/v1/platform/incident-exercises", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	server.listPlatformIncidentExercises(response, request)
	return response
}

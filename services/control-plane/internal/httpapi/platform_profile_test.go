package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestPlatformProfileEndpointExposesOnlySafeCapabilities(t *testing.T) {
	profile, _ := platform.Defaults(platform.ProfileSingleNode)
	server := &Server{config: config.Config{
		Platform: profile, DatabaseURL: "postgres://secret", SQLitePath: "/secret/metadata.sqlite",
		WorkerRegistrationToken: "worker-secret",
	}}
	recorder := httptest.NewRecorder()
	server.getPlatformProfile(recorder, httptest.NewRequest("GET", "/v1/platform/profile", nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["profile"] != "single-node" || body["metadataStore"] != "postgresql" {
		t.Fatalf("unexpected profile response: %#v", body)
	}
	for _, forbidden := range []string{"databaseUrl", "sqlitePath", "workerRegistrationToken", "installationId"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("profile response exposed %s", forbidden)
		}
	}
}

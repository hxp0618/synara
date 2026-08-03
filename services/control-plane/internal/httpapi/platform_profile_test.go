package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestPlatformProfileEndpointExposesOnlySafeCapabilities(t *testing.T) {
	profile, _ := platform.Defaults(platform.ProfileSingleNode)
	server := &Server{config: config.Config{
		Platform: profile, DatabaseURL: "postgres://secret", SQLitePath: "/secret/metadata.sqlite",
		WorkerRegistrationToken: "worker-secret", InternalStatusBoardURL: "https://status.synara.example",
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
	if body["commercializationMode"] != config.CommercializationModeInternalSelfHosted {
		t.Fatalf("unexpected commercialization mode: %#v", body["commercializationMode"])
	}
	statusBoard, ok := body["internalStatusBoard"].(map[string]any)
	if !ok || statusBoard["configured"] != true || statusBoard["url"] != "https://status.synara.example" {
		t.Fatalf("unexpected internal Status Board projection: %#v", body["internalStatusBoard"])
	}
	for _, forbidden := range []string{
		"commercialBilling", "paymentProvider", "payment", "checkout", "stripe", "billing", "statusPage",
	} {
		if _, exposed := body[forbidden]; exposed {
			t.Fatalf("internal self-hosted profile exposed a payment capability %q: %#v", forbidden, body[forbidden])
		}
	}
	for _, forbidden := range []string{"databaseUrl", "sqlitePath", "workerRegistrationToken", "installationId"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("profile response exposed %s", forbidden)
		}
	}
}

func TestPlatformProfileSuppressesDormantPaymentCapabilityForSelfHostedProduct(t *testing.T) {
	profile, _ := platform.Defaults(platform.ProfileEnterprise)
	server := &Server{config: config.Config{
		Platform: profile, CommercializationMode: config.CommercializationModeInternalSelfHosted,
	}}
	recorder := httptest.NewRecorder()
	server.getPlatformProfile(recorder, httptest.NewRequest("GET", "/v1/platform/profile", nil))

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"commercialBilling", "paymentProvider", "payment", "checkout", "stripe", "billing", "statusPage",
	} {
		if _, exposed := body[forbidden]; exposed {
			t.Fatalf("self-hosted profile exposed dormant payment capability %q: %#v", forbidden, body[forbidden])
		}
	}
	if body["commercializationMode"] != config.CommercializationModeInternalSelfHosted {
		t.Fatalf("unexpected self-hosted commercialization mode: %#v", body["commercializationMode"])
	}
	encoded := recorder.Body.String()
	for _, forbidden := range []string{"sk_test_", "whsec_", "price_enterprise123", "returnURL"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("public profile exposed commercial billing secret or configuration %q", forbidden)
		}
	}
}

func TestPlatformProfileEndpointReportsMissingInternalStatusBoardWithoutInventingURL(t *testing.T) {
	profile, _ := platform.Defaults(platform.ProfilePersonal)
	server := &Server{config: config.Config{Platform: profile}}
	recorder := httptest.NewRecorder()
	server.getPlatformProfile(recorder, httptest.NewRequest("GET", "/v1/platform/profile", nil))

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	statusBoard, ok := body["internalStatusBoard"].(map[string]any)
	if !ok || statusBoard["configured"] != false {
		t.Fatalf("unexpected missing internal Status Board projection: %#v", body["internalStatusBoard"])
	}
	if _, exists := statusBoard["url"]; exists {
		t.Fatalf("unconfigured internal Status Board invented a URL: %#v", statusBoard)
	}
	if _, legacy := body["statusPage"]; legacy {
		t.Fatalf("public profile exposed the retired Status Page contract: %#v", body["statusPage"])
	}
}

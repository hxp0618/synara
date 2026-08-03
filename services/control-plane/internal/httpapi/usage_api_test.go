package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestTenantUsageExportIsByteBoundAuditedAndTenantScoped(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	path := "/v1/tenants/" + fixture.tenantID.String() + "/usage/export.json"

	if response := fixture.request(t, http.MethodGet, path, "", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated usage export status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := fixture.request(t, http.MethodGet, path, fixture.crossTenantToken, nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant usage export status = %d, body = %s", response.Code, response.Body.String())
	}

	response := fixture.request(t, http.MethodGet, path, fixture.ownerToken, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("usage export status = %d, body = %s", response.Code, response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["tenantId"] != fixture.tenantID.String() {
		t.Fatalf("usage export tenantId = %#v", decoded["tenantId"])
	}
	digest := sha256.Sum256(response.Body.Bytes())
	if response.Header().Get("X-Synara-Usage-Export-Schema") != "json-v1" ||
		response.Header().Get("X-Synara-Usage-Export-SHA256") != hex.EncodeToString(digest[:]) ||
		response.Header().Get("X-Synara-Usage-Export-Bytes") != strconv.Itoa(response.Body.Len()) ||
		response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("usage export integrity headers = %#v bodyBytes=%d", response.Header(), response.Body.Len())
	}
	var auditRow persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND action = ?", fixture.tenantID, "tenant.usage_exported").Order("occurred_at DESC").Take(&auditRow).Error; err != nil {
		t.Fatal(err)
	}
	if auditRow.Action != "tenant.usage_exported" || len(auditRow.Metadata) == 0 {
		t.Fatalf("usage export audit row = %#v", auditRow)
	}
}

func TestTenantSupportDiagnosticExportIsAggregateOnlyAndTenantScoped(t *testing.T) {
	fixture := newBillingHTTPFixture(t)
	path := "/v1/tenants/" + fixture.tenantID.String() + "/support-diagnostic.json"

	if response := fixture.request(t, http.MethodGet, path, "", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated support diagnostic status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := fixture.request(t, http.MethodGet, path, fixture.crossTenantToken, nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant support diagnostic status = %d, body = %s", response.Code, response.Body.String())
	}

	response := fixture.request(t, http.MethodGet, path, fixture.ownerToken, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("support diagnostic status = %d, body = %s", response.Code, response.Body.String())
	}
	var decoded struct {
		SchemaVersion string `json:"schemaVersion"`
		Tenant        struct {
			ID                         string `json:"id"`
			ActiveExecutionCount       int64  `json:"activeExecutionCount"`
			ActiveCredentialCount      int64  `json:"activeCredentialCount"`
			UnavailableCredentialCount int64  `json:"unavailableCredentialCount"`
		} `json:"tenant"`
		Usage struct {
			TenantID string `json:"tenantId"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != "json-v1" || decoded.Tenant.ID != fixture.tenantID.String() || decoded.Usage.TenantID != fixture.tenantID.String() {
		t.Fatalf("support diagnostic scope = %#v", decoded)
	}
	if strings.Contains(response.Body.String(), "encryptedPayload") || strings.Contains(response.Body.String(), "inputText") {
		t.Fatalf("support diagnostic contains secret or execution payload fields: %s", response.Body.String())
	}
	digest := sha256.Sum256(response.Body.Bytes())
	if response.Header().Get("X-Synara-Support-Diagnostic-Schema") != "json-v1" ||
		response.Header().Get("X-Synara-Support-Diagnostic-SHA256") != hex.EncodeToString(digest[:]) ||
		response.Header().Get("X-Synara-Support-Diagnostic-Bytes") != strconv.Itoa(response.Body.Len()) ||
		response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("support diagnostic integrity headers = %#v bodyBytes=%d", response.Header(), response.Body.Len())
	}
	var auditRow persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND action = ?", fixture.tenantID, "tenant.support_diagnostic_exported").Order("occurred_at DESC").Take(&auditRow).Error; err != nil {
		t.Fatal(err)
	}
	if auditRow.Action != "tenant.support_diagnostic_exported" || len(auditRow.Metadata) == 0 {
		t.Fatalf("support diagnostic audit row = %#v", auditRow)
	}
}

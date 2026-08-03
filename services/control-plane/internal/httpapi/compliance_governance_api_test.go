package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/compliancegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformComplianceGovernanceSeparatesManagementAndEvidenceAuthority(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "compliance-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	security := persistence.User{ID: uuid.New(), Email: "compliance-security@example.test", DisplayName: "Compliance Security", Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := store.DB().Create(&security).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: security.ID, Role: "security_admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{
		supportAccess:        supportaccess.NewService(store.DB(), domain.TenantID),
		complianceGovernance: compliancegovernance.NewService(store.DB(), domain.TenantID),
	}
	createBody := map[string]any{
		"programKey": "soc2-http", "framework": "soc2_type2", "scopeVersion": "2026.1",
		"scopeSummary":           "Synara internal self-hosted services and the production operations supporting the bounded service.",
		"executiveSponsorUserId": domain.UserID, "auditorOrganization": "Independent Auditor LLP",
		"auditorEngagementReference": "https://evidence.example.test/auditor", "observationStart": now.Add(time.Hour),
		"observationEnd": now.AddDate(0, 6, 0), "evidenceRepositoryReference": "https://evidence.example.test/repository",
		"evidenceAccessPolicyReference": "https://evidence.example.test/access-policy", "evidenceRetentionDays": 365,
		"vendorRegisterReference": "https://evidence.example.test/vendors", "riskRegisterReference": "https://evidence.example.test/risks",
	}
	denied := callComplianceHandler(t, server.createPlatformComplianceProgram, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, nil, createBody)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("security program create status = %d, body = %s", denied.Code, denied.Body.String())
	}
	assertHTTPProblemCode(t, denied, "compliance_governance_manage_forbidden")

	created := callComplianceHandler(t, server.createPlatformComplianceProgram, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, nil, createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("program create status = %d, body = %s", created.Code, created.Body.String())
	}
	var program compliancegovernance.Program
	if err := json.Unmarshal(created.Body.Bytes(), &program); err != nil {
		t.Fatal(err)
	}
	control := callComplianceHandler(t, server.createPlatformComplianceControl, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, map[string]string{"programID": program.ID.String()}, map[string]any{
		"controlId": "CC6.1", "family": "logical_access", "title": "Logical access review",
		"description": "Quarterly review of active identities and privileged access assignments.",
		"ownerUserId": security.ID, "cadence": "quarterly",
		"evidenceRequirement": "Retain the exact access review population, decisions and remediation evidence.",
	})
	if control.Code != http.StatusCreated {
		t.Fatalf("control create status = %d, body = %s", control.Code, control.Body.String())
	}
	if err := json.Unmarshal(control.Body.Bytes(), &program); err != nil {
		t.Fatal(err)
	}
	evidence := callComplianceHandler(t, server.submitPlatformComplianceEvidence, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, map[string]string{"programID": program.ID.String()}, map[string]any{
		"controlRecordId": program.Controls[0].ID, "evidenceId": "access-review-http", "evidenceType": "access_review",
		"periodStart": now.Add(-24 * time.Hour), "periodEnd": now, "sourceReference": "https://evidence.example.test/access-review",
		"sha256": strings.Repeat("a", 64), "mediaType": "application/json", "classification": "restricted",
		"collectedAt": now, "retentionUntil": now.AddDate(2, 0, 0),
	})
	if evidence.Code != http.StatusCreated {
		t.Fatalf("security evidence submit status = %d, body = %s", evidence.Code, evidence.Body.String())
	}
	if err := json.Unmarshal(evidence.Body.Bytes(), &program); err != nil {
		t.Fatal(err)
	}
	if len(program.Controls) != 1 || len(program.Controls[0].Evidence) != 1 || program.Controls[0].Evidence[0].SubmittedBy != security.ID {
		t.Fatalf("compliance program evidence = %#v", program)
	}
}

func callComplianceHandler(t *testing.T, handler http.HandlerFunc, principal identity.Principal, pathValues map[string]string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/platform/compliance-programs", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", uuid.NewString())
	for key, value := range pathValues {
		request.SetPathValue(key, value)
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

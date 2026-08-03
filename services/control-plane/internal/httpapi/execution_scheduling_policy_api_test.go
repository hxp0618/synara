package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

func TestExecutionSchedulingPolicyErrorsMapToStableProblems(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{err: schedulingpolicy.ErrVersionConflict, status: http.StatusConflict, code: "execution_scheduling_policy_version_conflict"},
		{err: schedulingpolicy.ErrOrganizationWidensTenant, status: http.StatusConflict, code: "organization_execution_scheduling_policy_widens_tenant"},
		{err: schedulingpolicy.ErrPolicyCorrupt, status: http.StatusInternalServerError, code: "execution_scheduling_policy_corrupt"},
	}
	for _, test := range tests {
		var apiError *problem.Error
		if mapped := executionSchedulingPolicyProblem(test.err); !errors.As(mapped, &apiError) || apiError.Status != test.status || apiError.Code != test.code {
			t.Fatalf("mapped %v = %#v", test.err, mapped)
		}
	}
}

func TestTenantExecutionSchedulingPolicyRoutesAuthorizeCASAndAudit(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	auditorToken := createSchedulingPolicyHTTPUser(t, fixture, "auditor", nil, "")
	path := "/v1/tenants/" + fixture.tenantID.String() + "/execution-scheduling-policy"

	assertProblemResponse(t, schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, "", nil), http.StatusUnauthorized, "authentication_required")
	assertProblemResponse(t, schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, fixture.crossTenantToken, nil), http.StatusNotFound, "tenant_not_found")
	assertProblemResponse(t, schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, fixture.memberToken, nil), http.StatusForbidden, "tenant_forbidden")
	if response := schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, auditorToken, nil); response.Code != http.StatusOK {
		t.Fatalf("auditor GET status = %d, body = %s", response.Code, response.Body.String())
	}
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, path, auditorToken, schedulingPolicyUpdateJSON(t, 0, schedulingpolicy.UnrestrictedDocument())),
		http.StatusForbidden,
		"tenant_forbidden",
	)

	initialResponse := schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, fixture.ownerToken, nil)
	if initialResponse.Code != http.StatusOK {
		t.Fatalf("initial tenant GET status = %d, body = %s", initialResponse.Code, initialResponse.Body.String())
	}
	initial := decodeSchedulingPolicyHTTPResponse(t, initialResponse)
	if initial.Scope.ScopeKind != "tenant" || initial.Scope.ScopeID != fixture.tenantID || initial.Scope.Version != 0 ||
		initial.Scope.Digest != schedulingpolicy.UnrestrictedDigest || initial.Parent != nil {
		t.Fatalf("initial tenant policy response = %#v", initial)
	}

	document := schedulingpolicy.UnrestrictedDocument()
	document.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"cn-east-1"}}
	updateBody := schedulingPolicyUpdateJSON(t, 0, document)
	updatedResponse := schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, path, fixture.ownerToken, updateBody)
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("tenant PUT status = %d, body = %s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeSchedulingPolicyHTTPResponse(t, updatedResponse)
	if updated.Scope.Version != 1 || updated.Scope.Digest == schedulingpolicy.UnrestrictedDigest ||
		updated.Effective.Region.Mode != schedulingpolicy.ModeAllow || len(updated.Effective.Region.Values) != 1 {
		t.Fatalf("updated tenant policy response = %#v", updated)
	}
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, path, fixture.ownerToken, updateBody),
		http.StatusConflict,
		"execution_scheduling_policy_version_conflict",
	)
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, path, fixture.ownerToken, map[string]any{
			"expectedVersion": 1,
			"document":        document,
			"actorId":         uuid.New(),
		}),
		http.StatusBadRequest,
		"invalid_json",
	)

	var entry persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND action = ?", fixture.tenantID, "execution_scheduling_policy.tenant_updated").Take(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if entry.ActorID == nil || entry.ResourceID == nil || *entry.ResourceID != fixture.tenantID || entry.OrganizationID != nil ||
		entry.Metadata["digest"] != updated.Scope.Digest || entry.Metadata["scopeKind"] != "tenant" ||
		schedulingPolicyMetadataVersion(entry.Metadata) != 1 {
		t.Fatalf("tenant policy audit entry = %#v", entry)
	}
	encodedMetadata, err := json.Marshal(entry.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedMetadata), "document") || strings.Contains(string(encodedMetadata), "cn-east-1") {
		t.Fatalf("tenant policy audit leaked rule document: %s", encodedMetadata)
	}
	var tenantAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ?", fixture.tenantID, "execution_scheduling_policy.tenant_updated").
		Count(&tenantAuditCount).Error; err != nil || tenantAuditCount != 1 {
		t.Fatalf("tenant policy audit count = %d, err = %v", tenantAuditCount, err)
	}
}

func TestTenantDataResidencyStatementRouteIsServerAuthoritativeAndByteBound(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	path := "/v1/tenants/" + fixture.tenantID.String() + "/data-residency-statement"
	assertProblemResponse(t, schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, "", nil), http.StatusUnauthorized, "authentication_required")
	assertProblemResponse(t, schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, fixture.crossTenantToken, nil), http.StatusNotFound, "tenant_not_found")
	assertProblemResponse(t, schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, fixture.memberToken, nil), http.StatusForbidden, "tenant_forbidden")

	document := schedulingpolicy.UnrestrictedDocument()
	document.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"cn-east-1"}}
	if response := schedulingPolicyHTTPRequest(t, fixture, http.MethodPut,
		"/v1/tenants/"+fixture.tenantID.String()+"/execution-scheduling-policy", fixture.ownerToken,
		schedulingPolicyUpdateJSON(t, 0, document)); response.Code != http.StatusOK {
		t.Fatalf("policy update status = %d, body = %s", response.Code, response.Body.String())
	}

	response := schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, path, fixture.ownerToken, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("statement status = %d, body = %s", response.Code, response.Body.String())
	}
	var statement dataResidencyStatement
	if err := json.Unmarshal(response.Body.Bytes(), &statement); err != nil {
		t.Fatalf("decode statement: %v", err)
	}
	if statement.SchemaVersion != dataResidencyStatementSchema || statement.Tenant.ID != fixture.tenantID ||
		statement.Execution.Status != "enforced" || statement.Execution.PolicyVersion != 1 ||
		statement.Execution.PolicyDigest == schedulingpolicy.UnrestrictedDigest ||
		len(statement.Execution.AllowedRegions) != 1 || statement.Execution.AllowedRegions[0] != "cn-east-1" {
		t.Fatalf("server residency statement = %#v", statement)
	}
	if statement.GeneratedAt.IsZero() || statement.DataPlanes.Metadata != "deployment-annex-required" {
		t.Fatalf("statement authority/limitation fields = %#v", statement)
	}
	digest := sha256.Sum256(response.Body.Bytes())
	if response.Header().Get("X-Synara-Data-Residency-SHA256") != hex.EncodeToString(digest[:]) ||
		response.Header().Get("X-Synara-Data-Residency-Bytes") != strconv.Itoa(response.Body.Len()) ||
		response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("statement integrity headers = %#v body=%d", response.Header(), response.Body.Len())
	}
	var entry persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND action = ?", fixture.tenantID, "tenant.data_residency_statement_exported").Take(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if entry.ResourceID == nil || *entry.ResourceID != fixture.tenantID || entry.Metadata["sha256"] != "sha256:"+hex.EncodeToString(digest[:]) ||
		entry.Metadata["policyVersion"] != float64(1) {
		t.Fatalf("statement audit entry = %#v", entry)
	}
}

func TestOrganizationExecutionSchedulingPolicyRoutesUseExactOrganizationAuthority(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	var organization persistence.Organization
	if err := fixture.db.Where("tenant_id = ? AND archived_at IS NULL", fixture.tenantID).Take(&organization).Error; err != nil {
		t.Fatal(err)
	}
	viewerToken := createSchedulingPolicyHTTPUser(t, fixture, "member", &organization.ID, "viewer")
	organizationOwnerToken := createSchedulingPolicyHTTPUser(t, fixture, "member", &organization.ID, "owner")
	tenantPath := "/v1/tenants/" + fixture.tenantID.String() + "/execution-scheduling-policy"
	organizationPath := "/v1/tenants/" + fixture.tenantID.String() + "/organizations/" + organization.ID.String() + "/execution-scheduling-policy"

	tenantDocument := schedulingpolicy.UnrestrictedDocument()
	tenantDocument.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"cn-east-1"}}
	response := schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, tenantPath, fixture.ownerToken, schedulingPolicyUpdateJSON(t, 0, tenantDocument))
	if response.Code != http.StatusOK {
		t.Fatalf("tenant restriction status = %d, body = %s", response.Code, response.Body.String())
	}

	if response := schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, organizationPath, viewerToken, nil); response.Code != http.StatusOK {
		t.Fatalf("organization viewer GET status = %d, body = %s", response.Code, response.Body.String())
	}
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, organizationPath, viewerToken, schedulingPolicyUpdateJSON(t, 0, schedulingpolicy.UnrestrictedDocument())),
		http.StatusForbidden,
		"organization_forbidden",
	)

	organizationDocument := schedulingpolicy.UnrestrictedDocument()
	organizationDocument.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"cn-east-1"}}
	organizationDocument.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	updateBody := schedulingPolicyUpdateJSON(t, 0, organizationDocument)
	response = schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, organizationPath, organizationOwnerToken, updateBody)
	if response.Code != http.StatusOK {
		t.Fatalf("organization PUT status = %d, body = %s", response.Code, response.Body.String())
	}
	updated := decodeSchedulingPolicyHTTPResponse(t, response)
	if updated.Scope.ScopeKind != "organization" || updated.Scope.ScopeID != organization.ID || updated.Scope.Version != 1 ||
		updated.Parent == nil || updated.Parent.Version != 1 || updated.Parent.ScopeID != fixture.tenantID ||
		updated.Effective.Provider.Mode != schedulingpolicy.ModeAllow {
		t.Fatalf("organization policy response = %#v", updated)
	}
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, organizationPath, organizationOwnerToken, updateBody),
		http.StatusConflict,
		"execution_scheduling_policy_version_conflict",
	)

	widening := schedulingpolicy.UnrestrictedDocument()
	widening.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"cn-west-1"}}
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodPut, organizationPath, organizationOwnerToken, schedulingPolicyUpdateJSON(t, 1, widening)),
		http.StatusConflict,
		"organization_execution_scheduling_policy_widens_tenant",
	)

	foreignOrganizationID := createForeignSchedulingPolicyOrganization(t, fixture.db, fixture.tenantID)
	foreignPath := "/v1/tenants/" + fixture.tenantID.String() + "/organizations/" + foreignOrganizationID.String() + "/execution-scheduling-policy"
	assertProblemResponse(
		t,
		schedulingPolicyHTTPRequest(t, fixture, http.MethodGet, foreignPath, fixture.ownerToken, nil),
		http.StatusNotFound,
		"organization_not_found",
	)

	var entry persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND organization_id = ? AND action = ?", fixture.tenantID, organization.ID, "execution_scheduling_policy.organization_updated").Take(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if entry.ResourceID == nil || *entry.ResourceID != organization.ID || schedulingPolicyMetadataVersion(entry.Metadata) != 1 ||
		entry.Metadata["digest"] != updated.Scope.Digest || entry.Metadata["scopeKind"] != "organization" {
		t.Fatalf("organization policy audit entry = %#v", entry)
	}
	var organizationAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND organization_id = ? AND action = ?", fixture.tenantID, organization.ID, "execution_scheduling_policy.organization_updated").
		Count(&organizationAuditCount).Error; err != nil || organizationAuditCount != 1 {
		t.Fatalf("organization policy audit count = %d, err = %v", organizationAuditCount, err)
	}
}

func createSchedulingPolicyHTTPUser(
	t *testing.T,
	fixture workerManifestHTTPFixture,
	tenantRole string,
	organizationID *uuid.UUID,
	organizationRole string,
) string {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: userID, Email: userID.String() + "@example.com", DisplayName: "Scheduling policy user",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: fixture.tenantID, UserID: userID, Role: tenantRole, Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if organizationID != nil {
			return tx.Create(&persistence.OrganizationMembership{
				TenantID: fixture.tenantID, OrganizationID: *organizationID, UserID: userID,
				Role: organizationRole, Status: "active", CreatedAt: now, UpdatedAt: now,
			}).Error
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return createWorkerManifestHTTPLogin(t, fixture.db, userID, fixture.tenantID)
}

func createForeignSchedulingPolicyOrganization(t *testing.T, db *gorm.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var tenant persistence.Tenant
	if err := db.Where("id <> ?", tenantID).Take(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	organizationID := uuid.New()
	if err := db.Create(&persistence.Organization{
		ID: organizationID, TenantID: tenant.ID, Slug: "foreign-policy-" + uuid.NewString(),
		Name: "Foreign scheduling policy organization", Kind: "team", Status: "active",
		Settings: map[string]any{}, CreatedBy: tenant.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return organizationID
}

func schedulingPolicyHTTPRequest(
	t *testing.T,
	fixture workerManifestHTTPFixture,
	method, path, token string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var encoded string
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = string(payload)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(encoded))
	if token != "" {
		request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: token})
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	return response
}

func schedulingPolicyUpdateJSON(t *testing.T, expectedVersion int64, document schedulingpolicy.Document) map[string]any {
	t.Helper()
	return map[string]any{"expectedVersion": expectedVersion, "document": document}
}

func decodeSchedulingPolicyHTTPResponse(t *testing.T, response *httptest.ResponseRecorder) executionSchedulingPolicyResponse {
	t.Helper()
	var decoded executionSchedulingPolicyResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode scheduling policy response %s: %v", response.Body.String(), err)
	}
	return decoded
}

func schedulingPolicyMetadataVersion(metadata map[string]any) int64 {
	value, _ := metadata["version"].(float64)
	return int64(value)
}

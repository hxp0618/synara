package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6candidate"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformReleaseGovernanceSeparatesManagementAndReviewAuthority(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "release-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	security := persistence.User{
		ID: uuid.New(), Email: "release-security@example.test", DisplayName: "Release Security",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&security).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: security.ID, Role: "security_admin", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: security.ID, AuthorityKey: "release.security",
		Status: "active", Version: 1, ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: domain.UserID,
		Reason: "Owner assigned the exact Release Security approval function.", EvidenceReference: "https://evidence.example.test/authority/release-security",
		EvidenceSHA256: stage6authority.DigestPointer("http-release-security"),
		CreatedAt:      now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{
		supportAccess:     supportaccess.NewService(store.DB(), domain.TenantID),
		releaseGovernance: releasegovernance.NewService(store.DB(), domain.TenantID),
	}

	createBody := releaseCandidateHTTPCreateBody(t)
	denied := callReleaseGovernanceHandler(t, server.createPlatformReleaseCandidate, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, "", createBody)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("security candidate create status = %d, body = %s", denied.Code, denied.Body.String())
	}
	assertHTTPProblemCode(t, denied, "release_governance_manage_forbidden")
	unboundCommercialBody := make(map[string]any, len(createBody))
	for key, value := range createBody {
		unboundCommercialBody[key] = value
	}
	unboundCommercialBody["impactDomains"] = []string{"code_change", "provider_commercial"}
	unboundCommercial := callReleaseGovernanceHandler(
		t, server.createPlatformReleaseCandidate,
		identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, "", unboundCommercialBody,
	)
	if unboundCommercial.Code != http.StatusBadRequest {
		t.Fatalf("unbound commercial candidate status = %d, body = %s", unboundCommercial.Code, unboundCommercial.Body.String())
	}
	assertHTTPProblemCode(t, unboundCommercial, "release_provider_authorizations_invalid")

	created := callReleaseGovernanceHandler(t, server.createPlatformReleaseCandidate, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, "", createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("candidate create status = %d, body = %s", created.Code, created.Body.String())
	}
	var candidate releasegovernance.Candidate
	if err := json.Unmarshal(created.Body.Bytes(), &candidate); err != nil {
		t.Fatal(err)
	}
	if !candidate.EvidenceReceiptBound || candidate.EvidenceBundleSchema != "synara.stage6-candidate-evidence-bundle-validation.v5" ||
		candidate.DesktopArtifactSetSHA256 != "sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("HTTP candidate did not expose its verified evidence binding: %#v", candidate)
	}
	deniedFinalReview := callReleaseGovernanceHandler(t, server.recordPlatformReleaseFinalReview, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, candidate.ID.String(), map[string]any{
		"receiptSha256": "sha256:" + strings.Repeat("a", 64), "receiptBase64": "e30=",
	})
	if deniedFinalReview.Code != http.StatusForbidden {
		t.Fatalf("security Final Review bind status = %d, body = %s", deniedFinalReview.Code, deniedFinalReview.Body.String())
	}
	assertHTTPProblemCode(t, deniedFinalReview, "release_governance_manage_forbidden")
	prematureFinalReview := callReleaseGovernanceHandler(t, server.recordPlatformReleaseFinalReview, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, candidate.ID.String(), map[string]any{
		"receiptSha256": "sha256:" + strings.Repeat("a", 64), "receiptBase64": "e30=",
	})
	if prematureFinalReview.Code != http.StatusConflict {
		t.Fatalf("premature Final Review bind status = %d, body = %s", prematureFinalReview.Code, prematureFinalReview.Body.String())
	}
	assertHTTPProblemCode(t, prematureFinalReview, "release_final_review_not_recordable")
	readinessResponse := callReleaseReadinessHandler(
		t, server.getPlatformReleaseCandidateReadiness,
		identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, candidate.ID.String(),
	)
	if readinessResponse.Code != http.StatusOK {
		t.Fatalf("candidate readiness status = %d, body = %s", readinessResponse.Code, readinessResponse.Body.String())
	}
	var readiness releasegovernance.Readiness
	if err := json.Unmarshal(readinessResponse.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if len(readiness.Gates) != 10 || readiness.Gates[0].ID != "candidate_evidence" ||
		!readiness.Gates[0].Satisfied || readiness.Gates[1].ID != "provider_commercial_authorizations" ||
		!readiness.Gates[1].Satisfied || readiness.Gates[2].Satisfied ||
		readiness.FinalReviewGate.ID != "final_review" || readiness.FinalReviewGate.Satisfied ||
		readiness.ReleaseTransitionEligible {
		t.Fatalf("candidate readiness = %#v", readiness)
	}
	ready := callReleaseGovernanceHandler(t, server.transitionPlatformReleaseCandidate, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, candidate.ID.String(), map[string]any{
		"expectedVersion": candidate.Version, "targetState": "ready_for_review",
		"reason": "Submit the exact candidate for separated role review.",
	})
	if ready.Code != http.StatusOK {
		t.Fatalf("candidate review transition status = %d, body = %s", ready.Code, ready.Body.String())
	}

	reviewed := callReleaseGovernanceHandler(t, server.recordPlatformReleaseApproval, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, candidate.ID.String(), map[string]any{
		"role": "security", "decision": "approved", "reason": "Security evidence is complete for the exact candidate.",
		"evidenceReference": "https://evidence.example.test/security",
		"evidenceSha256":    "sha256:" + strings.Repeat("e", 64),
	})
	if reviewed.Code != http.StatusOK {
		t.Fatalf("security candidate review status = %d, body = %s", reviewed.Code, reviewed.Body.String())
	}
	if err := json.Unmarshal(reviewed.Body.Bytes(), &candidate); err != nil {
		t.Fatal(err)
	}
	if len(candidate.Approvals) != 1 || candidate.Approvals[0].EvidenceSHA256 == nil ||
		*candidate.Approvals[0].EvidenceSHA256 != "sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("release approval evidence digest = %#v", candidate.Approvals)
	}
	if len(candidate.Approvals) != 1 || candidate.Approvals[0].ApproverUserID != security.ID {
		t.Fatalf("reviewed candidate = %#v", candidate)
	}
}

func callReleaseReadinessHandler(
	t *testing.T,
	handler http.HandlerFunc,
	principal identity.Principal,
	candidateRecordID string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://control-plane.test/v1/platform/release-candidates/"+candidateRecordID+"/readiness", nil)
	request.SetPathValue("candidateRecordID", candidateRecordID)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func releaseCandidateHTTPCreateBody(t *testing.T) map[string]any {
	t.Helper()
	candidateID := "v0.6.3-stage6.http"
	environmentID := "stage6/http-test"
	encoded, digest, err := stage6candidate.Receipt(candidateID, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"candidateId": candidateID, "sourceCommit": strings.Repeat("a", 40),
		"lockfileSha256":              "sha256:" + strings.Repeat("b", 64),
		"evidenceBundleSha256":        digest,
		"evidenceBundleReceiptBase64": base64.StdEncoding.EncodeToString(encoded),
		"finalAssetSetSha256":         "sha256:" + strings.Repeat("d", 64),
		"environmentId":               environmentID, "impactDomains": []string{"code_change"},
	}
}

func callReleaseGovernanceHandler(
	t *testing.T,
	handler http.HandlerFunc,
	principal identity.Principal,
	candidateRecordID string,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/platform/release-candidates", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", uuid.NewString())
	if candidateRecordID != "" {
		request.SetPathValue("candidateRecordID", candidateRecordID)
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

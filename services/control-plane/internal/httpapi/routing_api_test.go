package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

func TestPlatformRoutingAuthorityRouteUsesSignedPublisherWithoutTenantSession(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	targetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: targetID, Kind: "kubernetes", Name: "platform-signed-routing", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const publisherIdentity = "platform-routing-http-publisher"
	const keyID = "platform-routing-http-key-v1"
	tenantID := fixture.tenantID
	service, err := routing.NewPlatformAuthorityService(fixture.db, []routing.PlatformAuthorityPublisherConfig{{
		PublisherIdentity: publisherIdentity,
		Keys:              []routing.PlatformAuthorityPublisherKey{{KeyID: keyID, PublicKey: publicKey}},
		Targets: []routing.PlatformAuthorityTargetScope{{
			ExecutionTargetID: targetID,
			Ownership:         routing.PlatformAuthorityOwnershipShared,
			PublishHealth:     true,
			DRRoutes: []routing.PlatformAuthorityDRRouteScope{{
				SourceDRDomain: "region-a/cluster-a", DRDomain: "region-b/cluster-b",
			}},
		}, {
			ExecutionTargetID: fixture.targetID,
			Ownership:         routing.PlatformAuthorityOwnershipTenantOwned,
			TenantID:          &tenantID,
			PublishHealth:     true,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.platformRouting = service
	publication, err := routing.SignPlatformAuthorityPublication(privateKey, routing.PlatformAuthorityPublication{
		SchemaVersion: routing.PlatformAuthoritySchemaVersionV1, PublisherIdentity: publisherIdentity,
		KeyID: keyID, Nonce: uuid.NewString(), IssuedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), ExecutionTargetID: targetID.String(),
		ObservedAt: now.Format(time.RFC3339Nano),
		Health: &routing.PlatformAuthorityHealth{
			Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
			AvailableCapacityUnits: routingIntPointer(8), AllocatedCapacityUnits: 2, TTLSeconds: 120,
		},
		DRReadiness: []routing.PlatformAuthorityDRReadiness{{
			SourceDRDomain: "region-a/cluster-a", DRDomain: "region-b/cluster-b",
			ReplicatedThroughAt: now.Add(-time.Second).Format(time.RFC3339Nano),
			ArtifactsReady:      true, CheckpointsReady: true, MemoryReady: true, TTLSeconds: 120,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/platform/routing-authority/execution-targets/" + targetID.String() + "/observations"
	publish := func(value routing.PlatformAuthorityPublication) *httptest.ResponseRecorder {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, request)
		return recorder
	}

	accepted := publish(publication)
	if accepted.Code != http.StatusOK {
		t.Fatalf("signed publication status = %d body=%s", accepted.Code, accepted.Body.String())
	}
	var result routing.PlatformAuthorityPublicationResult
	if err := json.Unmarshal(accepted.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Replayed || result.ExecutionTargetID != targetID || result.Health == nil ||
		len(result.DRReadiness) != 1 {
		t.Fatalf("signed publication result = %#v", result)
	}
	replayed := publish(publication)
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}

	tampered := publication
	tampered.Nonce = uuid.NewString()
	assertProblemResponse(t, publish(tampered), http.StatusUnauthorized, "platform_routing_publisher_authentication_failed")

	tenantWrite := fixture.routingRequest(
		t,
		http.MethodPut,
		fixture.ownerTargetHealthPath(fixture.targetID),
		fixture.ownerToken,
		routingHTTPJSON(t, map[string]any{
			"status": routing.HealthHealthy, "capacityStatus": routing.CapacityAvailable,
			"allocatedCapacityUnits": 0, "ttlSeconds": 60,
		}),
	)
	assertProblemResponse(t, tenantWrite, http.StatusForbidden, "execution_target_health_platform_authority")
}

func TestExecutionTargetRoutingRoutesEnforceReadManageAndRejectSharedHealthWrites(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	basePath := fixture.ownerRoutingGroupsPath()
	ownedHealthPath := fixture.ownerTargetHealthPath(fixture.targetID)

	sharedTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID:                     sharedTargetID,
		Kind:                   "kubernetes",
		Name:                   "platform-shared-routing",
		Status:                 "active",
		ConfigurationEncrypted: []byte("SHARED-ROUTING-TARGET-CONFIG-SECRET"),
		Capabilities:           map[string]any{},
		CreatedAt:              now,
		UpdatedAt:              now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	routingService := routing.NewService(fixture.db)
	sharedSeed, err := routingService.ObserveHealth(context.Background(), routing.HealthObservation{
		ExecutionTargetID:      sharedTargetID,
		Status:                 routing.HealthHealthy,
		CapacityStatus:         routing.CapacityAvailable,
		AvailableCapacityUnits: routingIntPointer(9),
		AllocatedCapacityUnits: 1,
		Source:                 "seed",
		ObservedAt:             now,
		TTL:                    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	sharedHealthPath := fixture.ownerTargetHealthPath(sharedTargetID)
	healthBody := routingHTTPJSON(t, map[string]any{
		"status":                 routing.HealthUnreachable,
		"capacityStatus":         routing.CapacityUnknown,
		"availableCapacityUnits": 0,
		"allocatedCapacityUnits": 4,
		"reason":                 "tenant tried to evict the shared target",
		"observedAt":             now.Add(2 * time.Minute).Format(time.RFC3339),
		"ttlSeconds":             60,
	})

	for _, test := range []struct {
		name   string
		method string
		path   string
		token  string
		body   string
		status int
		code   string
	}{
		{name: "list unauthenticated", method: http.MethodGet, path: basePath, status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "list active tenant mismatch", method: http.MethodGet, path: basePath, token: fixture.crossTenantToken, status: http.StatusConflict, code: "active_tenant_mismatch"},
		{name: "list missing worker read", method: http.MethodGet, path: basePath, token: fixture.memberToken, status: http.StatusForbidden, code: "tenant_forbidden"},
		{name: "observe active tenant mismatch", method: http.MethodPut, path: ownedHealthPath, token: fixture.crossTenantToken, body: healthBody, status: http.StatusConflict, code: "active_tenant_mismatch"},
		{name: "observe missing worker manage", method: http.MethodPut, path: ownedHealthPath, token: fixture.readOnlyToken, body: healthBody, status: http.StatusForbidden, code: "tenant_forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertProblemResponse(t, fixture.routingRequest(t, test.method, test.path, test.token, test.body), test.status, test.code)
		})
	}

	readable := fixture.routingRequest(t, http.MethodGet, basePath, fixture.readOnlyToken, "")
	if readable.Code != http.StatusOK {
		t.Fatalf("read-only list status = %d, body = %s", readable.Code, readable.Body.String())
	}
	var initial struct {
		Items []routing.GroupState `json:"items"`
	}
	if err := json.Unmarshal(readable.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if len(initial.Items) != 0 {
		t.Fatalf("initial routing groups = %#v", initial.Items)
	}

	sharedDenied := fixture.routingRequest(t, http.MethodPut, sharedHealthPath, fixture.ownerToken, healthBody)
	assertProblemResponse(t, sharedDenied, http.StatusForbidden, "shared_execution_target_health_immutable")

	var sharedHealth persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", sharedTargetID).Take(&sharedHealth).Error; err != nil {
		t.Fatal(err)
	}
	if sharedHealth.Status != sharedSeed.Status || sharedHealth.Version != sharedSeed.Version ||
		sharedHealth.ObservedAt.UTC() != sharedSeed.ObservedAt.UTC() || sharedHealth.Source != sharedSeed.Source {
		t.Fatalf("shared health mutated: got=%#v want=%#v", sharedHealth, sharedSeed)
	}

	ownedResponse := fixture.routingRequest(t, http.MethodPut, ownedHealthPath, fixture.ownerToken, routingHTTPJSON(t, map[string]any{
		"status":                 routing.HealthDegraded,
		"capacityStatus":         routing.CapacitySaturated,
		"availableCapacityUnits": 1,
		"allocatedCapacityUnits": 7,
		"reason":                 "tenant-owned target can still be updated",
		"observedAt":             now.Add(3 * time.Minute).Format(time.RFC3339),
		"ttlSeconds":             120,
		"drReadiness": map[string]any{
			"sourceDrDomain":      "cn-shanghai/tenant-cluster",
			"drDomain":            "cn-shanghai/tenant-cluster",
			"replicatedThroughAt": now.Add(2 * time.Minute).Format(time.RFC3339),
			"checkpointsReady":    true,
			"memoryReady":         true,
			"publisherIdentity":   "tenant-router",
			"observedAt":          now.Add(3 * time.Minute).Format(time.RFC3339),
			"ttlSeconds":          120,
		},
	}))
	if ownedResponse.Code != http.StatusOK {
		t.Fatalf("tenant-owned health status = %d, body = %s", ownedResponse.Code, ownedResponse.Body.String())
	}
	var payload struct {
		ExecutionTargetID uuid.UUID                     `json:"executionTargetId"`
		Health            routing.TargetHealthView      `json:"health"`
		DRReadiness       routing.TargetDRReadinessView `json:"drReadiness"`
	}
	if err := json.Unmarshal(ownedResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ExecutionTargetID != fixture.targetID || payload.Health.Status != routing.HealthDegraded ||
		payload.Health.CapacityStatus != routing.CapacitySaturated || payload.Health.Version != 1 {
		t.Fatalf("owned health payload = %#v", payload)
	}
	if payload.DRReadiness.SourceDRDomain != "cn-shanghai/tenant-cluster" ||
		!payload.DRReadiness.CheckpointsReady || !payload.DRReadiness.MemoryReady || payload.DRReadiness.Version != 1 {
		t.Fatalf("owned dr readiness payload = %#v", payload.DRReadiness)
	}

	var ownedHealth persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&ownedHealth).Error; err != nil {
		t.Fatal(err)
	}
	if ownedHealth.Status != routing.HealthDegraded || ownedHealth.CapacityStatus != routing.CapacitySaturated ||
		ownedHealth.Version != 1 || ownedHealth.Source == "" {
		t.Fatalf("owned health row = %#v", ownedHealth)
	}
	var ownedReadiness persistence.ExecutionTargetDRReadiness
	if err := fixture.db.Where("execution_target_id = ? AND source_dr_domain = ?", fixture.targetID, "cn-shanghai/tenant-cluster").
		Take(&ownedReadiness).Error; err != nil {
		t.Fatal(err)
	}
	if !ownedReadiness.CheckpointsReady || !ownedReadiness.MemoryReady || ownedReadiness.PublisherIdentity != "tenant-router" {
		t.Fatalf("owned dr readiness row = %#v", ownedReadiness)
	}
}

func TestListExecutionTargetGroupsRouteProjectsOnlySafeTargetFields(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("configuration_encrypted", []byte("OWNED-ROUTING-TARGET-CONFIG-SECRET")).Error; err != nil {
		t.Fatal(err)
	}

	sharedTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID:                     sharedTargetID,
		Kind:                   "kubernetes",
		Name:                   "platform-shared-read",
		Status:                 "active",
		ConfigurationEncrypted: []byte("SHARED-ROUTING-TARGET-CONFIG-SECRET"),
		Capabilities:           map[string]any{},
		CreatedAt:              now,
		UpdatedAt:              now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	routingService := routing.NewService(fixture.db)
	group, err := routingService.CreateGroup(context.Background(), routing.CreateGroupInput{
		TenantID:                  fixture.tenantID,
		Name:                      "safe-routing-group-" + uuid.NewString(),
		Strategy:                  routing.StrategyPriority,
		PreferredRegions:          []string{"cn-shanghai"},
		AllowCrossRegion:          true,
		MaxFailoverAttempts:       routingIntPointer(2),
		HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []struct {
		targetID  uuid.UUID
		region    string
		clusterID string
		priority  int
	}{
		{targetID: fixture.targetID, region: "cn-shanghai", clusterID: "tenant-cluster", priority: 10},
		{targetID: sharedTargetID, region: "cn-beijing", clusterID: "shared-cluster", priority: 20},
	} {
		if _, err := routingService.AddMember(context.Background(), routing.AddMemberInput{
			TenantID:          fixture.tenantID,
			TargetGroupID:     group.ID,
			ExecutionTargetID: member.targetID,
			Region:            member.region,
			ClusterID:         member.clusterID,
			Priority:          member.priority,
			Weight:            100,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := routingService.ObserveHealth(context.Background(), routing.HealthObservation{
			ExecutionTargetID:      member.targetID,
			Status:                 routing.HealthHealthy,
			CapacityStatus:         routing.CapacityAvailable,
			AvailableCapacityUnits: routingIntPointer(5),
			AllocatedCapacityUnits: 1,
			Source:                 "seed",
			ObservedAt:             now.Add(time.Duration(member.priority) * time.Second),
			TTL:                    time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := routingService.ObserveDRReadiness(context.Background(), routing.DRReadinessObservation{
			ExecutionTargetID:   member.targetID,
			SourceDRDomain:      "cn-shanghai/tenant-cluster",
			DRDomain:            member.region + "/" + member.clusterID,
			ReplicatedThroughAt: now.Add(time.Duration(member.priority) * time.Second),
			CheckpointsReady:    true,
			PublisherIdentity:   "routing-test",
			ObservedAt:          now.Add(time.Duration(member.priority) * time.Second),
			TTL:                 time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}

	response := fixture.routingRequest(t, http.MethodGet, fixture.ownerRoutingGroupsPath(), fixture.readOnlyToken, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{
		"OWNED-ROUTING-TARGET-CONFIG-SECRET",
		"SHARED-ROUTING-TARGET-CONFIG-SECRET",
		"configurationEncrypted",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("routing list leaked %q: %s", forbidden, body)
		}
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, envelope, "items")
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["items"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("routing items = %#v", items)
	}
	assertJSONKeys(t, items[0], "group", "members")

	var members []map[string]json.RawMessage
	if err := json.Unmarshal(items[0]["members"], &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("routing members = %#v", members)
	}
	for _, member := range members {
		assertJSONKeys(t, member, "drReadinessAuthorities", "health", "member", "target")
		var target map[string]json.RawMessage
		if err := json.Unmarshal(member["target"], &target); err != nil {
			t.Fatal(err)
		}
		for _, forbiddenKey := range []string{"capabilities", "configuration", "configurationEncrypted"} {
			if _, exists := target[forbiddenKey]; exists {
				t.Fatalf("routing target leaked %q: %s", forbiddenKey, member["target"])
			}
		}
		var targetID uuid.UUID
		if err := json.Unmarshal(target["id"], &targetID); err != nil {
			t.Fatal(err)
		}
		switch targetID {
		case fixture.targetID:
			assertJSONKeys(t, target, "id", "kind", "name", "organizationId", "status", "tenantId")
		case sharedTargetID:
			assertJSONKeys(t, target, "id", "kind", "name", "status")
		default:
			t.Fatalf("unexpected routing target %s", targetID)
		}
		var readinessAuthorities []routing.TargetDRReadinessView
		if err := json.Unmarshal(member["drReadinessAuthorities"], &readinessAuthorities); err != nil {
			t.Fatal(err)
		}
		if len(readinessAuthorities) != 1 || readinessAuthorities[0].SourceDRDomain != "cn-shanghai/tenant-cluster" {
			t.Fatalf("routing readiness authorities = %#v", readinessAuthorities)
		}
	}
}

func TestLocationOutageRoutesEnforceReadManageAndProjectCurrentAuthority(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	basePath := fixture.ownerLocationOutagesPath()
	body := routingHTTPJSON(t, map[string]any{
		"region":     "cn-shanghai",
		"clusterId":  "cluster-a",
		"status":     routing.LocationStatusDraining,
		"reason":     "planned evacuation",
		"observedAt": now.Format(time.RFC3339),
		"ttlSeconds": 120,
	})

	for _, test := range []struct {
		name   string
		method string
		token  string
		status int
		code   string
		body   string
	}{
		{name: "list unauthenticated", method: http.MethodGet, status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "list active tenant mismatch", method: http.MethodGet, token: fixture.crossTenantToken, status: http.StatusConflict, code: "active_tenant_mismatch"},
		{name: "list missing worker read", method: http.MethodGet, token: fixture.memberToken, status: http.StatusForbidden, code: "tenant_forbidden"},
		{name: "observe active tenant mismatch", method: http.MethodPut, token: fixture.crossTenantToken, status: http.StatusConflict, code: "active_tenant_mismatch", body: body},
		{name: "observe missing worker manage", method: http.MethodPut, token: fixture.readOnlyToken, status: http.StatusForbidden, code: "tenant_forbidden", body: body},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertProblemResponse(t, fixture.routingRequest(t, test.method, basePath, test.token, test.body), test.status, test.code)
		})
	}

	readable := fixture.routingRequest(t, http.MethodGet, basePath, fixture.readOnlyToken, "")
	if readable.Code != http.StatusOK {
		t.Fatalf("initial location outage list status = %d, body = %s", readable.Code, readable.Body.String())
	}
	var initial struct {
		Items []routing.LocationOutageView `json:"items"`
	}
	if err := json.Unmarshal(readable.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if len(initial.Items) != 0 {
		t.Fatalf("initial location outages = %#v", initial.Items)
	}

	response := fixture.routingRequest(t, http.MethodPut, basePath, fixture.ownerToken, body)
	if response.Code != http.StatusOK {
		t.Fatalf("observe location outage status = %d, body = %s", response.Code, response.Body.String())
	}
	var observed routing.LocationOutageView
	if err := json.Unmarshal(response.Body.Bytes(), &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Region != "cn-shanghai" || observed.ClusterID == nil || *observed.ClusterID != "cluster-a" ||
		observed.Status != routing.LocationStatusDraining || observed.Version != 1 {
		t.Fatalf("observed location outage = %#v", observed)
	}
	if !strings.HasPrefix(observed.PublisherIdentity, "operator:") {
		t.Fatalf("default publisher identity = %q", observed.PublisherIdentity)
	}

	var stored persistence.ExecutionLocationOutage
	if err := fixture.db.Where("tenant_id = ? AND region = ? AND cluster_id = ?", fixture.tenantID, "cn-shanghai", "cluster-a").
		Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != routing.LocationStatusDraining || stored.PublisherIdentity != observed.PublisherIdentity {
		t.Fatalf("stored location outage = %#v", stored)
	}

	listed := fixture.routingRequest(t, http.MethodGet, basePath, fixture.readOnlyToken, "")
	if listed.Code != http.StatusOK {
		t.Fatalf("location outage list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	var listedBody struct {
		Items []routing.LocationOutageView `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listedBody); err != nil {
		t.Fatal(err)
	}
	if len(listedBody.Items) != 1 || listedBody.Items[0].Region != "cn-shanghai" {
		t.Fatalf("listed location outages = %#v", listedBody.Items)
	}
}

func TestTargetGroupMemberLifecycleRouteUsesManagePermissionCASAndAudit(t *testing.T) {
	fixture := newWorkerManifestHTTPFixture(t)
	routingService := routing.NewService(fixture.db)
	group, err := routingService.CreateGroup(context.Background(), routing.CreateGroupInput{
		TenantID: fixture.tenantID, Name: "member lifecycle", Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	member, err := routingService.AddMember(context.Background(), routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: fixture.targetID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := fixture.ownerRoutingGroupMemberPath(group.ID, member.ID)
	drainBody := routingHTTPJSON(t, map[string]any{
		"expectedVersion": 1,
		"status":          routing.MemberStatusDraining,
	})

	for _, test := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "active tenant mismatch", token: fixture.crossTenantToken, status: http.StatusConflict, code: "active_tenant_mismatch"},
		{name: "missing worker manage", token: fixture.readOnlyToken, status: http.StatusForbidden, code: "tenant_forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertProblemResponse(
				t,
				fixture.routingRequest(t, http.MethodPatch, path, test.token, drainBody),
				test.status,
				test.code,
			)
		})
	}

	drained := fixture.routingRequest(t, http.MethodPatch, path, fixture.ownerToken, drainBody)
	if drained.Code != http.StatusOK {
		t.Fatalf("drain status = %d body=%s", drained.Code, drained.Body.String())
	}
	var view routing.ExecutionTargetGroupMemberView
	if err := json.Unmarshal(drained.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ID != member.ID || view.Status != routing.MemberStatusDraining || view.Version != 2 {
		t.Fatalf("drained member = %#v", view)
	}

	replayed := fixture.routingRequest(t, http.MethodPatch, path, fixture.ownerToken, drainBody)
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("drain replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}
	staleAbort := fixture.routingRequest(t, http.MethodPatch, path, fixture.ownerToken, routingHTTPJSON(t, map[string]any{
		"expectedVersion": 1,
		"status":          routing.MemberStatusActive,
	}))
	assertProblemResponse(t, staleAbort, http.StatusConflict, "target_group_member_version_conflict")

	aborted := fixture.routingRequest(t, http.MethodPatch, path, fixture.ownerToken, routingHTTPJSON(t, map[string]any{
		"expectedVersion": 2,
		"status":          routing.MemberStatusActive,
	}))
	if aborted.Code != http.StatusOK {
		t.Fatalf("abort drain status = %d body=%s", aborted.Code, aborted.Body.String())
	}
	if err := json.Unmarshal(aborted.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != routing.MemberStatusActive || view.Version != 3 {
		t.Fatalf("reactivated member = %#v", view)
	}

	var audits []persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND resource_id = ?", fixture.tenantID, member.ID).
		Order("occurred_at, event_id").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 || audits[0].Action != "execution_target_group_member.drain_started" ||
		audits[1].Action != "execution_target_group_member.drain_aborted" {
		t.Fatalf("member lifecycle audits = %#v", audits)
	}
}

func (f workerManifestHTTPFixture) ownerRoutingGroupsPath() string {
	return "/v1/tenants/" + f.tenantID.String() + "/execution-target-groups"
}

func (f workerManifestHTTPFixture) ownerTargetHealthPath(targetID uuid.UUID) string {
	return "/v1/tenants/" + f.tenantID.String() + "/execution-targets/" + targetID.String() + "/health-observation"
}

func (f workerManifestHTTPFixture) ownerRoutingGroupMemberPath(groupID, memberID uuid.UUID) string {
	return "/v1/tenants/" + f.tenantID.String() + "/execution-target-groups/" + groupID.String() +
		"/members/" + memberID.String()
}

func (f workerManifestHTTPFixture) ownerLocationOutagesPath() string {
	return "/v1/tenants/" + f.tenantID.String() + "/location-outages"
}

func (f workerManifestHTTPFixture) routingRequest(
	t *testing.T,
	method, path, token, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.AddCookie(&http.Cookie{Name: f.cookieName, Value: token})
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func routingHTTPJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func routingIntPointer(value int) *int { return &value }

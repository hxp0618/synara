package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestGatherUsesBoundedRoutePatternsAndAuthoritativeState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	models := []any{
		&persistence.AgentExecution{}, &persistence.WorkerInstance{}, &persistence.WorkerLease{},
		&persistence.ExecutionTarget{}, &persistence.OutboxMessage{}, &persistence.SSEConnectionLease{},
		&persistence.LoginSession{}, &persistence.Artifact{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	workerID := uuid.New()
	staleWorkerID := uuid.New()
	freshWorkerID := uuid.New()
	executionID := uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.ExecutionTarget{ID: targetID, Kind: "docker", Name: "test", Status: "active", Capabilities: map[string]any{}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerInstance{
		ID: workerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
		Namespace: "test", PodName: "worker", Version: "test", ProtocolVersion: 1, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"), Status: "ready",
		RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, worker := range []persistence.WorkerInstance{
		{
			ID: staleWorkerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
			Namespace: "test", PodName: "stale-worker", Version: "test", ProtocolVersion: 2,
			Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: []byte("stale-hash"), Status: "online", RegisteredAt: now,
			LastHeartbeatAt: now.Add(-2 * time.Minute),
		},
		{
			ID: freshWorkerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
			Namespace: "test", PodName: "fresh-worker", Version: "test", ProtocolVersion: 2,
			Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: []byte("fresh-hash"), Status: "online", RegisteredAt: now,
			LastHeartbeatAt: now,
		},
	} {
		if err := db.Create(&worker).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(), Attempt: 1,
		Status: "running", ExecutionTargetID: targetID, TargetKind: "docker", WorkerID: &workerID,
		Generation: 1, RequestedBy: uuid.New(), QueuedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerLease{
		ExecutionID: executionID, TenantID: uuid.New(), WorkerID: workerID, Generation: 1,
		LeaseTokenHash: []byte("hash"), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	deadAt := now
	if err := db.Create(&persistence.OutboxMessage{
		ID: uuid.New(), Topic: "execution.queued", MessageKey: uuid.NewString(),
		Payload: map[string]any{}, Headers: map[string]any{}, Attempts: 2,
		AvailableAt: now, CreatedAt: now.Add(-time.Minute), DeadLetteredAt: &deadAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.OutboxMessage{
		ID: uuid.New(), Topic: "execution.queued", MessageKey: uuid.NewString(),
		Payload: map[string]any{}, Headers: map[string]any{}, Attempts: 1,
		AvailableAt: now, CreatedAt: now.Add(-30 * time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}

	registry := New(db, Config{SessionIdleTTL: 7 * 24 * time.Hour, WorkerHeartbeatTimeout: 90 * time.Second})
	registry.ObserveHTTP("GET", "GET /v1/sessions/{sessionID}", 200, 25*time.Millisecond, "")
	registry.ObserveHTTP("GET", "/v1/sessions/"+executionID.String(), 404, 10*time.Millisecond, "")
	registry.ObserveBackground("docker", now, nil)
	registry.ObserveArtifact("complete", 128, nil)
	registry.ObserveSSECatchup(20*time.Millisecond, 3, nil)
	registry.ObserveSSELimit("user")
	payload, err := registry.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := string(payload)
	for _, expected := range []string{
		`route="/v1/sessions/{sessionID}"`, `route="unmatched"`,
		`synara_workers{status="ready",target_kind="docker"} 1`,
		`synara_stale_workers{status="online",target_kind="docker"} 1`,
		`synara_executions{status="running",target_kind="docker"} 1`,
		`synara_worker_leases{state="active"} 1`, `synara_metrics_collection_success 1`,
		`synara_outbox_pending 1`, `synara_outbox_retrying 1`,
		`synara_outbox_dead_letter 1`, `synara_outbox_oldest_pending_seconds`,
		`synara_sse_connections{state="active"} 0`, `synara_artifact_ready_bytes 0`,
		`synara_database_connections{state="open"}`, `synara_artifact_operations_total{operation="complete",result="success"} 1`,
		`synara_sse_catchup_events_total 3`, `synara_sse_connection_rejections_total{scope="user"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	if strings.Contains(metrics, executionID.String()) {
		t.Fatalf("metrics leaked a high-cardinality execution identifier:\n%s", metrics)
	}
}

func TestObserveHTTPDerivesBoundedLoginLeaseFencingAndEventMetrics(t *testing.T) {
	registry := New(nil)
	registry.ObserveHTTP("POST", "POST /v1/auth/dev-login", 200, 10*time.Millisecond, "")
	registry.ObserveHTTP("GET", "GET /v1/auth/sso/{connectionID}/callback", 401, 15*time.Millisecond, "oidc_id_token_invalid")
	registry.ObserveHTTP("POST", "POST /v1/auth/sso/{connectionID}/callback", 303, 20*time.Millisecond, "")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/renew", 200, 5*time.Millisecond, "")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/renew", 409, 7*time.Millisecond, "generation_fenced")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/events", 201, 25*time.Millisecond, "")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/events", 409, 30*time.Millisecond, "lease_expired")
	registry.ObserveHTTP("POST", "POST /v1/workers/heartbeat", 409, 3*time.Millisecond, "worker_incarnation_fenced")

	var output bytes.Buffer
	registry.writeProcessMetrics(&output)
	metrics := output.String()
	for _, expected := range []string{
		`synara_login_attempts_total{method="dev",result="success"} 1`,
		`synara_login_attempts_total{method="oidc",result="failure"} 1`,
		`synara_login_attempts_total{method="saml",result="success"} 1`,
		`synara_worker_lease_renewals_total{result="success"} 1`,
		`synara_worker_lease_renewals_total{result="rejected"} 1`,
		`synara_worker_fencing_rejections_total{operation="lease-renew"} 1`,
		`synara_worker_fencing_rejections_total{operation="session-event"} 1`,
		`synara_worker_fencing_rejections_total{operation="heartbeat"} 1`,
		`synara_session_event_append_duration_seconds_count{result="success"} 1`,
		`synara_session_event_append_duration_seconds_count{result="rejected"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{"oidc_id_token_invalid", "generation_fenced", "lease_expired", "worker_incarnation_fenced"} {
		if strings.Contains(metrics, forbidden) {
			t.Fatalf("metrics leaked unbounded problem code %q:\n%s", forbidden, metrics)
		}
	}
}

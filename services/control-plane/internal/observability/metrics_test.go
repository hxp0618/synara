package observability

import (
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
		&persistence.ExecutionTarget{}, &persistence.OutboxMessage{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	workerID := uuid.New()
	executionID := uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.ExecutionTarget{ID: targetID, Kind: "docker", Name: "test", Status: "active", Capabilities: map[string]any{}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerInstance{
		ID: workerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
		Namespace: "test", PodName: "worker", Version: "test", Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"), Status: "ready",
		RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
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

	registry := New(db)
	registry.ObserveHTTP("GET", "GET /v1/sessions/{sessionID}", 200, 25*time.Millisecond)
	registry.ObserveHTTP("GET", "/v1/sessions/"+executionID.String(), 404, 10*time.Millisecond)
	registry.ObserveBackground("docker", now, nil)
	payload, err := registry.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := string(payload)
	for _, expected := range []string{
		`route="/v1/sessions/{sessionID}"`, `route="unmatched"`,
		`synara_workers{status="ready",target_kind="docker"} 1`,
		`synara_executions{status="running",target_kind="docker"} 1`,
		`synara_worker_leases{state="active"} 1`, `synara_metrics_collection_success 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	if strings.Contains(metrics, executionID.String()) {
		t.Fatalf("metrics leaked a high-cardinality execution identifier:\n%s", metrics)
	}
}

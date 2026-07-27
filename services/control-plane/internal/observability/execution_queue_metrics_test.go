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

func TestExecutionQueueMetricsAreDurableBoundedAndCurrent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.AgentExecution{}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	standard := "standard"
	standardWithWhitespace := " Standard "
	interactive := "interactive"
	empty := ""
	unboundedClass := "tenant-" + uuid.NewString()
	unboundedTargetKind := "tenant-" + uuid.NewString()
	executions := []persistence.AgentExecution{
		queueMetricFixture("queued", "kubernetes", &standard, now.Add(-30*time.Second)),
		queueMetricFixture("recovering", "kubernetes", &standardWithWhitespace, now.Add(-90*time.Second)),
		queueMetricFixture("running", "kubernetes", &standard, now.Add(-5*time.Minute)),
		queueMetricFixture("queued", "docker", &interactive, now.Add(-20*time.Second)),
		queueMetricFixture("recovering", "docker", &empty, now.Add(-40*time.Second)),
		queueMetricFixture("queued", "ssh", nil, now.Add(-50*time.Second)),
		queueMetricFixture("queued", unboundedTargetKind, &unboundedClass, now.Add(-10*time.Second)),
	}
	if err := db.Create(&executions).Error; err != nil {
		t.Fatal(err)
	}

	registry := New(db)
	var first bytes.Buffer
	if err := registry.writeExecutionQueueMetrics(context.Background(), &first, now); err != nil {
		t.Fatal(err)
	}
	metrics := first.String()
	for _, expected := range []string{
		`synara_execution_queue_depth{capacity_class="interactive",queue_class="interactive",target_kind="docker"} 1`,
		`synara_execution_queue_oldest_age_seconds{capacity_class="interactive",queue_class="interactive",target_kind="docker"} 20`,
		`synara_execution_queue_depth{capacity_class="unknown",queue_class="interactive",target_kind="docker"} 1`,
		`synara_execution_queue_oldest_age_seconds{capacity_class="unknown",queue_class="interactive",target_kind="docker"} 40`,
		`synara_execution_queue_depth{capacity_class="standard",queue_class="interactive",target_kind="kubernetes"} 2`,
		`synara_execution_queue_oldest_age_seconds{capacity_class="standard",queue_class="interactive",target_kind="kubernetes"} 90`,
		`synara_execution_queue_depth{capacity_class="unknown",queue_class="interactive",target_kind="ssh"} 1`,
		`synara_execution_queue_depth{capacity_class="other",queue_class="interactive",target_kind="other"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{
		unboundedClass,
		unboundedTargetKind,
		executions[0].ID.String(),
		executions[0].TenantID.String(),
		executions[0].SessionID.String(),
		executions[0].ExecutionTargetID.String(),
	} {
		if strings.Contains(metrics, forbidden) {
			t.Fatalf("metrics leaked high-cardinality identifier %q:\n%s", forbidden, metrics)
		}
	}
	if strings.Contains(metrics, "status=") {
		t.Fatalf("queue metrics exposed a status label instead of combining queued and recovering rows:\n%s", metrics)
	}

	if err := db.Model(&persistence.AgentExecution{}).
		Where("target_kind = ?", "kubernetes").
		Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := registry.writeExecutionQueueMetrics(context.Background(), &second, now); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.String(), `target_kind="kubernetes"`) {
		t.Fatalf("stale queue series survived the authoritative refresh:\n%s", second.String())
	}
}

func TestExecutionQueueMetricsQueryFailurePublishesNoHealthySample(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	output.WriteString("existing\n")
	err = New(db).writeExecutionQueueMetrics(context.Background(), &output, time.Now().UTC())
	if err == nil {
		t.Fatal("queue metric collection succeeded without agent_executions")
	}
	if got := output.String(); got != "existing\n" {
		t.Fatalf("failed queue metric collection published output %q", got)
	}
}

func queueMetricFixture(
	status string,
	targetKind string,
	capacityClass *string,
	queuedAt time.Time,
) persistence.AgentExecution {
	return persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Status: status, ExecutionTargetID: uuid.New(), TargetKind: targetKind,
		CapacityClass: capacityClass, QueueClass: "interactive", QuotaUnits: 1,
		Generation: 1, RequestedBy: uuid.New(), QueuedAt: queuedAt,
	}
}

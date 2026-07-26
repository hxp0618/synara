package executions

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestClaimFairShareOrderAlternatesTenantBacklogsAcrossSequentialClaims(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.AgentExecution{}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	tenantA := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	tenantB := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	a1 := claimFairQueueExecution(tenantA, targetID, "10000000-0000-0000-0000-000000000001", base)
	a2 := claimFairQueueExecution(tenantA, targetID, "10000000-0000-0000-0000-000000000002", base.Add(time.Second))
	a3 := claimFairQueueExecution(tenantA, targetID, "10000000-0000-0000-0000-000000000003", base.Add(2*time.Second))
	b1 := claimFairQueueExecution(tenantB, targetID, "20000000-0000-0000-0000-000000000001", base.Add(3*time.Second))
	if err := db.Create(&[]persistence.AgentExecution{a3, b1, a2, a1}).Error; err != nil {
		t.Fatal(err)
	}

	want := []uuid.UUID{a1.ID, b1.ID, a2.ID, a3.ID}
	for index, wantID := range want {
		var selected persistence.AgentExecution
		err := db.Transaction(func(tx *gorm.DB) error {
			query := persistence.WithLocking(tx.WithContext(context.Background()), "UPDATE", "SKIP LOCKED").
				Where("agent_executions.execution_target_id = ?", targetID).
				Where("agent_executions.target_kind = ?", "kubernetes").
				Where("agent_executions.status IN ?", []string{"queued", "recovering"})
			if err := applyClaimFairShareOrder(tx, query, false).Take(&selected).Error; err != nil {
				return err
			}
			return tx.Model(&persistence.AgentExecution{}).
				Where("id = ?", selected.ID).
				Update("status", "leased").Error
		})
		if err != nil {
			t.Fatal(err)
		}
		if selected.ID != wantID {
			t.Fatalf("claim %d selected %s, want %s", index+1, selected.ID, wantID)
		}
	}
}

func TestClaimFairShareOrderCountsOnlyActiveServiceStatuses(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.AgentExecution{}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	tenantA := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	tenantB := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	aQueued := claimFairQueueExecution(tenantA, targetID, "10000000-0000-0000-0000-000000000001", base)
	bQueued := claimFairQueueExecution(tenantB, targetID, "20000000-0000-0000-0000-000000000001", base.Add(time.Minute))
	rows := []persistence.AgentExecution{aQueued, bQueued}
	for index, status := range []string{"leased", "running", "waiting-for-approval", "suspended", "completed"} {
		row := claimFairQueueExecution(
			tenantA,
			targetID,
			uuid.NewString(),
			base.Add(time.Duration(index+2)*time.Minute),
		)
		row.Status = status
		rows = append(rows, row)
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var selected persistence.AgentExecution
	query := db.Where("agent_executions.execution_target_id = ?", targetID).
		Where("agent_executions.status IN ?", []string{"queued", "recovering"})
	if err := applyClaimFairShareOrder(db, query, false).Take(&selected).Error; err != nil {
		t.Fatal(err)
	}
	if selected.ID != bQueued.ID {
		t.Fatalf("active-service count selected %s, want Tenant B %s", selected.ID, bQueued.ID)
	}
}

func claimFairQueueExecution(
	tenantID uuid.UUID,
	targetID uuid.UUID,
	id string,
	queuedAt time.Time,
) persistence.AgentExecution {
	return persistence.AgentExecution{
		ID: uuid.MustParse(id), TenantID: tenantID, SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "kubernetes",
		Generation: 0, RequestedBy: uuid.New(), QueuedAt: queuedAt,
	}
}

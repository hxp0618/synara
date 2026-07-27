package routing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestAddMemberPostgresSerializesWithTargetDisable(t *testing.T) {
	db := openRoutingCommitPostgresDB(t)
	service, request, _ := seedRoutingCommitPostgresFixture(t, db)
	targetID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &request.TenantID, OrganizationID: &request.OrganizationID,
		Kind: "kubernetes", Name: "member-target-disable-" + targetID.String(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	group, err := service.CreateGroup(context.Background(), CreateGroupInput{
		TenantID: request.TenantID, OrganizationID: &request.OrganizationID,
		Name: "member-target-disable-" + uuid.NewString(), Strategy: StrategyPriority,
	})
	if err != nil {
		t.Fatal(err)
	}

	disableTx := db.Begin()
	if disableTx.Error != nil {
		t.Fatal(disableTx.Error)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = disableTx.Rollback().Error
		}
	})
	var locked persistence.ExecutionTarget
	if err := persistence.WithLocking(disableTx, "UPDATE", "").
		Where("id = ?", targetID).
		Take(&locked).Error; err != nil {
		t.Fatal(err)
	}
	if err := disableTx.Model(&persistence.ExecutionTarget{}).
		Where("id = ? AND status = ?", targetID, "active").
		Updates(map[string]any{"status": "disabled", "updated_at": now.Add(time.Second)}).Error; err != nil {
		t.Fatal(err)
	}

	addDone := make(chan error, 1)
	addStarted := make(chan struct{})
	go func() {
		close(addStarted)
		_, addErr := service.AddMember(context.Background(), AddMemberInput{
			TenantID: request.TenantID, TargetGroupID: group.ID, ExecutionTargetID: targetID,
			Region: "local", ClusterID: "orbstack", Priority: 100, Weight: 100,
		})
		addDone <- addErr
	}()
	<-addStarted
	select {
	case addErr := <-addDone:
		_ = disableTx.Rollback().Error
		t.Fatalf("Target Group member insert crossed the Target disable lock: %v", addErr)
	case <-time.After(150 * time.Millisecond):
	}

	if err := disableTx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	committed = true
	select {
	case addErr := <-addDone:
		if codeOf(addErr) != "target_group_member_target_not_found" {
			t.Fatalf("post-disable AddMember err = %v", addErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Target Group member insert did not resume after Target disable committed")
	}
	var memberCount int64
	if err := db.Model(&persistence.ExecutionTargetGroupMember{}).
		Where("target_group_id = ? AND execution_target_id = ?", group.ID, targetID).
		Count(&memberCount).Error; err != nil {
		t.Fatal(err)
	}
	if memberCount != 0 {
		t.Fatalf("disabled Target gained %d active member rows", memberCount)
	}
}

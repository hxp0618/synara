package schedulingdecision_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresPersistsCompleteSchedulingCandidateTrace(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "complete-scheduling-trace-integration")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID, sessionID, turnID := uuid.New(), uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Complete scheduling trace", DefaultBranch: "main", Visibility: "private",
			CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Complete scheduling trace",
			Status: "active", Visibility: "private", Provider: "codex",
			ExecutionTargetID: domain.ExecutionTargetID, ProviderResumeCursorState: "absent",
			ResourceState: "provisioning", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900,
			SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled",
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
			Status: "queued", InputText: "freeze complete trace", TurnKind: "message",
			RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
		},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	tenantID, organizationID := domain.TenantID, domain.OrganizationID
	rejectedTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantID, OrganizationID: &organizationID,
		Kind: "local", Name: "rejected-trace-target", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&rejectedTarget).Error; err != nil {
		t.Fatal(err)
	}
	group := persistence.ExecutionTargetGroup{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: &organizationID,
		Name: "complete-trace-" + uuid.NewString(), Strategy: "balanced",
		PreferredRegions: []string{},
		AllowCrossRegion: true, MaxFailoverAttempts: 2, HealthMaxStalenessSeconds: 90,
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	selectedMember := persistence.ExecutionTargetGroupMember{
		ID: uuid.New(), TenantID: tenantID, TargetGroupID: group.ID,
		ExecutionTargetID: domain.ExecutionTargetID, Region: "local", ClusterID: "orbstack",
		Priority: 100, Weight: 100, Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	rejectedMember := persistence.ExecutionTargetGroupMember{
		ID: uuid.New(), TenantID: tenantID, TargetGroupID: group.ID,
		ExecutionTargetID: rejectedTarget.ID, Region: "local", ClusterID: "orbstack-rejected",
		Priority: 100, Weight: 100, Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&[]persistence.ExecutionTargetGroupMember{selectedMember, rejectedMember}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	routingReason := "balanced"
	selectedRegion, selectedClusterID := selectedMember.Region, selectedMember.ClusterID
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		Provider: &provider, Generation: 0, RequestedBy: domain.UserID, QueuedAt: now,
		TargetGroupID: &group.ID, TargetGroupVersion: &group.Version,
		TargetGroupMemberVersion: &selectedMember.Version,
		SelectedRegion:           &selectedRegion, SelectedClusterID: &selectedClusterID,
		RoutingReason: &routingReason, PlacementRegion: selectedRegion, PlacementClusterID: selectedClusterID,
		TenantSchedulingPolicyDigest:       schedulingpolicy.UnrestrictedDigest,
		OrganizationSchedulingPolicyDigest: schedulingpolicy.UnrestrictedDigest,
	}
	healthVersion := int64(1)
	healthStatus, capacityStatus := "healthy", "available"
	observedAt, expiresAt := now.Add(-time.Second), now.Add(time.Minute)
	allocated := 0
	queued, effectiveLoadRank := int64(0), int64(0)
	priority, weight := 100, 100
	routeRank, regionRank, capacityRank := 0, 0, 0
	selected := schedulingdecision.CandidateFromExecution(execution)
	selected.TargetGroupMemberID = &selectedMember.ID
	selected.HealthVersion = &healthVersion
	selected.HealthStatus = &healthStatus
	selected.CapacityStatus = &capacityStatus
	selected.HealthObservedAt = &observedAt
	selected.HealthExpiresAt = &expiresAt
	selected.AllocatedCapacityUnits = &allocated
	selected.QueuedExecutionUnits = &queued
	selected.EffectiveLoadRank = &effectiveLoadRank
	selected.Priority = &priority
	selected.Weight = &weight
	selected.PriorityRank = &routeRank
	selected.RegionRank = &regionRank
	selected.CapacityRank = &capacityRank
	rejectionCode := "health-missing"
	rejected := schedulingdecision.CandidateSnapshot{
		ExecutionTargetID: rejectedTarget.ID, TargetKind: rejectedTarget.Kind,
		TargetGroupID: &group.ID, TargetGroupVersion: &group.Version,
		TargetGroupMemberID: &rejectedMember.ID, TargetGroupMemberVersion: &rejectedMember.Version,
		Region: rejectedMember.Region, ClusterID: rejectedMember.ClusterID,
		Priority: &priority, Weight: &weight, Eligibility: schedulingdecision.EligibilityRejected,
		RejectionCode: &rejectionCode,
	}
	input := schedulingdecision.NewCompleteInput(
		uuid.New(), schedulingdecision.AlgorithmQueuePressureV1, now,
		[]schedulingdecision.CandidateSnapshot{rejected, selected},
	)
	decision, err := schedulingdecision.CreateExecution(ctx, db, &execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.EvidenceCompleteness != schedulingdecision.EvidenceComplete ||
		decision.CandidateCount != 2 || decision.SelectedOrdinal != 1 {
		t.Fatalf("PostgreSQL complete Decision = %#v", decision)
	}
	var candidates []persistence.ExecutionSchedulingCandidate
	if err := db.Where("tenant_id = ? AND decision_id = ?", tenantID, decision.ID).
		Order("ordinal ASC").Find(&candidates).Error; err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].RejectionCode == nil ||
		*candidates[0].RejectionCode != rejectionCode || !candidates[1].Selected ||
		candidates[1].CandidateSHA256 == "" || decision.CandidateSetSHA256 == "" {
		t.Fatalf("PostgreSQL complete Candidates = %#v", candidates)
	}
	if err := db.Model(&persistence.ExecutionSchedulingCandidate{}).
		Where("tenant_id = ? AND decision_id = ? AND ordinal = 0", tenantID, decision.ID).
		Update("rejection_code", "health-expired").Error; err == nil {
		t.Fatal("PostgreSQL allowed complete rejected-candidate evidence to mutate")
	}
}

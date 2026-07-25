package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresGlobalTargetRoutingAndCrossAttemptRecoveryConstraints(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	var sourceTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", domain.ExecutionTargetID).Take(&sourceTarget).Error; err != nil {
		t.Fatal(err)
	}
	destinationTarget := sourceTarget
	destinationTarget.ID = uuid.New()
	destinationTarget.TenantID = &domain.TenantID
	destinationTarget.OrganizationID = &domain.OrganizationID
	destinationTarget.Name = "routing-destination-" + uuid.NewString()
	if err := db.Create(&destinationTarget).Error; err != nil {
		t.Fatal(err)
	}
	router := routing.NewService(db)
	limit := 3
	group, err := router.CreateGroup(ctx, routing.CreateGroupInput{
		TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID,
		Name: "routing-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: &limit, HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceMember, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: domain.TenantID, TargetGroupID: group.ID, ExecutionTargetID: sourceTarget.ID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: domain.TenantID, TargetGroupID: group.ID, ExecutionTargetID: destinationTarget.ID,
		Region: "cn-beijing", ClusterID: "cluster-b", Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	capacity := 10
	firstHealth, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: destinationTarget.ID, Status: routing.HealthHealthy,
		CapacityStatus: routing.CapacityAvailable, AvailableCapacityUnits: &capacity,
		Source: "postgres-migration-test", ObservedAt: now, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstHealth.Version != 1 {
		t.Fatalf("health version = %d", firstHealth.Version)
	}
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: destinationTarget.ID, Status: routing.HealthHealthy,
		CapacityStatus: routing.CapacityAvailable, AvailableCapacityUnits: &capacity,
		Source: "postgres-migration-test", ObservedAt: now, TTL: time.Minute,
	}); err == nil {
		t.Fatal("PostgreSQL accepted a stale Target health observation")
	}
	sourceDRDomain := routing.DRDomainForLocation("cn-shanghai", "cluster-a")
	replicatedThroughAt := now.Add(-time.Second)
	readiness, err := router.ObserveDRReadiness(ctx, routing.DRReadinessObservation{
		ExecutionTargetID:   destinationTarget.ID,
		SourceDRDomain:      sourceDRDomain,
		DRDomain:            routing.DRDomainForLocation("cn-beijing", "cluster-b"),
		ReplicatedThroughAt: replicatedThroughAt,
		CheckpointsReady:    true,
		PublisherIdentity:   "postgres-migration-test",
		ObservedAt:          now,
		TTL:                 time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Version != 1 {
		t.Fatalf("dr readiness version = %d", readiness.Version)
	}
	duplicateReadiness := persistence.ExecutionTargetDRReadiness{
		ExecutionTargetID:   destinationTarget.ID,
		SourceDRDomain:      sourceDRDomain,
		DRDomain:            routing.DRDomainForLocation("cn-beijing", "cluster-b"),
		ReplicatedThroughAt: replicatedThroughAt,
		CheckpointsReady:    true,
		PublisherIdentity:   "postgres-migration-test",
		ObservedAt:          now.Add(time.Second),
		ExpiresAt:           now.Add(2 * time.Minute),
		Version:             1,
		UpdatedAt:           now.Add(time.Second),
	}
	if err := db.Create(&duplicateReadiness).Error; err == nil {
		t.Fatal("PostgreSQL accepted a duplicate Target DR readiness authority for the same source domain")
	}
	if err := db.Exec(
		`UPDATE execution_target_dr_readiness
		   SET version = ?, observed_at = ?
		 WHERE execution_target_id = ? AND source_dr_domain = ?`,
		readiness.Version+1, readiness.ObservedAt, destinationTarget.ID, sourceDRDomain,
	).Error; err == nil {
		t.Fatal("PostgreSQL accepted a non-monotonic Target DR readiness update")
	}
	if err := db.Exec(
		`UPDATE execution_target_dr_readiness
		   SET version = ?, observed_at = ?, replicated_through_at = ?
		 WHERE execution_target_id = ? AND source_dr_domain = ?`,
		readiness.Version+1, readiness.ObservedAt.Add(time.Second), readiness.ObservedAt.Add(2*time.Second),
		destinationTarget.ID, sourceDRDomain,
	).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Target DR readiness watermark later than observed_at")
	}

	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	if err := db.Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "routing migration", DefaultBranch: "main", Visibility: "organization",
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "routing migration",
		Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: sourceTarget.ID, RequestedExecutionTargetID: sourceTarget.ID,
		ExecutionTargetGroupID: &group.ID, RoutingPolicyVersion: &group.Version,
		ResourceState: "active", MeaningfulActivityAt: now, WarmPoolMode: "disabled",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
		Status: "queued", InputText: "recover across targets", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(),
		ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind, WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: "migration", Namespace: "default",
		PodName: "routing-migration-" + uuid.NewString(), Version: "test", ProtocolVersion: 1,
		Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: []byte("routing-migration-" + uuid.NewString()), Status: "online", AdministrativeStatus: "active",
		RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	destinationWorker := worker
	destinationWorker.ID = uuid.New()
	destinationWorker.InstanceUID = uuid.NewString()
	destinationWorker.ExecutionTargetID = destinationTarget.ID
	destinationWorker.PodName = "routing-destination-" + uuid.NewString()
	destinationWorker.AuthTokenHash = []byte("routing-destination-" + uuid.NewString())
	if err := db.Create(&destinationWorker).Error; err != nil {
		t.Fatal(err)
	}
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "leased", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, WorkerID: &worker.ID, ProviderResumeStrategySnapshot: "authoritative-history",
		Generation: 1, RequestedBy: domain.UserID, QueuedAt: now, WarmPoolModeSnapshot: "disabled",
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: sourceTarget, Group: group, Member: sourceMember, RoutingReason: routing.StrategyPriority,
	})
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	sourceBundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: source.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
		Payload: map[string]any{}, PayloadSHA256: strings.Repeat("a", 64), CreatedAt: now,
	}
	if err := db.Create(&sourceBundle).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", domain.TenantID, source.ID).
		Updates(map[string]any{"status": "interrupted", "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).Where("tenant_id = ? AND id = ?", domain.TenantID, sessionID).
		Update("execution_target_id", destinationTarget.ID).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "disaster-recovery"
	destination := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 2, Status: "recovering", ExecutionTargetID: destinationTarget.ID, TargetKind: destinationTarget.Kind,
		Provider: &provider, WorkerID: &destinationWorker.ID, ProviderResumeStrategySnapshot: "authoritative-history",
		PredecessorExecutionID: &source.ID, NextRecoveryReason: &recoveryReason,
		Generation: 1, RequestedBy: domain.UserID, QueuedAt: now, WarmPoolModeSnapshot: "disabled",
	}
	selection, err := router.Select(ctx, db, routing.SelectRequest{
		TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, TargetGroupID: group.ID,
		ExcludedTargetIDs: []uuid.UUID{sourceTarget.ID},
		SourceRegion:      "cn-shanghai", SourceClusterID: "cluster-a", SourceDRDomain: sourceDRDomain,
		ReplicatedThroughAt: replicatedThroughAt, RequiredDRStores: routing.DRStoreRequirements{Checkpoints: true},
		DisasterRecovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	routing.ApplyExecutionSelection(&destination, selection)
	if err := db.Create(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", domain.TenantID, destination.ID).
		Updates(map[string]any{"status": "leased", "next_recovery_reason": nil}).Error; err != nil {
		t.Fatal(err)
	}
	destinationBundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		ExecutionID: destination.ID, Generation: 1, SchemaVersion: 1,
		RecoveryReason: "disaster-recovery", PreviousBundleID: &sourceBundle.ID,
		Payload: map[string]any{}, PayloadSHA256: strings.Repeat("b", 64), CreatedAt: now,
	}
	if err := db.Create(&destinationBundle).Error; err != nil {
		t.Fatalf("insert cross-attempt Recovery Bundle: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", domain.TenantID, destination.ID).
		Update("selected_region", "us-west").Error; err == nil {
		t.Fatal("PostgreSQL accepted mutation of an immutable global routing snapshot")
	}
}

func TestPostgresLocationOutageAuthorityMonotonicityAndDeleteProtection(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	outage := persistence.ExecutionLocationOutage{
		TenantID:          domain.TenantID,
		Region:            "cn-shanghai",
		ClusterID:         "",
		Status:            "draining",
		PublisherIdentity: "postgres-location-outage-test",
		ObservedAt:        now,
		ExpiresAt:         now.Add(time.Minute),
		Version:           1,
		UpdatedAt:         now,
	}
	if err := db.Create(&outage).Error; err != nil {
		t.Fatalf("insert region outage authority: %v", err)
	}

	clusterOutage := outage
	clusterOutage.ClusterID = "cluster-a"
	clusterOutage.Status = "unreachable"
	clusterOutage.ObservedAt = now.Add(time.Second)
	clusterOutage.ExpiresAt = now.Add(2 * time.Minute)
	clusterOutage.UpdatedAt = clusterOutage.ObservedAt
	if err := db.Create(&clusterOutage).Error; err != nil {
		t.Fatalf("insert cluster outage authority: %v", err)
	}

	if err := db.Exec(
		`UPDATE execution_location_outages
		   SET version = ?, observed_at = ?
		 WHERE tenant_id = ? AND region = ? AND cluster_id = ?`,
		2, now, outage.TenantID, outage.Region, outage.ClusterID,
	).Error; err == nil {
		t.Fatal("PostgreSQL accepted a non-monotonic location outage update")
	}

	if err := db.Delete(&persistence.ExecutionLocationOutage{}, "tenant_id = ? AND region = ? AND cluster_id = ?", outage.TenantID, outage.Region, outage.ClusterID).Error; err == nil {
		t.Fatal("PostgreSQL allowed a location outage authority row to be deleted")
	}
}

package routing

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestLockSelectionForCommitPostgresLinearizesReadinessRevocation(t *testing.T) {
	db := openRoutingCommitPostgresDB(t)
	service, request, selection := seedRoutingCommitPostgresFixture(t, db)

	commitTx := db.Begin()
	if commitTx.Error != nil {
		t.Fatal(commitTx.Error)
	}
	locked, err := service.LockSelectionForCommit(context.Background(), commitTx, request, selection)
	if err != nil {
		_ = commitTx.Rollback().Error
		t.Fatal(err)
	}
	if locked.DRReadiness == nil || locked.DRReadiness.Version != selection.DRReadiness.Version {
		_ = commitTx.Rollback().Error
		t.Fatalf("locked selection = %#v", locked)
	}

	revocationDone := make(chan error, 1)
	revocationStarted := make(chan struct{})
	go func() {
		close(revocationStarted)
		observedAt := time.Now().UTC()
		_, observeErr := NewService(db).ObserveDRReadiness(context.Background(), DRReadinessObservation{
			ExecutionTargetID:   selection.Target.ID,
			SourceDRDomain:      selection.DRReadiness.SourceDRDomain,
			DRDomain:            selection.DRReadiness.DRDomain,
			ReplicatedThroughAt: selection.DRReadiness.ReplicatedThroughAt,
			CheckpointsReady:    false,
			PublisherIdentity:   "postgres-revoker",
			ObservedAt:          observedAt,
			TTL:                 time.Minute,
		})
		revocationDone <- observeErr
	}()
	<-revocationStarted

	select {
	case observeErr := <-revocationDone:
		_ = commitTx.Rollback().Error
		t.Fatalf("readiness revocation crossed commit authority lock: %v", observeErr)
	case <-time.After(150 * time.Millisecond):
	}
	if err := commitTx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case observeErr := <-revocationDone:
		if observeErr != nil {
			t.Fatal(observeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readiness revocation did not resume after commit authority lock released")
	}

	staleTx := db.Begin()
	if staleTx.Error != nil {
		t.Fatal(staleTx.Error)
	}
	_, err = service.LockSelectionForCommit(context.Background(), staleTx, request, selection)
	_ = staleTx.Rollback().Error
	if codeOf(err) != "target_routing_selection_stale" {
		t.Fatalf("revocation-first validation err = %v", err)
	}
}

func TestLockSelectionForCommitPostgresRejectsQueuePressureCommittedWhileWaiting(t *testing.T) {
	db := openRoutingCommitPostgresDB(t)
	service, request, selection := seedRoutingCommitPostgresFixture(t, db)
	queuedExecution := seedRoutingPressureExecutionParents(t, db, request, selection)

	firstTx := db.Begin()
	if firstTx.Error != nil {
		t.Fatal(firstTx.Error)
	}
	if _, err := service.LockSelectionForCommit(context.Background(), firstTx, request, selection); err != nil {
		_ = firstTx.Rollback().Error
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	secondStarted := make(chan struct{})
	go func() {
		secondTx := db.Begin()
		if secondTx.Error != nil {
			secondDone <- secondTx.Error
			return
		}
		close(secondStarted)
		_, lockErr := service.LockSelectionForCommit(context.Background(), secondTx, request, selection)
		_ = secondTx.Rollback().Error
		secondDone <- lockErr
	}()
	<-secondStarted

	select {
	case secondErr := <-secondDone:
		_ = firstTx.Rollback().Error
		t.Fatalf("second routing commit crossed the first authority lock: %v", secondErr)
	case <-time.After(150 * time.Millisecond):
	}
	if err := firstTx.Create(&queuedExecution).Error; err != nil {
		_ = firstTx.Rollback().Error
		t.Fatal(err)
	}
	if err := firstTx.Commit().Error; err != nil {
		t.Fatal(err)
	}

	select {
	case secondErr := <-secondDone:
		if codeOf(secondErr) != "target_routing_selection_stale" {
			t.Fatalf("queue-pressure second commit err = %v", secondErr)
		}
		var routingError *problem.Error
		if !errors.As(secondErr, &routingError) || routingError.Details["reason"] != "queue-pressure-changed" ||
			routingError.Details["expectedQueuedExecutionUnits"] != int64(0) ||
			routingError.Details["actualQueuedExecutionUnits"] != int64(1) {
			t.Fatalf("queue-pressure second commit details = %#v", routingError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second routing commit did not resume after the first transaction committed")
	}
}

func openRoutingCommitPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedRoutingCommitPostgresFixture(
	t *testing.T,
	db *gorm.DB,
) (*Service, SelectRequest, Selection) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Second)
	userID := uuid.New()
	tenantID := uuid.New()
	organizationID := uuid.New()
	targetID := uuid.New()
	slugSuffix := uuid.NewString()[:8]
	models := []any{
		&persistence.User{
			ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Routing commit PostgreSQL",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Tenant{
			ID: tenantID, Slug: "routing-commit-" + slugSuffix, Name: "Routing commit PostgreSQL",
			Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID,
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.Organization{
			ID: organizationID, TenantID: tenantID, Slug: "root-" + slugSuffix,
			Name: "Routing commit PostgreSQL", Kind: "root", Status: "active", Settings: map[string]any{},
			CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID,
			Kind: "kubernetes", Name: "routing-commit-" + slugSuffix, Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	service := NewService(db)
	group, err := service.CreateGroup(context.Background(), CreateGroupInput{
		TenantID: tenantID, OrganizationID: &organizationID, Name: "routing-commit-" + slugSuffix,
		Strategy: StrategyPriority, AllowCrossRegion: true, HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AddMember(context.Background(), AddMemberInput{
		TenantID: tenantID, TargetGroupID: group.ID, ExecutionTargetID: targetID,
		Region: "cn-beijing", ClusterID: "cluster-b", Priority: 10, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: targetID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(10), Source: "postgres-routing-commit", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	sourceDRDomain := DRDomainForLocation("cn-shanghai", "cluster-a")
	watermark := now.Add(-time.Second)
	if _, err := service.ObserveDRReadiness(context.Background(), DRReadinessObservation{
		ExecutionTargetID: targetID, SourceDRDomain: sourceDRDomain,
		DRDomain: DRDomainForLocation("cn-beijing", "cluster-b"), ReplicatedThroughAt: watermark,
		CheckpointsReady: true, PublisherIdentity: "postgres-routing-commit", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	request := SelectRequest{
		TenantID: tenantID, OrganizationID: organizationID, TargetGroupID: group.ID,
		SourceRegion: "cn-shanghai", SourceClusterID: "cluster-a", SourceDRDomain: sourceDRDomain,
		ReplicatedThroughAt: watermark, RequiredDRStores: DRStoreRequirements{Checkpoints: true}, DisasterRecovery: true,
	}
	selection, err := service.Select(context.Background(), db, request)
	if err != nil {
		t.Fatal(err)
	}
	return service, request, selection
}

func seedRoutingPressureExecutionParents(
	t *testing.T,
	db *gorm.DB,
	request SelectRequest,
	selection Selection,
) persistence.AgentExecution {
	t.Helper()
	var tenant persistence.Tenant
	if err := db.Where("id = ?", request.TenantID).Take(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	project := persistence.Project{
		ID: uuid.New(), TenantID: request.TenantID, OrganizationID: request.OrganizationID,
		Name: "Routing queue pressure", DefaultBranch: "main", Visibility: "organization",
		CreatedBy: tenant.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: request.TenantID, OrganizationID: request.OrganizationID,
		ProjectID: project.ID, CreatedBy: tenant.CreatedBy, Title: "Routing queue pressure",
		Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: selection.Target.ID, RequestedExecutionTargetID: selection.Target.ID,
		ResourceState: "idle", CreatedAt: now, UpdatedAt: now,
	}
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: request.TenantID, SessionID: session.ID, CreatedBy: tenant.CreatedBy,
		Status: "queued", InputText: "Queue pressure concurrency", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range []any{&project, &session, &turn} {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	return persistence.AgentExecution{
		ID: uuid.New(), TenantID: request.TenantID, SessionID: session.ID, TurnID: turn.ID,
		Attempt: 1, Status: "queued", ExecutionTargetID: selection.Target.ID, TargetKind: selection.Target.Kind,
		Provider: &provider, Generation: 0, RequestedBy: tenant.CreatedBy, QueuedAt: now,
	}
}

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

func TestCapacityAdmissionPostgresSerializesConcurrentReservations(t *testing.T) {
	db := openRoutingCommitPostgresDB(t)
	service, request, selection := seedRoutingCommitPostgresFixture(t, db)
	firstExecution := seedRoutingPressureExecutionParents(t, db, request, selection)
	now := time.Now().UTC()
	if _, err := service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: selection.Target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(1), AllocatedCapacityUnits: 0,
		ReservationAuthority: &ReservationAuthorityObservation{
			Mode:             ReservationAuthorityExactActiveV1,
			Acknowledgements: []ReservationIdentity{},
		},
		Source: "postgres-capacity-reservation", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	firstTx := db.Begin()
	if firstTx.Error != nil {
		t.Fatal(firstTx.Error)
	}
	firstAdmission, err := AdmitExecutionCapacity(
		context.Background(), firstTx, request.TenantID, firstExecution.ID,
		selection.Target.ID, false, now,
	)
	if err != nil {
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
		_, admitErr := AdmitExecutionCapacity(
			context.Background(), secondTx, request.TenantID, uuid.New(),
			selection.Target.ID, false, time.Now().UTC(),
		)
		_ = secondTx.Rollback().Error
		secondDone <- admitErr
	}()
	<-secondStarted

	select {
	case secondErr := <-secondDone:
		_ = firstTx.Rollback().Error
		t.Fatalf("concurrent capacity admission crossed the Target lock: %v", secondErr)
	case <-time.After(150 * time.Millisecond):
	}
	if err := firstTx.Create(&firstExecution).Error; err != nil {
		_ = firstTx.Rollback().Error
		t.Fatal(err)
	}
	if err := CreateCapacityAdmission(context.Background(), firstTx, firstAdmission); err != nil {
		_ = firstTx.Rollback().Error
		t.Fatal(err)
	}
	if err := firstTx.Commit().Error; err != nil {
		t.Fatal(err)
	}

	select {
	case secondErr := <-secondDone:
		if codeOf(secondErr) != "execution_target_capacity_reserved" {
			t.Fatalf("second capacity admission err = %v", secondErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second capacity admission did not resume after the first transaction committed")
	}
	var evidence persistence.ExecutionCapacityAdmission
	if err := db.Where("tenant_id = ? AND execution_id = ?", request.TenantID, firstExecution.ID).
		Take(&evidence).Error; err != nil {
		t.Fatal(err)
	}
	if evidence.AdmissionMode != CapacityAdmissionExactActiveV1 ||
		evidence.SnapshotSHA256 != CapacityAdmissionSHA256(evidence) {
		t.Fatalf("capacity admission evidence = %#v", evidence)
	}
	if err := db.Model(&evidence).Update("active_reservation_units", 9).Error; err == nil {
		t.Fatal("PostgreSQL allowed immutable capacity admission evidence to be updated")
	}
	var health persistence.ExecutionTargetHealth
	if err := db.Where("execution_target_id = ?", selection.Target.ID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionTargetReservationAcknowledgement{
		ExecutionTargetID: selection.Target.ID, ExecutionID: firstExecution.ID,
		ExecutionGeneration: firstExecution.Generation, HealthVersion: health.Version,
		AcknowledgedAt: time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed a reservation acknowledgement to be inserted into the current Health version")
	}
}

func TestMemberDisablePostgresSerializesWithExecutionCommitAndFailsClosed(t *testing.T) {
	db := openRoutingCommitPostgresDB(t)
	service, request, selection := seedRoutingCommitPostgresFixture(t, db)
	execution := seedRoutingPressureExecutionParents(t, db, request, selection)
	ApplyExecutionSelection(&execution, selection)

	launchTx := db.Begin()
	if launchTx.Error != nil {
		t.Fatal(launchTx.Error)
	}
	if _, err := service.LockSelectionForCommit(context.Background(), launchTx, request, selection); err != nil {
		_ = launchTx.Rollback().Error
		t.Fatal(err)
	}

	disableDone := make(chan error, 1)
	disableStarted := make(chan struct{})
	go func() {
		close(disableStarted)
		_, _, updateErr := NewService(db).UpdateMemberStatus(context.Background(), UpdateMemberStatusInput{
			TenantID: request.TenantID, TargetGroupID: request.TargetGroupID, MemberID: selection.Member.ID,
			ExpectedVersion: selection.Member.Version, Status: MemberStatusDisabled,
			ActorID: execution.RequestedBy, RequestID: "postgres-member-disable",
		})
		disableDone <- updateErr
	}()
	<-disableStarted

	select {
	case updateErr := <-disableDone:
		_ = launchTx.Rollback().Error
		t.Fatalf("member disable crossed launch authority lock: %v", updateErr)
	case <-time.After(150 * time.Millisecond):
	}
	if err := launchTx.Create(&execution).Error; err != nil {
		_ = launchTx.Rollback().Error
		t.Fatal(err)
	}
	if err := launchTx.Commit().Error; err != nil {
		t.Fatal(err)
	}

	select {
	case updateErr := <-disableDone:
		if codeOf(updateErr) != "target_group_member_execution_active" {
			t.Fatalf("member disable after committed launch err = %v", updateErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("member disable did not resume after launch transaction committed")
	}
	var member persistence.ExecutionTargetGroupMember
	if err := db.Where("tenant_id = ? AND id = ?", request.TenantID, selection.Member.ID).Take(&member).Error; err != nil {
		t.Fatal(err)
	}
	if member.Status != MemberStatusActive || member.Version != selection.Member.Version {
		t.Fatalf("failed disable changed member = %#v", member)
	}
	drained, replayed, err := service.UpdateMemberStatus(context.Background(), UpdateMemberStatusInput{
		TenantID: request.TenantID, TargetGroupID: request.TargetGroupID, MemberID: selection.Member.ID,
		ExpectedVersion: selection.Member.Version, Status: MemberStatusDraining,
		ActorID: execution.RequestedBy, RequestID: "postgres-member-drain",
	})
	if err != nil || replayed || drained.Status != MemberStatusDraining || drained.Version != selection.Member.Version+1 {
		t.Fatalf("committed member drain = %#v replayed=%t err=%v", drained, replayed, err)
	}
	staleTx := db.Begin()
	if staleTx.Error != nil {
		t.Fatal(staleTx.Error)
	}
	_, err = service.LockSelectionForCommit(context.Background(), staleTx, request, selection)
	_ = staleTx.Rollback().Error
	if codeOf(err) != "target_routing_selection_stale" {
		t.Fatalf("pre-Drain selection committed after member transition: %v", err)
	}
	replayedMember, replayed, err := service.UpdateMemberStatus(context.Background(), UpdateMemberStatusInput{
		TenantID: request.TenantID, TargetGroupID: request.TargetGroupID, MemberID: selection.Member.ID,
		ExpectedVersion: selection.Member.Version, Status: MemberStatusDraining,
		ActorID: execution.RequestedBy, RequestID: "postgres-member-drain-replay",
	})
	if err != nil || !replayed || replayedMember.Version != drained.Version {
		t.Fatalf("PostgreSQL drain replay = %#v replayed=%t err=%v", replayedMember, replayed, err)
	}
	var drainAuditCount int64
	if err := db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action = ?", request.TenantID, selection.Member.ID, "execution_target_group_member.drain_started").
		Count(&drainAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if drainAuditCount != 1 {
		t.Fatalf("PostgreSQL drain audit count = %d, want 1", drainAuditCount)
	}
	if err := db.Model(&persistence.ExecutionTargetGroupMember{}).
		Where("tenant_id = ? AND id = ?", request.TenantID, selection.Member.ID).
		Updates(map[string]any{"status": MemberStatusDisabled, "version": drained.Version}).Error; err == nil {
		t.Fatal("PostgreSQL allowed member status mutation without a next version")
	}
	if err := db.Where("tenant_id = ? AND id = ?", request.TenantID, selection.Member.ID).
		Delete(&persistence.ExecutionTargetGroupMember{}).Error; err == nil {
		t.Fatal("PostgreSQL allowed Execution Target Group member deletion")
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

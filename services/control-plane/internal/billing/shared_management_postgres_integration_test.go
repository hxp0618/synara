package billing

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/leadership"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/reconcilerleadership"
)

type postgresSharedSchedulerOutcome struct {
	summary SharedAllocationSchedulerRunSummary
	err     error
}

func TestPostgresConcurrentSharedLedgerCoverageSealSerializesExactReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := openBillingPostgresIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)

	now := time.Now().UTC().Truncate(time.Second)
	operatorUserID, operatorTenantID := seedPostgresSharedBillingOperator(t, ctx, db, now)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "shared-coverage-pg-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := db.WithContext(ctx).Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db, nil, WithPlatformBillingOperatorTenant(operatorTenantID))
	service.now = func() time.Time { return now }
	principal := identity.Principal{
		UserID: operatorUserID, ActiveTenantID: &operatorTenantID,
	}
	input := SealSharedTargetLedgerCoverageInput{
		CompleteFromAt: now.Add(-30 * time.Minute), MinimumWriterVersion: "stage4-pg-writer-v1",
		DeploymentAttestationSHA256: strings.Repeat("a", 64),
	}
	type outcome struct {
		coverage SharedTargetLedgerCoverage
		created  bool
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for index := range 2 {
		go func() {
			<-start
			coverage, created, sealErr := service.SealSharedTargetLedgerCoverageAuthorized(
				ctx, principal, operatorTenantID, target.ID, input,
				"shared-coverage-pg-"+string(rune('a'+index)), "127.0.0.1",
			)
			outcomes <- outcome{coverage: coverage, created: created, err: sealErr}
		}()
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent shared coverage errors: first=%v second=%v", first.err, second.err)
	}
	if first.coverage.ID == uuid.Nil || first.coverage.ID != second.coverage.ID || first.created == second.created {
		t.Fatalf("unexpected concurrent shared coverage outcomes: first=%#v second=%#v", first, second)
	}
	var coverageCount, auditCount int64
	if err := db.WithContext(ctx).Model(&persistence.BillingSharedTargetLedgerCoverage{}).
		Where("execution_target_id = ?", target.ID).Count(&coverageCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", operatorTenantID,
			"billing.shared_target_ledger_coverage_sealed", first.coverage.ID).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if coverageCount != 1 || auditCount != 1 {
		t.Fatalf("PostgreSQL shared coverage counts = coverage:%d audit:%d", coverageCount, auditCount)
	}

	conflict := input
	conflict.MinimumWriterVersion = "stage4-pg-writer-v2"
	_, _, err = service.SealSharedTargetLedgerCoverageAuthorized(
		ctx, principal, operatorTenantID, target.ID, conflict, "shared-coverage-pg-conflict", "127.0.0.1",
	)
	assertProblemCode(t, err, "billing_shared_ledger_coverage_conflict")
}

func TestPostgresSharedAllocationSchedulerLeadershipHandoffReplaysExplicitPeriod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := openBillingPostgresIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(12)

	now := time.Now().UTC().Truncate(time.Second)
	operatorUserID, operatorTenantID := seedPostgresSharedBillingOperator(t, ctx, db, now)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "shared-scheduler-pg-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := db.WithContext(ctx).Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	sealer := NewService(db, nil, WithPlatformBillingOperatorTenant(operatorTenantID))
	sealer.now = func() time.Time { return now }
	if _, created, err := sealer.SealSharedTargetLedgerCoverageAuthorized(
		ctx,
		identity.Principal{UserID: operatorUserID, ActiveTenantID: &operatorTenantID},
		operatorTenantID,
		target.ID,
		SealSharedTargetLedgerCoverageInput{
			CompleteFromAt: now.Add(-40 * time.Minute), MinimumWriterVersion: "stage4-pg-scheduler-v1",
			DeploymentAttestationSHA256: strings.Repeat("b", 64),
		},
		"shared-scheduler-pg-seal",
		"127.0.0.1",
	); err != nil || !created {
		t.Fatalf("seal shared scheduler coverage: created=%v err=%v", created, err)
	}

	job := ConfiguredSharedAllocation{
		ExecutionTargetID: target.ID, Provider: "aws", CurrencyCode: "USD",
		BillingPeriodStartAt: now.Add(-30 * time.Minute), BillingPeriodEndAt: now.Add(-20 * time.Minute),
		SettlementDelay: time.Minute, ScheduleInterval: time.Hour,
	}
	newBillingService := func() *Service {
		service := NewService(
			db,
			nil,
			WithConfiguredSharedAllocations([]ConfiguredSharedAllocation{job}),
			WithPlatformBillingOperatorTenant(operatorTenantID),
		)
		service.now = func() time.Time { return now }
		return service
	}
	firstLeadership, err := leadership.New(db, leadership.Config{
		HolderID: "shared-billing-scheduler-a-" + uuid.NewString(), LeaseTTL: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondLeadership, err := leadership.New(db, leadership.Config{
		HolderID: "shared-billing-scheduler-b-" + uuid.NewString(), LeaseTTL: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	newRunner := func(service *leadership.Service) *reconcilerleadership.Runner {
		runner, runnerErr := reconcilerleadership.NewRunner(service, reconcilerleadership.RunnerConfig{
			LeaseName: "synara:billing-shared-allocation-scheduler", CycleInterval: time.Hour,
			AcquireRetryDelay: 20 * time.Millisecond, RenewInterval: 250 * time.Millisecond,
			AssertInterval: 100 * time.Millisecond, Logger: logger,
		})
		if runnerErr != nil {
			t.Fatal(runnerErr)
		}
		return runner
	}
	firstRunner := newRunner(firstLeadership)
	secondRunner := newRunner(secondLeadership)

	firstOutcomes := make(chan postgresSharedSchedulerOutcome, 1)
	secondOutcomes := make(chan postgresSharedSchedulerOutcome, 1)
	firstCtx, stopFirst := context.WithCancel(ctx)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstRunner.Run(firstCtx, func(run reconcilerleadership.RunContext) error {
			summary, runErr := newBillingService().RunSharedAllocationSchedulerOnce(run.Context)
			select {
			case firstOutcomes <- postgresSharedSchedulerOutcome{summary: summary, err: runErr}:
			default:
			}
			return runErr
		})
	}()
	first := waitPostgresSharedSchedulerOutcome(t, firstOutcomes, "first leader")
	assertCompletedPostgresSharedSchedulerOutcome(t, first)

	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		secondRunner.Run(secondCtx, func(run reconcilerleadership.RunContext) error {
			summary, runErr := newBillingService().RunSharedAllocationSchedulerOnce(run.Context)
			select {
			case secondOutcomes <- postgresSharedSchedulerOutcome{summary: summary, err: runErr}:
			default:
			}
			return runErr
		})
	}()
	select {
	case outcome := <-secondOutcomes:
		t.Fatalf("standby scheduler ran before leadership handoff: %#v", outcome)
	case <-time.After(200 * time.Millisecond):
	}

	// The due cursor is durable rather than process-local. Advance exactly one
	// configured interval before takeover so the new leader performs a replay;
	// an immediate takeover is covered separately as a durable skip.
	now = now.Add(time.Hour)
	stopFirst()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first shared scheduler leader did not release its lease")
	}
	second := waitPostgresSharedSchedulerOutcome(t, secondOutcomes, "takeover leader")
	assertCompletedPostgresSharedSchedulerOutcome(t, second)
	stopSecond()
	select {
	case <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("takeover shared scheduler leader did not stop")
	}

	var scheduledAudits int64
	if err := db.WithContext(ctx).Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", operatorTenantID,
			"billing.shared_cost_allocation_sweep_scheduled", target.ID).
		Count(&scheduledAudits).Error; err != nil {
		t.Fatal(err)
	}
	if scheduledAudits != 2 {
		t.Fatalf("scheduled shared allocation audit count after handoff = %d, want 2", scheduledAudits)
	}
	var lease persistence.ReconcilerLease
	if err := db.WithContext(ctx).
		Where("lease_name = ?", "synara:billing-shared-allocation-scheduler").
		Take(&lease).Error; err != nil {
		t.Fatal(err)
	}
	if lease.FencingToken != 2 {
		t.Fatalf("shared scheduler handoff fencing token = %d, want 2", lease.FencingToken)
	}
}

func TestPostgresMonthlySharedAllocationScheduleDurableClaimAllowsOneReplica(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db := openBillingPostgresIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(12)

	wallNow := time.Now().UTC()
	periodEnd := time.Date(wallNow.Year(), wallNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodStart := periodEnd.AddDate(0, -1, 0)
	now := periodEnd.Add(2*time.Hour + 123456789*time.Nanosecond)
	operatorUserID, operatorTenantID := seedPostgresSharedBillingOperator(t, ctx, db, now)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "shared-calendar-pg-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{},
		CreatedAt: periodStart.Add(-time.Hour), UpdatedAt: periodStart.Add(-time.Hour),
	}
	if err := db.WithContext(ctx).Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	sealer := NewService(db, nil, WithPlatformBillingOperatorTenant(operatorTenantID))
	sealer.now = func() time.Time { return now }
	if _, created, err := sealer.SealSharedTargetLedgerCoverageAuthorized(
		ctx,
		identity.Principal{UserID: operatorUserID, ActiveTenantID: &operatorTenantID},
		operatorTenantID,
		target.ID,
		SealSharedTargetLedgerCoverageInput{
			CompleteFromAt: periodStart.Add(-time.Minute), MinimumWriterVersion: "stage4-pg-calendar-v1",
			DeploymentAttestationSHA256: strings.Repeat("c", 64),
		},
		"shared-calendar-pg-seal",
		"127.0.0.1",
	); err != nil || !created {
		t.Fatalf("seal monthly shared scheduler coverage: created=%v err=%v", created, err)
	}

	lastPeriodEnd := periodEnd
	job := ConfiguredSharedAllocation{
		ExecutionTargetID: target.ID, Provider: "aws", CurrencyCode: "USD",
		Calendar:           SharedAllocationCalendarMonthlyUTC,
		FirstPeriodStartAt: periodStart, LastPeriodEndAt: &lastPeriodEnd,
		SettlementDelay: time.Hour, ScheduleInterval: time.Hour,
	}
	newBillingService := func() *Service {
		service := NewService(
			db,
			nil,
			WithConfiguredSharedAllocations([]ConfiguredSharedAllocation{job}),
			WithPlatformBillingOperatorTenant(operatorTenantID),
		)
		service.now = func() time.Time { return now }
		return service
	}

	start := make(chan struct{})
	outcomes := make(chan postgresSharedSchedulerOutcome, 2)
	for range 2 {
		go func() {
			<-start
			summary, runErr := newBillingService().RunSharedAllocationSchedulerOnce(ctx)
			outcomes <- postgresSharedSchedulerOutcome{summary: summary, err: runErr}
		}()
	}
	close(start)
	first := <-outcomes
	second := <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent monthly scheduler errors: first=%v second=%v", first.err, second.err)
	}
	attempted := 0
	skipped := 0
	for _, outcome := range []postgresSharedSchedulerOutcome{first, second} {
		if outcome.summary.Checked != 1 || outcome.summary.GeneratedCalendarPeriods != 1 {
			t.Fatalf("monthly scheduler generated summary = %#v", outcome.summary)
		}
		attempted += outcome.summary.Attempted
		skipped += outcome.summary.Skipped
	}
	if attempted != 1 || skipped != 1 {
		t.Fatalf("concurrent monthly scheduler split attempted=%d skipped=%d first=%#v second=%#v", attempted, skipped, first, second)
	}

	var state persistence.BillingSharedAllocationSchedulePeriod
	if err := db.WithContext(ctx).
		Where("execution_target_id = ? AND billing_period_start_at = ? AND billing_period_end_at = ?", target.ID, periodStart, periodEnd).
		Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.ScheduleKind != SharedAllocationCalendarMonthlyUTC || state.AttemptCount != 1 ||
		state.LastOutcome != "completed" || state.LastSuccessAt == nil {
		t.Fatalf("PostgreSQL monthly schedule state = %#v", state)
	}
	var scheduledAudits int64
	if err := db.WithContext(ctx).Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND resource_id = ?", operatorTenantID,
			"billing.shared_cost_allocation_sweep_scheduled", target.ID).
		Count(&scheduledAudits).Error; err != nil {
		t.Fatal(err)
	}
	if scheduledAudits != 1 {
		t.Fatalf("concurrent monthly scheduler audit count = %d, want 1", scheduledAudits)
	}
	payload, err := observability.New(db).Gather(ctx)
	if err != nil {
		t.Fatalf("gather PostgreSQL monthly schedule metrics: %v", err)
	}
	if expected := `synara_billing_shared_allocation_schedule_periods{due_state="due",outcome="completed",schedule_kind="monthly-utc"}`; !strings.Contains(string(payload), expected) {
		t.Fatalf("PostgreSQL monthly schedule metrics omitted %q:\n%s", expected, payload)
	}
}

func seedPostgresSharedBillingOperator(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	now time.Time,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	operatorUserID := uuid.New()
	operatorTenantID := uuid.New()
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: operatorUserID, Email: "shared-billing-pg-" + uuid.NewString() + "@example.com",
			DisplayName: "Shared billing PostgreSQL operator", Status: "active",
			EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.Tenant{
			ID: operatorTenantID, Slug: "shared-billing-pg-" + uuid.NewString(),
			Name: "Shared billing PostgreSQL operator", Status: "active", PlanCode: "test", Region: "local",
			Settings: map[string]any{}, CreatedBy: operatorUserID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: operatorTenantID, UserID: operatorUserID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return operatorUserID, operatorTenantID
}

func waitPostgresSharedSchedulerOutcome(
	t *testing.T,
	outcomes <-chan postgresSharedSchedulerOutcome,
	label string,
) postgresSharedSchedulerOutcome {
	t.Helper()
	select {
	case outcome := <-outcomes:
		return outcome
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not run", label)
		return postgresSharedSchedulerOutcome{}
	}
}

func assertCompletedPostgresSharedSchedulerOutcome(
	t *testing.T,
	outcome postgresSharedSchedulerOutcome,
) {
	t.Helper()
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.summary.Checked != 1 || outcome.summary.Attempted != 1 || outcome.summary.Completed != 1 ||
		outcome.summary.Workers != 0 || outcome.summary.Failed != 0 {
		t.Fatalf("unexpected shared scheduler outcome: %#v", outcome)
	}
}

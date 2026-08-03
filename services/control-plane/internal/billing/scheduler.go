package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type SchedulerRunSummary struct {
	Checked                int
	Imported               int
	Reconciled             int
	EstimateWorkers        int
	EstimateSweeps         int
	EstimateWorkerFailures int
	Skipped                int
	Failed                 int
}

type schedulerStateKey struct {
	tenantID         uuid.UUID
	provider         string
	externalImportID string
}

type scheduledImportState struct {
	lastStartedAt time.Time
}

type SharedAllocationSchedulerRunSummary struct {
	Checked                  int
	GeneratedCalendarPeriods int
	Attempted                int
	Completed                int
	Workers                  int
	AllocationRuns           int
	AllocationSlices         int
	FailedWorkers            int
	NotSettled               int
	Skipped                  int
	Failed                   int
}

const maxGeneratedSharedAllocationPeriods = 1200

type sharedAllocationSchedulerJob struct {
	ExecutionTargetID      uuid.UUID
	Provider               string
	CurrencyCode           string
	BillingPeriodStartAt   time.Time
	BillingPeriodEndAt     time.Time
	SettlementDelay        time.Duration
	ScheduleInterval       time.Duration
	ScheduleKind           string
	ScheduleConfigSHA256   string
	FirstConfiguredStartAt time.Time
	LastConfiguredEndAt    *time.Time
}

func (s *Service) RunImportSchedulerOnce(ctx context.Context) (SchedulerRunSummary, error) {
	summary := SchedulerRunSummary{}
	if s == nil || len(s.configuredImports) == 0 {
		return summary, nil
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	jobs := s.scheduledImports()
	var failures []error
	for _, job := range jobs {
		summary.Checked++
		if !s.schedulerJobDue(job, now) {
			summary.Skipped++
			continue
		}
		s.markSchedulerStarted(job, now)
		requestID := "billing-import-scheduler:" + uuid.NewString()
		imported, importErr := s.importConfiguredInvoiceScheduled(ctx, job, requestID)
		if importErr != nil {
			summary.Failed++
			failures = append(failures, fmt.Errorf("tenant %s %s/%s import: %w", job.TenantID, job.Provider, job.ExternalImportID, importErr))
			continue
		}
		if imported.Created {
			summary.Imported++
		}
		if job.EstimateAfterImport {
			sweepResult, sweepErr := s.runEstimateSweep(ctx, job, imported.Result.Import, "billing-import-scheduler")
			summary.EstimateWorkers += sweepResult.WorkerCount
			summary.EstimateSweeps += sweepResult.EstimateCount
			summary.EstimateWorkerFailures += sweepResult.FailedWorkerCount
			if sweepErr != nil {
				summary.Failed++
				failures = append(failures, fmt.Errorf("tenant %s %s/%s estimate sweep: %w", job.TenantID, job.Provider, job.ExternalImportID, sweepErr))
				// A partial estimate set is not authoritative reconciliation input.
				// The immutable invoice import remains the retry cursor, so a later
				// scheduler cycle can replay the sweep before reconciliation.
				continue
			}
		}
		if !job.Reconcile {
			continue
		}
		reconciled, reconcileErr := s.reconcileActualInvoiceImportScheduled(ctx, job, imported.Result.Import.ID, requestID)
		if reconcileErr != nil {
			summary.Failed++
			failures = append(failures, fmt.Errorf("tenant %s %s/%s reconcile: %w", job.TenantID, job.Provider, job.ExternalImportID, reconcileErr))
			continue
		}
		if reconciled.Changed {
			summary.Reconciled++
		}
	}
	return summary, errors.Join(failures...)
}

func (s *Service) scheduledImports() []ConfiguredImport {
	jobs := make([]ConfiguredImport, 0, len(s.configuredImports))
	for _, configuredImport := range s.configuredImports {
		if configuredImport.ScheduleInterval > 0 {
			jobs = append(jobs, configuredImport)
		}
	}
	return jobs
}

func (s *Service) schedulerJobDue(configuredImport ConfiguredImport, now time.Time) bool {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	state, ok := s.schedulerState[schedulerKey(configuredImport)]
	if !ok {
		return true
	}
	return now.Sub(state.lastStartedAt) >= configuredImport.ScheduleInterval
}

func (s *Service) markSchedulerStarted(configuredImport ConfiguredImport, startedAt time.Time) {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.schedulerState == nil {
		s.schedulerState = make(map[schedulerStateKey]scheduledImportState)
	}
	s.schedulerState[schedulerKey(configuredImport)] = scheduledImportState{lastStartedAt: startedAt}
}

func schedulerKey(configuredImport ConfiguredImport) schedulerStateKey {
	return schedulerStateKey{
		tenantID: configuredImport.TenantID, provider: configuredImport.Provider, externalImportID: configuredImport.ExternalImportID,
	}
}

// RunSharedAllocationSchedulerOnce retries exact static periods and periods
// generated from explicit monthly-UTC anchors. Multi-replica exclusion is
// owned by the caller's reconciler-leadership Runner; each period also has a
// durable row-lock claim, and allocation identity locks remain the final
// correctness fence during failover overlap.
func (s *Service) RunSharedAllocationSchedulerOnce(
	ctx context.Context,
) (SharedAllocationSchedulerRunSummary, error) {
	summary := SharedAllocationSchedulerRunSummary{}
	if s == nil || len(s.configuredSharedAllocations) == 0 {
		return summary, nil
	}
	s.sharedSchedulerRunMu.Lock()
	defer s.sharedSchedulerRunMu.Unlock()
	now := s.now().UTC()
	jobs, err := s.scheduledSharedAllocations(now)
	if err != nil {
		summary.Failed++
		return summary, err
	}
	var failures []error
	for _, job := range jobs {
		summary.Checked++
		if job.ScheduleKind == SharedAllocationCalendarMonthlyUTC {
			summary.GeneratedCalendarPeriods++
		}
		settledAt := job.BillingPeriodEndAt.Add(job.SettlementDelay)
		if now.Before(settledAt) {
			summary.NotSettled++
			continue
		}
		claimed, claimErr := s.claimSharedAllocationSchedulerJob(ctx, job, now)
		if claimErr != nil {
			summary.Failed++
			failures = append(failures, fmt.Errorf(
				"Target %s %s/%s %s..%s shared allocation schedule claim: %w",
				job.ExecutionTargetID,
				job.Provider,
				job.CurrencyCode,
				job.BillingPeriodStartAt.Format(time.RFC3339Nano),
				job.BillingPeriodEndAt.Format(time.RFC3339Nano),
				claimErr,
			))
			continue
		}
		if !claimed {
			summary.Skipped++
			continue
		}
		summary.Attempted++
		requestID := "billing-shared-allocation-scheduler:" + uuid.NewString()
		result, sweepErr := s.sweepSharedUsageChargesScheduled(ctx, job, requestID)
		summary.Workers += result.WorkerCount
		summary.AllocationRuns += result.AllocationRunCount
		summary.AllocationSlices += result.AllocationSliceCount
		summary.FailedWorkers += result.FailedWorkerCount
		finishErr := s.finishSharedAllocationSchedulerJob(ctx, job, now, sweepErr)
		attemptErr := errors.Join(sweepErr, finishErr)
		if attemptErr != nil {
			summary.Failed++
			failures = append(failures, fmt.Errorf(
				"Target %s %s/%s %s..%s shared allocation sweep: %w",
				job.ExecutionTargetID,
				job.Provider,
				job.CurrencyCode,
				job.BillingPeriodStartAt.Format(time.RFC3339Nano),
				job.BillingPeriodEndAt.Format(time.RFC3339Nano),
				attemptErr,
			))
			continue
		}
		summary.Completed++
	}
	return summary, errors.Join(failures...)
}

func (s *Service) sweepSharedUsageChargesScheduled(
	ctx context.Context,
	job sharedAllocationSchedulerJob,
	requestID string,
) (SharedUsageAllocationSweepResult, error) {
	if s.platformBillingOperatorTenantID == uuid.Nil {
		return SharedUsageAllocationSweepResult{}, problem.New(
			503,
			"cost_accounting_shared_scheduler_operator_unavailable",
			"The shared billing scheduler requires an explicitly configured platform billing operator Tenant.",
		)
	}
	normalized, err := s.normalizeSharedUsageSweep(ctx, SweepSharedUsageChargesInput{
		ExecutionTargetID:    job.ExecutionTargetID,
		Provider:             job.Provider,
		CurrencyCode:         job.CurrencyCode,
		BillingPeriodStartAt: job.BillingPeriodStartAt,
		BillingPeriodEndAt:   job.BillingPeriodEndAt,
	})
	if err != nil {
		return SharedUsageAllocationSweepResult{}, err
	}
	if err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		metadata := map[string]any{
			"provider": normalized.Provider, "currencyCode": normalized.CurrencyCode,
			"billingPeriodStartAt": normalized.PeriodStart, "billingPeriodEndAt": normalized.PeriodEnd,
			"settlementDelaySeconds":  int64(job.SettlementDelay / time.Second),
			"scheduleIntervalSeconds": int64(job.ScheduleInterval / time.Second),
			"scheduleKind":            job.ScheduleKind,
			"scheduleConfigSha256":    job.ScheduleConfigSHA256,
		}
		if job.ScheduleKind == SharedAllocationCalendarMonthlyUTC {
			metadata["firstPeriodStartAt"] = job.FirstConfiguredStartAt
			if job.LastConfiguredEndAt != nil {
				metadata["lastPeriodEndAt"] = *job.LastConfiguredEndAt
			}
		}
		return s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID:     s.platformBillingOperatorTenantID,
			ActorType:    "system",
			Action:       "cost_accounting.shared_cost_allocation_sweep_scheduled",
			ResourceType: "execution_target", ResourceID: &normalized.ExecutionTargetID,
			RequestID: requestID,
			Metadata:  metadata,
		})
	}); err != nil {
		return SharedUsageAllocationSweepResult{}, err
	}
	result, err := s.sweepSharedUsageCharges(ctx, normalized)
	if err != nil {
		return result, err
	}
	if result.FailedWorkerCount == 0 {
		return result, nil
	}
	partialErr := problem.New(
		409,
		"cost_accounting_shared_allocation_sweep_partial_failure",
		"One or more shared Worker allocations failed; the configured period must be retried.",
	)
	partialErr.Details = map[string]any{
		"workerCount": result.WorkerCount, "allocationRunCount": result.AllocationRunCount,
		"allocationSliceCount": result.AllocationSliceCount, "failedWorkerCount": result.FailedWorkerCount,
	}
	return result, partialErr
}

func (s *Service) scheduledSharedAllocations(now time.Time) ([]sharedAllocationSchedulerJob, error) {
	configured := make([]ConfiguredSharedAllocation, 0, len(s.configuredSharedAllocations))
	for _, raw := range s.configuredSharedAllocations {
		if raw.ScheduleInterval > 0 {
			configured = append(configured, raw)
		}
	}
	slices.SortFunc(configured, compareConfiguredSharedAllocations)
	jobs := make([]sharedAllocationSchedulerJob, 0, len(configured))
	for _, raw := range configured {
		normalized, err := raw.Normalize()
		if err != nil {
			return nil, problem.Wrap(
				500,
				"cost_accounting_shared_scheduler_configuration_invalid",
				"A configured shared allocation schedule is invalid.",
				err,
			)
		}
		digest := sharedAllocationScheduleConfigSHA256(normalized)
		if normalized.Calendar == "" {
			jobs = append(jobs, newSharedAllocationSchedulerJob(
				normalized,
				normalized.BillingPeriodStartAt,
				normalized.BillingPeriodEndAt,
				"static",
				digest,
			))
			continue
		}
		generated, err := expandMonthlySharedAllocationSchedule(normalized, now, digest)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, generated...)
	}
	slices.SortFunc(jobs, compareSharedAllocationSchedulerJobs)
	return jobs, nil
}

func expandMonthlySharedAllocationSchedule(
	configured ConfiguredSharedAllocation,
	now time.Time,
	digest string,
) ([]sharedAllocationSchedulerJob, error) {
	if configured.Calendar != SharedAllocationCalendarMonthlyUTC {
		return nil, problem.New(
			500,
			"cost_accounting_shared_scheduler_calendar_unsupported",
			"The configured shared allocation calendar is unsupported.",
		)
	}
	now = now.UTC()
	periodStart := configured.FirstPeriodStartAt.UTC()
	jobs := make([]sharedAllocationSchedulerJob, 0)
	for generated := 0; ; generated++ {
		periodEnd := periodStart.AddDate(0, 1, 0)
		if periodEnd.After(now) ||
			(configured.LastPeriodEndAt != nil && periodEnd.After(configured.LastPeriodEndAt.UTC())) {
			break
		}
		if generated >= maxGeneratedSharedAllocationPeriods {
			return nil, problem.New(
				500,
				"cost_accounting_shared_scheduler_calendar_range_exceeded",
				"A monthly shared allocation schedule expanded beyond 1200 periods.",
			)
		}
		jobs = append(jobs, newSharedAllocationSchedulerJob(
			configured,
			periodStart,
			periodEnd,
			SharedAllocationCalendarMonthlyUTC,
			digest,
		))
		periodStart = periodEnd
	}
	return jobs, nil
}

func newSharedAllocationSchedulerJob(
	configured ConfiguredSharedAllocation,
	periodStart, periodEnd time.Time,
	scheduleKind, digest string,
) sharedAllocationSchedulerJob {
	var lastPeriodEnd *time.Time
	if configured.LastPeriodEndAt != nil {
		last := configured.LastPeriodEndAt.UTC()
		lastPeriodEnd = &last
	}
	return sharedAllocationSchedulerJob{
		ExecutionTargetID:      configured.ExecutionTargetID,
		Provider:               configured.Provider,
		CurrencyCode:           configured.CurrencyCode,
		BillingPeriodStartAt:   periodStart.UTC(),
		BillingPeriodEndAt:     periodEnd.UTC(),
		SettlementDelay:        configured.SettlementDelay,
		ScheduleInterval:       configured.ScheduleInterval,
		ScheduleKind:           scheduleKind,
		ScheduleConfigSHA256:   digest,
		FirstConfiguredStartAt: configured.FirstPeriodStartAt.UTC(),
		LastConfiguredEndAt:    lastPeriodEnd,
	}
}

func compareSharedAllocationSchedulerJobs(left, right sharedAllocationSchedulerJob) int {
	switch {
	case left.ExecutionTargetID != right.ExecutionTargetID:
		return strings.Compare(left.ExecutionTargetID.String(), right.ExecutionTargetID.String())
	case left.Provider != right.Provider:
		return strings.Compare(left.Provider, right.Provider)
	case left.CurrencyCode != right.CurrencyCode:
		return strings.Compare(left.CurrencyCode, right.CurrencyCode)
	case !left.BillingPeriodStartAt.Equal(right.BillingPeriodStartAt):
		if left.BillingPeriodStartAt.Before(right.BillingPeriodStartAt) {
			return -1
		}
		return 1
	default:
		return strings.Compare(left.ScheduleConfigSHA256, right.ScheduleConfigSHA256)
	}
}

func sharedAllocationScheduleConfigSHA256(configured ConfiguredSharedAllocation) string {
	parts := []string{
		"synara/billing-shared-allocation-schedule/v1",
		configured.ExecutionTargetID.String(),
		configured.Provider,
		configured.CurrencyCode,
		configured.Calendar,
		configured.BillingPeriodStartAt.UTC().Format(time.RFC3339Nano),
		configured.BillingPeriodEndAt.UTC().Format(time.RFC3339Nano),
		configured.FirstPeriodStartAt.UTC().Format(time.RFC3339Nano),
		nullableConfiguredTime(configured.LastPeriodEndAt),
		strconv.FormatInt(int64(configured.SettlementDelay/time.Second), 10),
		strconv.FormatInt(int64(configured.ScheduleInterval/time.Second), 10),
	}
	canonical := strings.Join(parts, "\x00") + "\x00"
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func (s *Service) claimSharedAllocationSchedulerJob(
	ctx context.Context,
	job sharedAllocationSchedulerJob,
	startedAt time.Time,
) (bool, error) {
	startedAt = startedAt.UTC().Truncate(time.Microsecond)
	claimed := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		initialNextAttemptAt := job.BillingPeriodEndAt.Add(job.SettlementDelay)
		state := persistence.BillingSharedAllocationSchedulePeriod{
			ScheduleConfigSHA256:    job.ScheduleConfigSHA256,
			BillingPeriodStartAt:    job.BillingPeriodStartAt,
			BillingPeriodEndAt:      job.BillingPeriodEndAt,
			ExecutionTargetID:       job.ExecutionTargetID,
			Provider:                job.Provider,
			CurrencyCode:            job.CurrencyCode,
			ScheduleKind:            job.ScheduleKind,
			SettlementDelaySeconds:  int64(job.SettlementDelay / time.Second),
			ScheduleIntervalSeconds: int64(job.ScheduleInterval / time.Second),
			NextAttemptAt:           initialNextAttemptAt,
			LastOutcome:             "never",
			CreatedAt:               startedAt,
			UpdatedAt:               startedAt,
		}
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; err != nil {
			return problem.Wrap(500, "cost_accounting_shared_scheduler_state_create_failed", "Shared allocation schedule state could not be created.", err)
		}
		state = persistence.BillingSharedAllocationSchedulePeriod{}
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
				job.ScheduleConfigSHA256,
				job.BillingPeriodStartAt,
				job.BillingPeriodEndAt,
			).
			Take(&state).Error; err != nil {
			return problem.Wrap(500, "cost_accounting_shared_scheduler_state_load_failed", "Shared allocation schedule state could not be locked.", err)
		}
		if !sharedAllocationScheduleStateMatchesJob(state, job) {
			return problem.New(409, "cost_accounting_shared_scheduler_state_conflict", "Durable shared allocation schedule state does not match its configured period.")
		}
		if startedAt.Before(state.NextAttemptAt) {
			return nil
		}
		nextAttemptAt := startedAt.Add(job.ScheduleInterval)
		result := tx.WithContext(ctx).Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
			Where(
				"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ? AND attempt_count = ?",
				state.ScheduleConfigSHA256,
				state.BillingPeriodStartAt,
				state.BillingPeriodEndAt,
				state.AttemptCount,
			).
			Updates(map[string]any{
				"attempt_count":   state.AttemptCount + 1,
				"last_started_at": startedAt,
				"last_outcome":    "running",
				"last_error_code": nil,
				"next_attempt_at": nextAttemptAt,
				"updated_at":      startedAt,
			})
		if result.Error != nil {
			return problem.Wrap(500, "cost_accounting_shared_scheduler_state_claim_failed", "Shared allocation schedule period could not be claimed.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "cost_accounting_shared_scheduler_state_claim_conflict", "Shared allocation schedule period changed before claim.")
		}
		claimed = true
		return nil
	})
	return claimed, err
}

func (s *Service) finishSharedAllocationSchedulerJob(
	ctx context.Context,
	job sharedAllocationSchedulerJob,
	startedAt time.Time,
	attemptErr error,
) error {
	startedAt = startedAt.UTC().Truncate(time.Microsecond)
	finishedAt := s.now().UTC().Truncate(time.Microsecond)
	if finishedAt.Before(startedAt) {
		finishedAt = startedAt
	}
	outcome := "completed"
	updates := map[string]any{
		"last_finished_at": finishedAt,
		"last_success_at":  finishedAt,
		"last_outcome":     outcome,
		"last_error_code":  nil,
		"updated_at":       finishedAt,
	}
	if attemptErr != nil {
		outcome = "failed"
		updates["last_outcome"] = outcome
		updates["last_error_code"] = boundedSharedAllocationSchedulerErrorCode(attemptErr)
		delete(updates, "last_success_at")
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var state persistence.BillingSharedAllocationSchedulePeriod
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where(
				"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ?",
				job.ScheduleConfigSHA256,
				job.BillingPeriodStartAt,
				job.BillingPeriodEndAt,
			).
			Take(&state).Error; err != nil {
			return problem.Wrap(500, "cost_accounting_shared_scheduler_state_finish_load_failed", "Shared allocation schedule state could not be loaded for completion.", err)
		}
		if state.LastStartedAt == nil || !state.LastStartedAt.Equal(startedAt) || state.LastOutcome != "running" {
			return problem.New(409, "cost_accounting_shared_scheduler_state_finish_conflict", "Shared allocation schedule claim changed before completion.")
		}
		result := tx.WithContext(ctx).Model(&persistence.BillingSharedAllocationSchedulePeriod{}).
			Where(
				"schedule_config_sha256 = ? AND billing_period_start_at = ? AND billing_period_end_at = ? AND attempt_count = ? AND last_outcome = ?",
				state.ScheduleConfigSHA256,
				state.BillingPeriodStartAt,
				state.BillingPeriodEndAt,
				state.AttemptCount,
				"running",
			).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "cost_accounting_shared_scheduler_state_finish_failed", "Shared allocation schedule outcome could not be persisted.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "cost_accounting_shared_scheduler_state_finish_conflict", "Shared allocation schedule claim changed before completion.")
		}
		return nil
	})
}

func sharedAllocationScheduleStateMatchesJob(
	state persistence.BillingSharedAllocationSchedulePeriod,
	job sharedAllocationSchedulerJob,
) bool {
	return state.ScheduleConfigSHA256 == job.ScheduleConfigSHA256 &&
		state.BillingPeriodStartAt.Equal(job.BillingPeriodStartAt) &&
		state.BillingPeriodEndAt.Equal(job.BillingPeriodEndAt) &&
		state.ExecutionTargetID == job.ExecutionTargetID &&
		state.Provider == job.Provider &&
		state.CurrencyCode == job.CurrencyCode &&
		state.ScheduleKind == job.ScheduleKind &&
		state.SettlementDelaySeconds == int64(job.SettlementDelay/time.Second) &&
		state.ScheduleIntervalSeconds == int64(job.ScheduleInterval/time.Second)
}

func boundedSharedAllocationSchedulerErrorCode(err error) string {
	code := "scheduler_error"
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		code = strings.ToLower(strings.TrimSpace(apiError.Code))
	}
	if code == "" || len(code) > 120 {
		return "scheduler_error"
	}
	for index, character := range code {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			(character == '_' || character == '-') && index > 0 {
			continue
		}
		return "scheduler_error"
	}
	return code
}

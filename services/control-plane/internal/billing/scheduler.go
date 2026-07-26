package billing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

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
	Checked          int
	Attempted        int
	Completed        int
	Workers          int
	AllocationRuns   int
	AllocationSlices int
	FailedWorkers    int
	NotSettled       int
	Skipped          int
	Failed           int
}

type sharedSchedulerStateKey struct {
	identity string
}

type scheduledSharedAllocationState struct {
	lastStartedAt time.Time
}

func (s *Service) RunImportSchedulerOnce(ctx context.Context) (SchedulerRunSummary, error) {
	summary := SchedulerRunSummary{}
	if s == nil || len(s.configuredImports) == 0 {
		return summary, nil
	}
	now := s.now().UTC()
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

// RunSharedAllocationSchedulerOnce retries only explicitly configured,
// already-settled periods. Multi-replica exclusion is owned by the caller's
// reconciler-leadership Runner; allocation identity locks remain the final
// correctness fence if an old and new leader briefly overlap during failover.
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
	jobs := s.scheduledSharedAllocations()
	var failures []error
	for _, job := range jobs {
		summary.Checked++
		settledAt := job.BillingPeriodEndAt.Add(job.SettlementDelay)
		if now.Before(settledAt) {
			summary.NotSettled++
			continue
		}
		if !s.sharedSchedulerJobDue(job, now) {
			summary.Skipped++
			continue
		}
		s.markSharedSchedulerStarted(job, now)
		summary.Attempted++
		requestID := "billing-shared-allocation-scheduler:" + uuid.NewString()
		result, sweepErr := s.sweepSharedUsageChargesScheduled(ctx, job, requestID)
		summary.Workers += result.WorkerCount
		summary.AllocationRuns += result.AllocationRunCount
		summary.AllocationSlices += result.AllocationSliceCount
		summary.FailedWorkers += result.FailedWorkerCount
		if sweepErr != nil {
			summary.Failed++
			failures = append(failures, fmt.Errorf(
				"Target %s %s/%s %s..%s shared allocation sweep: %w",
				job.ExecutionTargetID,
				job.Provider,
				job.CurrencyCode,
				job.BillingPeriodStartAt.Format(time.RFC3339Nano),
				job.BillingPeriodEndAt.Format(time.RFC3339Nano),
				sweepErr,
			))
			continue
		}
		summary.Completed++
	}
	return summary, errors.Join(failures...)
}

func (s *Service) sweepSharedUsageChargesScheduled(
	ctx context.Context,
	job ConfiguredSharedAllocation,
	requestID string,
) (SharedUsageAllocationSweepResult, error) {
	if s.platformBillingOperatorTenantID == uuid.Nil {
		return SharedUsageAllocationSweepResult{}, problem.New(
			503,
			"billing_shared_scheduler_operator_unavailable",
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
		return s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID:     s.platformBillingOperatorTenantID,
			ActorType:    "system",
			Action:       "billing.shared_cost_allocation_sweep_scheduled",
			ResourceType: "execution_target", ResourceID: &normalized.ExecutionTargetID,
			RequestID: requestID,
			Metadata: map[string]any{
				"provider": normalized.Provider, "currencyCode": normalized.CurrencyCode,
				"billingPeriodStartAt": normalized.PeriodStart, "billingPeriodEndAt": normalized.PeriodEnd,
				"settlementDelaySeconds":  int64(job.SettlementDelay / time.Second),
				"scheduleIntervalSeconds": int64(job.ScheduleInterval / time.Second),
			},
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
		"billing_shared_allocation_sweep_partial_failure",
		"One or more shared Worker allocations failed; the configured period must be retried.",
	)
	partialErr.Details = map[string]any{
		"workerCount": result.WorkerCount, "allocationRunCount": result.AllocationRunCount,
		"allocationSliceCount": result.AllocationSliceCount, "failedWorkerCount": result.FailedWorkerCount,
	}
	return result, partialErr
}

func (s *Service) scheduledSharedAllocations() []ConfiguredSharedAllocation {
	jobs := make([]ConfiguredSharedAllocation, 0, len(s.configuredSharedAllocations))
	for _, configuredAllocation := range s.configuredSharedAllocations {
		if configuredAllocation.ScheduleInterval > 0 {
			jobs = append(jobs, configuredAllocation)
		}
	}
	slices.SortFunc(jobs, compareConfiguredSharedAllocations)
	return jobs
}

func (s *Service) sharedSchedulerJobDue(job ConfiguredSharedAllocation, now time.Time) bool {
	s.sharedSchedulerMu.Lock()
	defer s.sharedSchedulerMu.Unlock()
	state, ok := s.sharedSchedulerState[sharedSchedulerKey(job)]
	if !ok {
		return true
	}
	return now.Sub(state.lastStartedAt) >= job.ScheduleInterval
}

func (s *Service) markSharedSchedulerStarted(job ConfiguredSharedAllocation, startedAt time.Time) {
	s.sharedSchedulerMu.Lock()
	defer s.sharedSchedulerMu.Unlock()
	if s.sharedSchedulerState == nil {
		s.sharedSchedulerState = make(map[sharedSchedulerStateKey]scheduledSharedAllocationState)
	}
	s.sharedSchedulerState[sharedSchedulerKey(job)] = scheduledSharedAllocationState{lastStartedAt: startedAt}
}

func sharedSchedulerKey(job ConfiguredSharedAllocation) sharedSchedulerStateKey {
	return sharedSchedulerStateKey{identity: configuredSharedAllocationKey(job)}
}

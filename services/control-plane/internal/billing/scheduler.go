package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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
		imported, importErr := s.importConfiguredInvoiceScheduled(ctx, job)
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
		reconciled, reconcileErr := s.reconcileActualInvoiceImportScheduled(ctx, job, imported.Result.Import.ID)
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

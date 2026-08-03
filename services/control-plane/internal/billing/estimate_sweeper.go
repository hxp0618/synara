package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const estimateSweepPageSize = 200

// SweepImportedInvoice derives immutable estimate rows from retained Worker
// incarnation facts for one explicit tenant-owned Target set. The imported
// invoice is the durable retry cursor: replaying the same import safely runs
// this method again because estimate row identities are deterministic.
func (s *Service) SweepImportedInvoice(
	ctx context.Context,
	request ScheduledEstimateSweepRequest,
) (ScheduledEstimateSweepResult, error) {
	if s == nil || s.db == nil {
		return ScheduledEstimateSweepResult{}, problem.New(
			500,
			"cost_accounting_estimate_sweeper_unavailable",
			"The cost estimate sweeper is not initialized.",
		)
	}
	provider, currency, periodStart, periodEnd, targetIDs, err := normalizeEstimateSweepRequest(request)
	if err != nil {
		return ScheduledEstimateSweepResult{}, err
	}
	if err := validateEstimateSweepTargetScope(ctx, s.db, request.TenantID, targetIDs); err != nil {
		return ScheduledEstimateSweepResult{}, err
	}

	result := ScheduledEstimateSweepResult{}
	var failures []error
	var cursorWorkerID uuid.UUID
	var cursorIncarnation int64
	for {
		facts := make([]persistence.WorkerIncarnationFact, 0, estimateSweepPageSize)
		query := s.db.WithContext(ctx).
			Select("worker_id", "worker_incarnation").
			Where("tenant_id = ? AND execution_target_id IN ?", request.TenantID, targetIDs).
			Where("registered_at < ?", periodEnd).
			Where("terminated_at IS NULL OR terminated_at > ?", periodStart)
		if cursorWorkerID != uuid.Nil {
			query = query.Where(
				"worker_id > ? OR (worker_id = ? AND worker_incarnation > ?)",
				cursorWorkerID, cursorWorkerID, cursorIncarnation,
			)
		}
		if err := query.Order("worker_id, worker_incarnation").Limit(estimateSweepPageSize).Find(&facts).Error; err != nil {
			return result, problem.Wrap(
				500,
				"cost_accounting_estimate_sweep_facts_load_failed",
				"The cost estimate sweep could not load Worker incarnation facts.",
				err,
			)
		}
		for _, fact := range facts {
			result.WorkerCount++
			rows, estimateErr := s.EstimateUsageCharges(ctx, EstimateUsageChargesInput{
				Provider: provider, CurrencyCode: currency,
				WorkerID: fact.WorkerID, WorkerIncarnation: fact.WorkerIncarnation,
				BillingPeriodStartAt: periodStart, BillingPeriodEndAt: periodEnd,
			})
			result.EstimateCount += len(rows)
			if estimateErr != nil {
				result.FailedWorkerCount++
				failures = append(failures, fmt.Errorf(
					"worker %s incarnation %d: %w",
					fact.WorkerID, fact.WorkerIncarnation, estimateErr,
				))
			}
		}
		if len(facts) < estimateSweepPageSize {
			break
		}
		cursorWorkerID = facts[len(facts)-1].WorkerID
		cursorIncarnation = facts[len(facts)-1].WorkerIncarnation
	}
	if len(failures) == 0 {
		return result, nil
	}
	status := 409
	for _, failure := range failures {
		var apiError *problem.Error
		if !errors.As(failure, &apiError) || apiError.Status >= 500 {
			status = 500
			break
		}
	}
	err = problem.Wrap(
		status,
		"cost_accounting_estimate_sweep_partial_failure",
		"One or more Worker usage estimates could not be derived; reconciliation must wait for a successful retry.",
		errors.Join(failures...),
	)
	if apiError, ok := err.(*problem.Error); ok {
		apiError.Details = map[string]any{
			"workerCount":       result.WorkerCount,
			"estimateCount":     result.EstimateCount,
			"failedWorkerCount": result.FailedWorkerCount,
		}
	}
	return result, err
}

func normalizeEstimateSweepRequest(
	request ScheduledEstimateSweepRequest,
) (string, string, time.Time, time.Time, []uuid.UUID, error) {
	if request.TenantID == uuid.Nil || request.Import.TenantID == uuid.Nil || request.Import.TenantID != request.TenantID {
		return "", "", time.Time{}, time.Time{}, nil, problem.New(
			409,
			"cost_accounting_estimate_sweep_import_scope_mismatch",
			"The cost estimate sweep import does not belong to the configured Tenant.",
		)
	}
	provider, err := normalizeProvider(request.Provider)
	if err != nil {
		return "", "", time.Time{}, time.Time{}, nil, err
	}
	importProvider, err := normalizeProvider(request.Import.Provider)
	if err != nil || importProvider != provider {
		return "", "", time.Time{}, time.Time{}, nil, problem.New(
			409,
			"cost_accounting_estimate_sweep_import_provider_mismatch",
			"The cost estimate sweep import does not match the configured provider.",
		)
	}
	currency, err := normalizeCurrency(request.Import.CurrencyCode)
	if err != nil {
		return "", "", time.Time{}, time.Time{}, nil, err
	}
	periodStart, periodEnd, err := normalizeClosedPeriod(
		request.Import.BillingPeriodStartAt,
		request.Import.BillingPeriodEndAt,
	)
	if err != nil {
		return "", "", time.Time{}, time.Time{}, nil, err
	}
	targetIDs := make([]uuid.UUID, 0, len(request.ExecutionTargetIDs))
	seen := make(map[uuid.UUID]struct{}, len(request.ExecutionTargetIDs))
	for _, targetID := range request.ExecutionTargetIDs {
		if targetID == uuid.Nil {
			return "", "", time.Time{}, time.Time{}, nil, problem.New(
				400,
				"cost_accounting_estimate_sweep_target_required",
				"Every cost estimate sweep executionTargetId must be a UUID.",
			)
		}
		if _, duplicate := seen[targetID]; duplicate {
			return "", "", time.Time{}, time.Time{}, nil, problem.New(
				400,
				"cost_accounting_estimate_sweep_target_duplicated",
				"The cost estimate sweep repeats an executionTargetId.",
			)
		}
		seen[targetID] = struct{}{}
		targetIDs = append(targetIDs, targetID)
	}
	if len(targetIDs) == 0 {
		return "", "", time.Time{}, time.Time{}, nil, problem.New(
			400,
			"cost_accounting_estimate_sweep_targets_required",
			"The cost estimate sweep requires explicit Tenant-owned executionTargetIds.",
		)
	}
	return provider, currency, periodStart, periodEnd, targetIDs, nil
}

func validateEstimateSweepTargetScope(
	ctx context.Context,
	db *gorm.DB,
	tenantID uuid.UUID,
	targetIDs []uuid.UUID,
) error {
	var targets []persistence.ExecutionTarget
	if err := db.WithContext(ctx).
		Select("id", "tenant_id").
		Where("id IN ?", targetIDs).
		Find(&targets).Error; err != nil {
		return problem.Wrap(
			500,
			"cost_accounting_estimate_sweep_targets_load_failed",
			"The cost estimate sweep could not load its configured Execution Targets.",
			err,
		)
	}
	if len(targets) != len(targetIDs) {
		return problem.New(
			409,
			"cost_accounting_estimate_sweep_target_scope_mismatch",
			"A configured cost estimate Execution Target is missing or outside the Tenant boundary.",
		)
	}
	for _, target := range targets {
		if target.TenantID == nil || *target.TenantID != tenantID {
			return problem.New(
				409,
				"cost_accounting_estimate_sweep_target_scope_mismatch",
				"A configured cost estimate Execution Target is shared or belongs to another Tenant.",
			)
		}
	}
	return nil
}

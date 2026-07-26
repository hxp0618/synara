package billing

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	sharedAllocationSweepPageSize       = 200
	sharedAllocationSweepFailureLimit   = 20
	sharedAllocationSweepStatusComplete = "completed"
	sharedAllocationSweepStatusRetry    = "retry-required"
)

var (
	sharedLedgerWriterVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$`)
	sharedLedgerAttestationPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type SealSharedTargetLedgerCoverageInput struct {
	CompleteFromAt              time.Time `json:"completeFromAt"`
	MinimumWriterVersion        string    `json:"minimumWriterVersion"`
	DeploymentAttestationSHA256 string    `json:"deploymentAttestationSHA256"`
}

type SharedTargetLedgerCoverage struct {
	ID                          uuid.UUID `json:"id"`
	ExecutionTargetID           uuid.UUID `json:"executionTargetId"`
	CompleteFromAt              time.Time `json:"completeFromAt"`
	MinimumWriterVersion        string    `json:"minimumWriterVersion"`
	DeploymentAttestationSHA256 string    `json:"deploymentAttestationSHA256"`
	SealedAt                    time.Time `json:"sealedAt"`
	SealedBy                    uuid.UUID `json:"sealedBy"`
}

type SweepSharedUsageChargesInput struct {
	ExecutionTargetID    uuid.UUID `json:"-"`
	Provider             string    `json:"provider"`
	CurrencyCode         string    `json:"currencyCode"`
	BillingPeriodStartAt time.Time `json:"billingPeriodStartAt"`
	BillingPeriodEndAt   time.Time `json:"billingPeriodEndAt"`
}

type SharedUsageAllocationSweepFailure struct {
	WorkerID          uuid.UUID `json:"workerId"`
	WorkerIncarnation int64     `json:"workerIncarnation"`
	ErrorCode         string    `json:"errorCode"`
	ErrorMessage      string    `json:"errorMessage"`
}

type SharedUsageAllocationSweepResult struct {
	Status               string                              `json:"status"`
	WorkerCount          int                                 `json:"workerCount"`
	AllocationRunCount   int                                 `json:"allocationRunCount"`
	AllocationSliceCount int                                 `json:"allocationSliceCount"`
	FailedWorkerCount    int                                 `json:"failedWorkerCount"`
	FailuresOmitted      int                                 `json:"failuresOmitted"`
	Failures             []SharedUsageAllocationSweepFailure `json:"failures,omitempty"`
}

type normalizedSharedUsageSweep struct {
	ExecutionTargetID uuid.UUID
	Provider          string
	CurrencyCode      string
	PeriodStart       time.Time
	PeriodEnd         time.Time
}

func (s *Service) GetSharedTargetLedgerCoverageAuthorized(
	ctx context.Context,
	principal identity.Principal,
	operatorTenantID uuid.UUID,
	executionTargetID uuid.UUID,
) (SharedTargetLedgerCoverage, error) {
	if err := s.requireSharedBillingOperator(ctx, principal, operatorTenantID); err != nil {
		return SharedTargetLedgerCoverage{}, err
	}
	if executionTargetID == uuid.Nil {
		return SharedTargetLedgerCoverage{}, problem.New(
			400,
			"billing_shared_target_required",
			"executionTargetId is required.",
		)
	}
	coverage, found, err := findSharedTargetLedgerCoverage(ctx, s.db, executionTargetID)
	if err != nil {
		return SharedTargetLedgerCoverage{}, err
	}
	if !found {
		return SharedTargetLedgerCoverage{}, problem.New(
			404,
			"billing_shared_ledger_coverage_not_found",
			"The shared Target ledger coverage was not found.",
		)
	}
	return viewSharedTargetLedgerCoverage(coverage), nil
}

// SealSharedTargetLedgerCoverageAuthorized creates the one immutable cutover
// authority for a platform-shared Target. An exact retry returns the existing
// row without writing another audit entry; any changed assertion conflicts.
func (s *Service) SealSharedTargetLedgerCoverageAuthorized(
	ctx context.Context,
	principal identity.Principal,
	operatorTenantID uuid.UUID,
	executionTargetID uuid.UUID,
	input SealSharedTargetLedgerCoverageInput,
	requestID, ipAddress string,
) (SharedTargetLedgerCoverage, bool, error) {
	if err := s.requireSharedBillingOperator(ctx, principal, operatorTenantID); err != nil {
		return SharedTargetLedgerCoverage{}, false, err
	}
	if executionTargetID == uuid.Nil {
		return SharedTargetLedgerCoverage{}, false, problem.New(
			400,
			"billing_shared_target_required",
			"executionTargetId is required.",
		)
	}
	sealedAt := s.now().UTC()
	normalized, err := normalizeSharedTargetLedgerCoverageInput(input, sealedAt)
	if err != nil {
		return SharedTargetLedgerCoverage{}, false, err
	}

	if s.db.Dialector.Name() != "postgres" {
		s.sharedAllocationMu.Lock()
		defer s.sharedAllocationMu.Unlock()
	}

	created := false
	coverage := persistence.BillingSharedTargetLedgerCoverage{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if lockErr := acquireSharedTargetCoverageLock(ctx, tx, executionTargetID); lockErr != nil {
			return problem.Wrap(
				500,
				"billing_shared_ledger_coverage_lock_failed",
				"The shared Target ledger coverage authority could not be locked.",
				lockErr,
			)
		}
		if targetErr := requirePlatformSharedTarget(ctx, tx, executionTargetID); targetErr != nil {
			return targetErr
		}

		existing, found, loadErr := findSharedTargetLedgerCoverage(ctx, tx, executionTargetID)
		if loadErr != nil {
			return loadErr
		}
		if found {
			if !sameSharedTargetLedgerCoverageAssertion(existing, normalized) {
				return problem.New(
					409,
					"billing_shared_ledger_coverage_conflict",
					"The shared Target already has a different immutable ledger coverage assertion.",
				)
			}
			coverage = existing
			return nil
		}

		coverage = persistence.BillingSharedTargetLedgerCoverage{
			ID:                          deterministicSharedTargetLedgerCoverageID(executionTargetID),
			ExecutionTargetID:           executionTargetID,
			CompleteFromAt:              normalized.CompleteFromAt,
			MinimumWriterVersion:        normalized.MinimumWriterVersion,
			DeploymentAttestationSHA256: normalized.DeploymentAttestationSHA256,
			SealedAt:                    sealedAt,
			SealedBy:                    principal.UserID,
		}
		if createErr := tx.WithContext(ctx).Omit(clause.Associations).Create(&coverage).Error; createErr != nil {
			return problem.Wrap(
				409,
				"billing_shared_ledger_coverage_create_failed",
				"The shared Target ledger coverage assertion could not be sealed.",
				createErr,
			)
		}
		if auditErr := s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID: operatorTenantID, ActorType: "user", ActorID: &principal.UserID,
			Action:       "billing.shared_target_ledger_coverage_sealed",
			ResourceType: "billing_shared_target_ledger_coverage", ResourceID: &coverage.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"executionTargetId":           executionTargetID,
				"completeFromAt":              normalized.CompleteFromAt,
				"minimumWriterVersion":        normalized.MinimumWriterVersion,
				"deploymentAttestationSHA256": normalized.DeploymentAttestationSHA256,
			},
		}); auditErr != nil {
			return auditErr
		}
		created = true
		return nil
	})
	if err != nil {
		return SharedTargetLedgerCoverage{}, false, err
	}
	return viewSharedTargetLedgerCoverage(coverage), created, nil
}

func (s *Service) SweepSharedUsageChargesAuthorized(
	ctx context.Context,
	principal identity.Principal,
	operatorTenantID uuid.UUID,
	input SweepSharedUsageChargesInput,
	requestID, ipAddress string,
) (SharedUsageAllocationSweepResult, error) {
	if err := s.requireSharedBillingOperator(ctx, principal, operatorTenantID); err != nil {
		return SharedUsageAllocationSweepResult{}, err
	}
	normalized, err := s.normalizeSharedUsageSweep(ctx, input)
	if err != nil {
		return SharedUsageAllocationSweepResult{}, err
	}
	if err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		return s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID: operatorTenantID, ActorType: "user", ActorID: &principal.UserID,
			Action:       "billing.shared_cost_allocation_sweep_requested",
			ResourceType: "execution_target", ResourceID: &normalized.ExecutionTargetID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"provider": normalized.Provider, "currencyCode": normalized.CurrencyCode,
				"billingPeriodStartAt": normalized.PeriodStart, "billingPeriodEndAt": normalized.PeriodEnd,
			},
		})
	}); err != nil {
		return SharedUsageAllocationSweepResult{}, err
	}
	return s.sweepSharedUsageCharges(ctx, normalized)
}

// SweepSharedUsageCharges scans every shared Worker incarnation overlapping a
// closed period. Each allocation commits independently and has deterministic
// identity, so a crash or partial failure is repaired by replaying the same
// request without duplicating or rewriting successful accounting history.
func (s *Service) SweepSharedUsageCharges(
	ctx context.Context,
	input SweepSharedUsageChargesInput,
) (SharedUsageAllocationSweepResult, error) {
	normalized, err := s.normalizeSharedUsageSweep(ctx, input)
	if err != nil {
		return SharedUsageAllocationSweepResult{}, err
	}
	return s.sweepSharedUsageCharges(ctx, normalized)
}

func (s *Service) sweepSharedUsageCharges(
	ctx context.Context,
	normalized normalizedSharedUsageSweep,
) (SharedUsageAllocationSweepResult, error) {
	result := SharedUsageAllocationSweepResult{Status: sharedAllocationSweepStatusComplete}
	var cursorWorkerID uuid.UUID
	var cursorIncarnation int64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		facts := make([]persistence.WorkerIncarnationFact, 0, sharedAllocationSweepPageSize)
		query := s.db.WithContext(ctx).
			Select("worker_id", "worker_incarnation").
			Where("execution_target_id = ? AND tenant_id IS NULL", normalized.ExecutionTargetID).
			Where("registered_at < ?", normalized.PeriodEnd).
			Where("terminated_at IS NULL OR terminated_at > ?", normalized.PeriodStart)
		if cursorWorkerID != uuid.Nil {
			query = query.Where(
				"worker_id > ? OR (worker_id = ? AND worker_incarnation > ?)",
				cursorWorkerID, cursorWorkerID, cursorIncarnation,
			)
		}
		if err := query.Order("worker_id, worker_incarnation").Limit(sharedAllocationSweepPageSize).Find(&facts).Error; err != nil {
			return result, problem.Wrap(
				500,
				"billing_shared_allocation_sweep_facts_load_failed",
				"The shared billing allocation sweep could not load Worker incarnation facts.",
				err,
			)
		}
		for _, fact := range facts {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.WorkerCount++
			allocation, allocationErr := s.AllocateSharedUsageCharges(ctx, AllocateSharedUsageChargesInput{
				Provider: normalized.Provider, CurrencyCode: normalized.CurrencyCode,
				WorkerID: fact.WorkerID, WorkerIncarnation: fact.WorkerIncarnation,
				BillingPeriodStartAt: normalized.PeriodStart, BillingPeriodEndAt: normalized.PeriodEnd,
			})
			if allocationErr != nil {
				result.Status = sharedAllocationSweepStatusRetry
				result.FailedWorkerCount++
				failure := sharedUsageAllocationSweepFailure(fact, allocationErr)
				if len(result.Failures) < sharedAllocationSweepFailureLimit {
					result.Failures = append(result.Failures, failure)
				} else {
					result.FailuresOmitted++
				}
				continue
			}
			if allocation.Run != nil {
				result.AllocationRunCount++
				result.AllocationSliceCount += len(allocation.Slices)
			}
		}
		if len(facts) < sharedAllocationSweepPageSize {
			break
		}
		cursorWorkerID = facts[len(facts)-1].WorkerID
		cursorIncarnation = facts[len(facts)-1].WorkerIncarnation
	}
	return result, nil
}

func (s *Service) normalizeSharedUsageSweep(
	ctx context.Context,
	input SweepSharedUsageChargesInput,
) (normalizedSharedUsageSweep, error) {
	if s == nil || s.db == nil {
		return normalizedSharedUsageSweep{}, problem.New(
			500,
			"billing_shared_allocation_sweeper_unavailable",
			"The shared billing allocation sweeper is not initialized.",
		)
	}
	if input.ExecutionTargetID == uuid.Nil {
		return normalizedSharedUsageSweep{}, problem.New(
			400,
			"billing_shared_target_required",
			"executionTargetId is required.",
		)
	}
	provider, err := normalizeProvider(input.Provider)
	if err != nil {
		return normalizedSharedUsageSweep{}, err
	}
	currency, err := normalizeCurrency(input.CurrencyCode)
	if err != nil {
		return normalizedSharedUsageSweep{}, err
	}
	periodStart, periodEnd, err := normalizeClosedPeriod(input.BillingPeriodStartAt, input.BillingPeriodEndAt)
	if err != nil {
		return normalizedSharedUsageSweep{}, err
	}
	if periodEnd.After(s.now().UTC()) {
		return normalizedSharedUsageSweep{}, problem.New(
			409,
			"billing_shared_allocation_period_open",
			"Shared cost allocation requires a billing period that has already closed.",
		)
	}
	if err := requirePlatformSharedTarget(ctx, s.db, input.ExecutionTargetID); err != nil {
		return normalizedSharedUsageSweep{}, err
	}
	if _, err := getSharedTargetLedgerCoverage(ctx, s.db, input.ExecutionTargetID); err != nil {
		return normalizedSharedUsageSweep{}, err
	}
	return normalizedSharedUsageSweep{
		ExecutionTargetID: input.ExecutionTargetID,
		Provider:          provider, CurrencyCode: currency,
		PeriodStart: periodStart, PeriodEnd: periodEnd,
	}, nil
}

func (s *Service) requireSharedBillingOperator(
	ctx context.Context,
	principal identity.Principal,
	operatorTenantID uuid.UUID,
) error {
	if err := requireBillingTenant(principal, operatorTenantID); err != nil {
		return err
	}
	if _, err := s.authorizer.RequireTenant(
		ctx,
		principal.UserID,
		operatorTenantID,
		authorization.BillingManage,
	); err != nil {
		return err
	}
	if s.platformBillingOperatorTenantID == uuid.Nil {
		return problem.New(
			503,
			"billing_shared_management_unavailable",
			"Shared billing management requires an explicitly configured platform billing operator Tenant.",
		)
	}
	if operatorTenantID != s.platformBillingOperatorTenantID {
		return problem.New(
			403,
			"billing_shared_operator_forbidden",
			"This Tenant is not the configured platform billing operator.",
		)
	}
	return nil
}

func normalizeSharedTargetLedgerCoverageInput(
	input SealSharedTargetLedgerCoverageInput,
	sealedAt time.Time,
) (SealSharedTargetLedgerCoverageInput, error) {
	input.CompleteFromAt = input.CompleteFromAt.UTC()
	input.MinimumWriterVersion = strings.TrimSpace(input.MinimumWriterVersion)
	input.DeploymentAttestationSHA256 = strings.TrimSpace(input.DeploymentAttestationSHA256)
	if input.CompleteFromAt.IsZero() {
		return SealSharedTargetLedgerCoverageInput{}, problem.New(
			400,
			"billing_shared_ledger_coverage_time_required",
			"completeFromAt is required.",
		)
	}
	if input.CompleteFromAt.After(sealedAt) {
		return SealSharedTargetLedgerCoverageInput{}, problem.New(
			409,
			"billing_shared_ledger_coverage_in_future",
			"completeFromAt cannot be later than the sealing authority timestamp.",
		)
	}
	if !sharedLedgerWriterVersionPattern.MatchString(input.MinimumWriterVersion) {
		return SealSharedTargetLedgerCoverageInput{}, problem.New(
			400,
			"billing_shared_ledger_writer_version_invalid",
			"minimumWriterVersion must use 1 to 80 version-safe characters.",
		)
	}
	if !sharedLedgerAttestationPattern.MatchString(input.DeploymentAttestationSHA256) {
		return SealSharedTargetLedgerCoverageInput{}, problem.New(
			400,
			"billing_shared_ledger_attestation_invalid",
			"deploymentAttestationSHA256 must be a lowercase SHA-256 digest.",
		)
	}
	return input, nil
}

func acquireSharedTargetCoverageLock(ctx context.Context, tx *gorm.DB, targetID uuid.UUID) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.WithContext(ctx).
		Exec(
			"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
			"synara:billing-shared-target-ledger-coverage:"+targetID.String(),
		).
		Error
}

func requirePlatformSharedTarget(ctx context.Context, db *gorm.DB, targetID uuid.UUID) error {
	var target persistence.ExecutionTarget
	err := persistence.WithLocking(db.WithContext(ctx), "UPDATE", "").
		Select("id", "tenant_id").
		Where("id = ?", targetID).
		First(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(404, "billing_shared_target_not_found", "The shared execution Target was not found.")
	}
	if err != nil {
		return problem.Wrap(500, "billing_shared_target_load_failed", "The shared execution Target could not be loaded.", err)
	}
	if target.TenantID != nil {
		return problem.New(
			409,
			"billing_shared_target_scope_invalid",
			"Shared billing management requires a platform-shared execution Target.",
		)
	}
	return nil
}

func findSharedTargetLedgerCoverage(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
) (persistence.BillingSharedTargetLedgerCoverage, bool, error) {
	var coverage persistence.BillingSharedTargetLedgerCoverage
	err := db.WithContext(ctx).Where("execution_target_id = ?", targetID).First(&coverage).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.BillingSharedTargetLedgerCoverage{}, false, nil
	}
	if err != nil {
		return persistence.BillingSharedTargetLedgerCoverage{}, false, problem.Wrap(
			500,
			"billing_shared_ledger_coverage_load_failed",
			"The shared Target ledger coverage could not be loaded.",
			err,
		)
	}
	return coverage, true, nil
}

func getSharedTargetLedgerCoverage(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
) (persistence.BillingSharedTargetLedgerCoverage, error) {
	coverage, found, err := findSharedTargetLedgerCoverage(ctx, db, targetID)
	if err != nil {
		return persistence.BillingSharedTargetLedgerCoverage{}, err
	}
	if !found {
		return persistence.BillingSharedTargetLedgerCoverage{}, problem.New(
			409,
			"billing_shared_ledger_coverage_missing",
			"The shared Target has no sealed complete claim/release ledger coverage.",
		)
	}
	return coverage, nil
}

func sameSharedTargetLedgerCoverageAssertion(
	coverage persistence.BillingSharedTargetLedgerCoverage,
	input SealSharedTargetLedgerCoverageInput,
) bool {
	return coverage.CompleteFromAt.UTC().Equal(input.CompleteFromAt.UTC()) &&
		coverage.MinimumWriterVersion == input.MinimumWriterVersion &&
		coverage.DeploymentAttestationSHA256 == input.DeploymentAttestationSHA256
}

func deterministicSharedTargetLedgerCoverageID(targetID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(
		deterministicNamespace,
		[]byte("shared-target-ledger-coverage\x00"+targetID.String()),
	)
}

func viewSharedTargetLedgerCoverage(
	coverage persistence.BillingSharedTargetLedgerCoverage,
) SharedTargetLedgerCoverage {
	return SharedTargetLedgerCoverage{
		ID: coverage.ID, ExecutionTargetID: coverage.ExecutionTargetID,
		CompleteFromAt:              coverage.CompleteFromAt.UTC(),
		MinimumWriterVersion:        coverage.MinimumWriterVersion,
		DeploymentAttestationSHA256: coverage.DeploymentAttestationSHA256,
		SealedAt:                    coverage.SealedAt.UTC(), SealedBy: coverage.SealedBy,
	}
}

func sharedUsageAllocationSweepFailure(
	fact persistence.WorkerIncarnationFact,
	err error,
) SharedUsageAllocationSweepFailure {
	failure := SharedUsageAllocationSweepFailure{
		WorkerID: fact.WorkerID, WorkerIncarnation: fact.WorkerIncarnation,
		ErrorCode:    "billing_shared_allocation_failed",
		ErrorMessage: "The Worker shared-cost allocation failed and must be retried.",
	}
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		failure.ErrorCode = apiError.Code
		failure.ErrorMessage = apiError.Message
	}
	return failure
}

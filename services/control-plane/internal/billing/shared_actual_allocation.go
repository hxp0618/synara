package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	SharedActualAllocationAlgorithmProportionalEstimateV1 = "proportional-shared-estimate-v1"
	SharedActualAllocationStateBuilding                   = "building"
	SharedActualAllocationStateSealed                     = "sealed"
	sharedActualAllocationMaxLines                        = 100_000
	sharedActualAllocationMaxSlices                       = 1_000_000
	sharedActualAllocationInsertBatchSize                 = 500
)

type AllocateSharedActualInvoiceInput struct {
	ExecutionTargetID            uuid.UUID `json:"-"`
	InvoiceImportID              uuid.UUID `json:"-"`
	SourceScopeAttestationSHA256 string    `json:"sourceScopeAttestationSHA256"`
}

type SharedActualInvoiceAllocationRun struct {
	ID                           uuid.UUID  `json:"id"`
	OperatorTenantID             uuid.UUID  `json:"operatorTenantId"`
	InvoiceImportID              uuid.UUID  `json:"invoiceImportId"`
	ExecutionTargetID            uuid.UUID  `json:"executionTargetId"`
	LedgerCoverageID             uuid.UUID  `json:"ledgerCoverageId"`
	Provider                     string     `json:"provider"`
	CurrencyCode                 string     `json:"currencyCode"`
	BillingPeriodStartAt         time.Time  `json:"billingPeriodStartAt"`
	BillingPeriodEndAt           time.Time  `json:"billingPeriodEndAt"`
	AlgorithmVersion             string     `json:"algorithmVersion"`
	SourceChecksum               string     `json:"sourceChecksum"`
	SourceScopeAttestationSHA256 string     `json:"sourceScopeAttestationSHA256"`
	SourceLineSetSHA256          string     `json:"sourceLineSetSHA256"`
	ImportLineCount              int64      `json:"importLineCount"`
	ImportAmountMicros           int64      `json:"importAmountMicros"`
	SourceLineCount              int64      `json:"sourceLineCount"`
	SourceAmountMicros           int64      `json:"sourceAmountMicros"`
	UnallocatedLineCount         int64      `json:"unallocatedLineCount"`
	UnallocatedAmountMicros      int64      `json:"unallocatedAmountMicros"`
	AllocationLineCount          int64      `json:"allocationLineCount"`
	AllocationSliceCount         int64      `json:"allocationSliceCount"`
	AllocatedAmountMicros        int64      `json:"allocatedAmountMicros"`
	State                        string     `json:"state"`
	CreatedBy                    uuid.UUID  `json:"createdBy"`
	CreatedAt                    time.Time  `json:"createdAt"`
	SealedAt                     *time.Time `json:"sealedAt"`
}

type SharedActualInvoiceAllocationResult struct {
	Run     SharedActualInvoiceAllocationRun `json:"run"`
	Created bool                             `json:"created"`
}

type sharedActualMatchRow struct {
	ActualInvoiceLineID   uuid.UUID  `gorm:"column:actual_invoice_line_id"`
	ExternalLineID        string     `gorm:"column:external_line_id"`
	SourceAmountMicros    int64      `gorm:"column:source_amount_micros"`
	EstimatedSliceID      uuid.UUID  `gorm:"column:estimated_slice_id"`
	EstimatedTenantID     *uuid.UUID `gorm:"column:estimated_tenant_id"`
	EstimatedAllocation   string     `gorm:"column:estimated_allocation_kind"`
	EstimatedAmountMicros int64      `gorm:"column:estimated_amount_micros"`
}

type sharedActualImportAggregate struct {
	LineCount  int64  `gorm:"column:line_count"`
	AmountText string `gorm:"column:amount_text"`
}

type sharedActualPlannedLine struct {
	ActualInvoiceLineID uuid.UUID
	ExternalLineID      string
	SourceAmountMicros  int64
	Matches             []sharedActualMatchRow
}

type sharedActualAllocationPlan struct {
	Run    persistence.BillingSharedActualAllocationRun
	Lines  []persistence.BillingSharedActualAllocationLine
	Slices []persistence.BillingSharedActualChargeSlice
}

func (s *Service) AllocateSharedActualInvoiceAuthorized(
	ctx context.Context,
	principal identity.Principal,
	operatorTenantID uuid.UUID,
	input AllocateSharedActualInvoiceInput,
	requestID, ipAddress string,
) (SharedActualInvoiceAllocationResult, error) {
	if err := s.requireSharedBillingOperator(ctx, principal, operatorTenantID); err != nil {
		return SharedActualInvoiceAllocationResult{}, err
	}
	if input.ExecutionTargetID == uuid.Nil {
		return SharedActualInvoiceAllocationResult{}, problem.New(
			400,
			"cost_accounting_shared_target_required",
			"executionTargetId is required.",
		)
	}
	if input.InvoiceImportID == uuid.Nil {
		return SharedActualInvoiceAllocationResult{}, problem.New(
			400,
			"cost_accounting_shared_actual_invoice_required",
			"invoiceImportId is required.",
		)
	}
	input.SourceScopeAttestationSHA256 = strings.TrimSpace(input.SourceScopeAttestationSHA256)
	if !sharedLedgerAttestationPattern.MatchString(input.SourceScopeAttestationSHA256) {
		return SharedActualInvoiceAllocationResult{}, problem.New(
			400,
			"cost_accounting_shared_actual_source_scope_attestation_invalid",
			"sourceScopeAttestationSHA256 must be a lowercase SHA-256 digest.",
		)
	}

	if s.db.Dialector.Name() != "postgres" {
		s.sharedAllocationMu.Lock()
		defer s.sharedAllocationMu.Unlock()
	}

	result := SharedActualInvoiceAllocationResult{}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := acquireSharedActualInvoiceLock(ctx, tx, operatorTenantID, input.InvoiceImportID); err != nil {
			return problem.Wrap(
				500,
				"cost_accounting_shared_actual_allocation_lock_failed",
				"The shared actual-invoice allocation identity could not be locked.",
				err,
			)
		}

		invoiceImport, err := loadSharedActualInvoiceImport(ctx, tx, operatorTenantID, input.InvoiceImportID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if invoiceImport.BillingPeriodEndAt.UTC().After(now) {
			return problem.New(
				409,
				"cost_accounting_shared_actual_invoice_period_open",
				"Shared actual-invoice allocation requires a billing period that has already closed.",
			)
		}
		if err := acquireSharedEstimatePeriodSnapshotLock(
			ctx,
			tx,
			invoiceImport.Provider,
			invoiceImport.CurrencyCode,
			invoiceImport.BillingPeriodStartAt.UTC(),
			invoiceImport.BillingPeriodEndAt.UTC(),
		); err != nil {
			return problem.Wrap(
				500,
				"cost_accounting_shared_actual_snapshot_lock_failed",
				"The shared estimate snapshot could not be locked.",
				err,
			)
		}
		if err := requirePlatformSharedTarget(ctx, tx, input.ExecutionTargetID); err != nil {
			return err
		}
		coverage, err := getSharedTargetLedgerCoverage(ctx, tx, input.ExecutionTargetID)
		if err != nil {
			return err
		}

		plan, err := buildSharedActualAllocationPlan(
			ctx,
			tx,
			operatorTenantID,
			principal.UserID,
			input.ExecutionTargetID,
			invoiceImport,
			coverage,
			input.SourceScopeAttestationSHA256,
			now,
		)
		if err != nil {
			return err
		}

		existingRun, found, err := loadSharedActualAllocationRun(ctx, tx, plan.Run.ID)
		if err != nil {
			return err
		}
		if found {
			existingLines, existingSlices, err := loadSharedActualAllocationGraph(ctx, tx, existingRun.ID)
			if err != nil {
				return err
			}
			if !sameSharedActualAllocationRun(existingRun, plan.Run) ||
				!sameSharedActualAllocationLines(existingLines, plan.Lines) ||
				!sameSharedActualChargeSlices(existingSlices, plan.Slices) {
				return problem.New(
					409,
					"cost_accounting_shared_actual_allocation_conflict",
					"The existing shared actual-invoice allocation does not match the sealed invoice and estimate evidence.",
				)
			}
			result = SharedActualInvoiceAllocationResult{
				Run: viewSharedActualAllocationRun(existingRun),
			}
			return nil
		}

		if err := rejectOccupiedSharedActualAuthorities(ctx, tx, plan); err != nil {
			return err
		}
		buildingRun := plan.Run
		buildingRun.State = SharedActualAllocationStateBuilding
		buildingRun.SealedAt = nil
		if err := tx.WithContext(ctx).Omit(clause.Associations).Create(&buildingRun).Error; err != nil {
			return problem.Wrap(
				409,
				"cost_accounting_shared_actual_allocation_create_failed",
				"The shared actual-invoice allocation run could not be created.",
				err,
			)
		}
		if err := tx.WithContext(ctx).Omit(clause.Associations).
			CreateInBatches(&plan.Lines, sharedActualAllocationInsertBatchSize).Error; err != nil {
			return problem.Wrap(
				409,
				"cost_accounting_shared_actual_allocation_line_create_failed",
				"The shared actual-invoice allocation lines could not be created.",
				err,
			)
		}
		if err := tx.WithContext(ctx).Omit(clause.Associations).
			CreateInBatches(&plan.Slices, sharedActualAllocationInsertBatchSize).Error; err != nil {
			return problem.Wrap(
				409,
				"cost_accounting_shared_actual_allocation_slice_create_failed",
				"The shared actual-invoice allocation slices could not be created.",
				err,
			)
		}
		seal := tx.WithContext(ctx).
			Model(&persistence.BillingSharedActualAllocationRun{}).
			Where("id = ? AND state = ?", plan.Run.ID, SharedActualAllocationStateBuilding).
			Updates(map[string]any{
				"state":     SharedActualAllocationStateSealed,
				"sealed_at": now,
			})
		if seal.Error != nil {
			return problem.Wrap(
				409,
				"cost_accounting_shared_actual_allocation_seal_failed",
				"The shared actual-invoice allocation did not pass conservation sealing.",
				seal.Error,
			)
		}
		if seal.RowsAffected != 1 {
			return problem.New(
				409,
				"cost_accounting_shared_actual_allocation_seal_conflict",
				"The shared actual-invoice allocation could not be sealed from its expected state.",
			)
		}
		if err := s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID:     operatorTenantID,
			ActorType:    "user",
			ActorID:      &principal.UserID,
			Action:       "cost_accounting.shared_actual_invoice_allocated",
			ResourceType: "cost_accounting_shared_actual_allocation_run",
			ResourceID:   &plan.Run.ID,
			RequestID:    requestID,
			IPAddress:    ipAddress,
			Metadata: map[string]any{
				"executionTargetId":            plan.Run.ExecutionTargetID,
				"invoiceImportId":              plan.Run.InvoiceImportID,
				"provider":                     plan.Run.Provider,
				"currencyCode":                 plan.Run.CurrencyCode,
				"billingPeriodStartAt":         plan.Run.BillingPeriodStartAt,
				"billingPeriodEndAt":           plan.Run.BillingPeriodEndAt,
				"sourceScopeAttestationSHA256": plan.Run.SourceScopeAttestationSHA256,
				"sourceLineSetSHA256":          plan.Run.SourceLineSetSHA256,
				"sourceLineCount":              plan.Run.SourceLineCount,
				"sourceAmountMicros":           plan.Run.SourceAmountMicros,
				"allocationSliceCount":         plan.Run.AllocationSliceCount,
				"unallocatedLineCount":         plan.Run.UnallocatedLineCount,
				"unallocatedAmountMicros":      plan.Run.UnallocatedAmountMicros,
			},
		}); err != nil {
			return err
		}
		result = SharedActualInvoiceAllocationResult{
			Run:     viewSharedActualAllocationRun(plan.Run),
			Created: true,
		}
		return nil
	})
	if err != nil {
		return SharedActualInvoiceAllocationResult{}, err
	}
	return result, nil
}

func acquireSharedActualInvoiceLock(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, invoiceImportID uuid.UUID,
) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.WithContext(ctx).Exec(
		"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
		strings.Join([]string{
			"synara:billing-shared-actual-invoice-allocation",
			operatorTenantID.String(),
			invoiceImportID.String(),
		}, "\x1f"),
	).Error
}

func acquireSharedEstimatePeriodSnapshotLock(
	ctx context.Context,
	tx *gorm.DB,
	provider, currency string,
	periodStart, periodEnd time.Time,
) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.WithContext(ctx).Exec(`
		SELECT pg_advisory_xact_lock(hashtextextended(concat_ws(
			chr(31),
			'synara:billing-shared-estimate-period-snapshot',
			CAST(? AS text),
			CAST(? AS text),
			((extract(epoch FROM CAST(? AS timestamptz)) * 1000000)::bigint)::text,
			((extract(epoch FROM CAST(? AS timestamptz)) * 1000000)::bigint)::text
		), 0))
	`, provider, currency, periodStart.UTC(), periodEnd.UTC()).Error
}

func loadSharedActualInvoiceImport(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, importID uuid.UUID,
) (persistence.BillingActualInvoiceImport, error) {
	var invoiceImport persistence.BillingActualInvoiceImport
	err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND id = ?", operatorTenantID, importID).
		Take(&invoiceImport).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.BillingActualInvoiceImport{}, problem.New(
			404,
			"cost_accounting_shared_actual_invoice_not_found",
			"The operator-owned actual invoice import was not found.",
		)
	}
	if err != nil {
		return persistence.BillingActualInvoiceImport{}, problem.Wrap(
			500,
			"cost_accounting_shared_actual_invoice_load_failed",
			"The operator-owned actual invoice import could not be loaded.",
			err,
		)
	}
	return invoiceImport, nil
}

func buildSharedActualAllocationPlan(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, createdBy, executionTargetID uuid.UUID,
	invoiceImport persistence.BillingActualInvoiceImport,
	coverage persistence.BillingSharedTargetLedgerCoverage,
	sourceScopeAttestationSHA256 string,
	createdAt time.Time,
) (sharedActualAllocationPlan, error) {
	importAggregate, err := loadSharedActualImportAggregate(ctx, tx, operatorTenantID, invoiceImport.ID)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	if importAggregate.LineCount <= 0 {
		return sharedActualAllocationPlan{}, problem.New(
			409,
			"cost_accounting_shared_actual_invoice_empty",
			"The actual invoice import has no immutable lines to allocate.",
		)
	}
	importAmount, err := parseSharedActualAggregate(importAggregate.AmountText)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	if err := rejectAmbiguousSharedActualMatches(
		ctx,
		tx,
		operatorTenantID,
		invoiceImport.ID,
		executionTargetID,
	); err != nil {
		return sharedActualAllocationPlan{}, err
	}

	matchCount, err := countSharedActualMatches(
		ctx,
		tx,
		operatorTenantID,
		invoiceImport.ID,
		executionTargetID,
	)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	if matchCount <= 0 {
		return sharedActualAllocationPlan{}, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_basis_missing",
			"No shared estimated charge slices exactly match this invoice and Target.",
		)
	}
	if matchCount > sharedActualAllocationMaxSlices {
		return sharedActualAllocationPlan{}, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_too_large",
			"The shared actual-invoice allocation exceeds the bounded slice limit.",
		)
	}

	rows, err := loadSharedActualMatches(
		ctx,
		tx,
		operatorTenantID,
		invoiceImport.ID,
		executionTargetID,
		matchCount,
	)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	plannedLines, err := groupSharedActualMatches(rows)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	if len(plannedLines) > sharedActualAllocationMaxLines {
		return sharedActualAllocationPlan{}, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_too_large",
			"The shared actual-invoice allocation exceeds the bounded line limit.",
		)
	}

	runID := deterministicSharedActualAllocationRunID(
		operatorTenantID,
		invoiceImport.ID,
		executionTargetID,
	)
	lines := make([]persistence.BillingSharedActualAllocationLine, 0, len(plannedLines))
	slices := make([]persistence.BillingSharedActualChargeSlice, 0, len(rows))
	var sourceAmount int64
	for _, plannedLine := range plannedLines {
		lineID := deterministicSharedActualAllocationLineID(runID, plannedLine.ActualInvoiceLineID)
		weights := make([]int64, len(plannedLine.Matches))
		for index, match := range plannedLine.Matches {
			weights[index] = match.EstimatedAmountMicros
		}
		amounts, estimatedAmount, err := allocateSharedActualMicros(plannedLine.SourceAmountMicros, weights)
		if err != nil {
			return sharedActualAllocationPlan{}, err
		}
		sliceSetSHA256, err := sharedActualEstimatedSliceSetSHA256(plannedLine.Matches)
		if err != nil {
			return sharedActualAllocationPlan{}, err
		}
		lines = append(lines, persistence.BillingSharedActualAllocationLine{
			ID:                      lineID,
			RunID:                   runID,
			ActualInvoiceLineID:     plannedLine.ActualInvoiceLineID,
			SourceAmountMicros:      plannedLine.SourceAmountMicros,
			EstimatedAmountMicros:   estimatedAmount,
			AllocatedAmountMicros:   plannedLine.SourceAmountMicros,
			EstimatedSliceCount:     int64(len(plannedLine.Matches)),
			EstimatedSliceSetSHA256: sliceSetSHA256,
			CreatedAt:               createdAt,
		})
		for index, match := range plannedLine.Matches {
			slices = append(slices, persistence.BillingSharedActualChargeSlice{
				ID:                   deterministicSharedActualChargeSliceID(lineID, match.EstimatedSliceID),
				AllocationLineID:     lineID,
				EstimatedSliceID:     match.EstimatedSliceID,
				TenantID:             cloneOptionalUUID(match.EstimatedTenantID),
				AllocationKind:       match.EstimatedAllocation,
				EstimateWeightMicros: match.EstimatedAmountMicros,
				AmountMicros:         amounts[index],
				CreatedAt:            createdAt,
			})
		}
		sourceAmount, err = addInt64Checked(sourceAmount, plannedLine.SourceAmountMicros)
		if err != nil {
			return sharedActualAllocationPlan{}, problem.Wrap(
				409,
				"cost_accounting_shared_actual_allocation_amount_overflow",
				"The selected actual invoice line total exceeds the supported micros range.",
				err,
			)
		}
	}
	lineSetSHA256, err := sharedActualSourceLineSetSHA256(lines)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	unallocatedAmount, err := subtractSharedActualInt64(importAmount, sourceAmount)
	if err != nil {
		return sharedActualAllocationPlan{}, err
	}
	sealedAt := createdAt
	run := persistence.BillingSharedActualAllocationRun{
		ID:                           runID,
		OperatorTenantID:             operatorTenantID,
		InvoiceImportID:              invoiceImport.ID,
		ExecutionTargetID:            executionTargetID,
		LedgerCoverageID:             coverage.ID,
		Provider:                     invoiceImport.Provider,
		CurrencyCode:                 invoiceImport.CurrencyCode,
		BillingPeriodStartAt:         invoiceImport.BillingPeriodStartAt.UTC(),
		BillingPeriodEndAt:           invoiceImport.BillingPeriodEndAt.UTC(),
		AlgorithmVersion:             SharedActualAllocationAlgorithmProportionalEstimateV1,
		SourceChecksum:               invoiceImport.SourceChecksum,
		SourceScopeAttestationSHA256: sourceScopeAttestationSHA256,
		SourceLineSetSHA256:          lineSetSHA256,
		ImportLineCount:              importAggregate.LineCount,
		ImportAmountMicros:           importAmount,
		SourceLineCount:              int64(len(lines)),
		SourceAmountMicros:           sourceAmount,
		UnallocatedLineCount:         importAggregate.LineCount - int64(len(lines)),
		UnallocatedAmountMicros:      unallocatedAmount,
		AllocationLineCount:          int64(len(lines)),
		AllocationSliceCount:         int64(len(slices)),
		AllocatedAmountMicros:        sourceAmount,
		State:                        SharedActualAllocationStateSealed,
		CreatedBy:                    createdBy,
		CreatedAt:                    createdAt,
		SealedAt:                     &sealedAt,
	}
	return sharedActualAllocationPlan{Run: run, Lines: lines, Slices: slices}, nil
}

func loadSharedActualImportAggregate(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, importID uuid.UUID,
) (sharedActualImportAggregate, error) {
	var aggregate sharedActualImportAggregate
	err := tx.WithContext(ctx).Raw(`
		SELECT count(*) AS line_count,
		       CAST(COALESCE(sum(amount_micros), 0) AS TEXT) AS amount_text
		FROM billing_actual_invoice_lines
		WHERE tenant_id = ? AND invoice_import_id = ?
	`, operatorTenantID, importID).Scan(&aggregate).Error
	if err != nil {
		return sharedActualImportAggregate{}, problem.Wrap(
			500,
			"cost_accounting_shared_actual_invoice_aggregate_failed",
			"The actual invoice import totals could not be loaded.",
			err,
		)
	}
	return aggregate, nil
}

func parseSharedActualAggregate(value string) (int64, error) {
	parsed := new(big.Int)
	if _, ok := parsed.SetString(strings.TrimSpace(value), 10); !ok || !parsed.IsInt64() {
		return 0, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_amount_overflow",
			"The actual invoice total exceeds the supported micros range.",
		)
	}
	return parsed.Int64(), nil
}

func rejectAmbiguousSharedActualMatches(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, importID, executionTargetID uuid.UUID,
) error {
	var ambiguous int64
	err := tx.WithContext(ctx).Raw(`
		SELECT count(*)
		FROM (
			SELECT actual_line.id
			FROM billing_actual_invoice_lines AS actual_line
			JOIN billing_shared_estimated_charge_slices AS estimate_slice
			  ON estimate_slice.charge_kind = actual_line.charge_kind
			 AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
			 AND estimate_slice.billing_period_start_at = actual_line.billing_period_start_at
			 AND estimate_slice.billing_period_end_at = actual_line.billing_period_end_at
			JOIN billing_shared_cost_allocation_runs AS estimate_run
			  ON estimate_run.id = estimate_slice.run_id
			 AND estimate_run.provider = actual_line.provider
			 AND estimate_run.currency_code = actual_line.currency_code
			WHERE actual_line.tenant_id = ? AND actual_line.invoice_import_id = ?
			GROUP BY actual_line.id
			HAVING count(DISTINCT estimate_run.execution_target_id) > 1
			   AND max(CASE WHEN estimate_run.execution_target_id = ? THEN 1 ELSE 0 END) = 1
		) AS ambiguous_lines
	`, operatorTenantID, importID, executionTargetID).Scan(&ambiguous).Error
	if err != nil {
		return problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_ambiguity_probe_failed",
			"The shared actual-invoice allocation scope could not be checked.",
			err,
		)
	}
	if ambiguous > 0 {
		return problem.New(
			409,
			"cost_accounting_shared_actual_allocation_target_ambiguous",
			"At least one actual invoice line matches shared estimate history in more than one Target.",
		)
	}
	return nil
}

func countSharedActualMatches(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, importID, executionTargetID uuid.UUID,
) (int64, error) {
	var count int64
	err := sharedActualMatchesQuery(tx.WithContext(ctx), operatorTenantID, importID, executionTargetID).
		Count(&count).Error
	if err != nil {
		return 0, problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_match_count_failed",
			"The matching shared estimate slices could not be counted.",
			err,
		)
	}
	return count, nil
}

func loadSharedActualMatches(
	ctx context.Context,
	tx *gorm.DB,
	operatorTenantID, importID, executionTargetID uuid.UUID,
	expectedCount int64,
) ([]sharedActualMatchRow, error) {
	rows := make([]sharedActualMatchRow, 0, int(expectedCount))
	err := sharedActualMatchesQuery(tx.WithContext(ctx), operatorTenantID, importID, executionTargetID).
		Select(`
			actual_line.id AS actual_invoice_line_id,
			actual_line.external_line_id AS external_line_id,
			actual_line.amount_micros AS source_amount_micros,
			estimate_slice.id AS estimated_slice_id,
			estimate_slice.tenant_id AS estimated_tenant_id,
			estimate_slice.allocation_kind AS estimated_allocation_kind,
			estimate_slice.amount_micros AS estimated_amount_micros
		`).
		Order("actual_line.id, estimate_slice.id").
		Find(&rows).Error
	if err != nil {
		return nil, problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_matches_load_failed",
			"The matching shared estimate slices could not be loaded.",
			err,
		)
	}
	if int64(len(rows)) != expectedCount {
		return nil, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_snapshot_changed",
			"The shared estimate snapshot changed while actual allocation was being planned.",
		)
	}
	return rows, nil
}

func sharedActualMatchesQuery(
	db *gorm.DB,
	operatorTenantID, importID, executionTargetID uuid.UUID,
) *gorm.DB {
	return db.Table("billing_actual_invoice_lines AS actual_line").
		Joins(`JOIN billing_shared_estimated_charge_slices AS estimate_slice
		  ON estimate_slice.charge_kind = actual_line.charge_kind
		 AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
		 AND estimate_slice.billing_period_start_at = actual_line.billing_period_start_at
		 AND estimate_slice.billing_period_end_at = actual_line.billing_period_end_at`).
		Joins(`JOIN billing_shared_cost_allocation_runs AS estimate_run
		  ON estimate_run.id = estimate_slice.run_id
		 AND estimate_run.provider = actual_line.provider
		 AND estimate_run.currency_code = actual_line.currency_code`).
		Where("actual_line.tenant_id = ? AND actual_line.invoice_import_id = ?", operatorTenantID, importID).
		Where("estimate_run.execution_target_id = ?", executionTargetID)
}

func groupSharedActualMatches(rows []sharedActualMatchRow) ([]sharedActualPlannedLine, error) {
	lines := make([]sharedActualPlannedLine, 0)
	for _, row := range rows {
		if row.ActualInvoiceLineID == uuid.Nil || row.EstimatedSliceID == uuid.Nil || row.EstimatedAmountMicros < 0 {
			return nil, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_basis_invalid",
				"A matching shared estimate slice has an invalid immutable allocation weight.",
			)
		}
		if row.EstimatedAllocation != SharedAllocationKindTenantClaim &&
			row.EstimatedAllocation != SharedAllocationKindPlatformIdle {
			return nil, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_basis_invalid",
				"A matching shared estimate slice has an invalid allocation kind.",
			)
		}
		if (row.EstimatedAllocation == SharedAllocationKindTenantClaim) != (row.EstimatedTenantID != nil) {
			return nil, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_basis_invalid",
				"A matching shared estimate slice has inconsistent Tenant attribution.",
			)
		}
		if len(lines) == 0 || lines[len(lines)-1].ActualInvoiceLineID != row.ActualInvoiceLineID {
			lines = append(lines, sharedActualPlannedLine{
				ActualInvoiceLineID: row.ActualInvoiceLineID,
				ExternalLineID:      row.ExternalLineID,
				SourceAmountMicros:  row.SourceAmountMicros,
			})
		}
		line := &lines[len(lines)-1]
		if line.ExternalLineID != row.ExternalLineID || line.SourceAmountMicros != row.SourceAmountMicros {
			return nil, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_source_changed",
				"An immutable actual invoice line changed while allocation was being planned.",
			)
		}
		if len(line.Matches) > 0 && line.Matches[len(line.Matches)-1].EstimatedSliceID == row.EstimatedSliceID {
			return nil, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_basis_duplicate",
				"A shared estimate slice matched the same actual invoice line more than once.",
			)
		}
		line.Matches = append(line.Matches, row)
	}
	return lines, nil
}

func allocateSharedActualMicros(total int64, weights []int64) ([]int64, int64, error) {
	weightTotal := new(big.Int)
	for _, weight := range weights {
		if weight < 0 {
			return nil, 0, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_basis_invalid",
				"Shared actual-invoice allocation weights cannot be negative.",
			)
		}
		weightTotal.Add(weightTotal, big.NewInt(weight))
	}
	if !weightTotal.IsInt64() {
		return nil, 0, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_amount_overflow",
			"The shared estimate allocation basis exceeds the supported micros range.",
		)
	}
	estimatedTotal := weightTotal.Int64()
	amounts := make([]int64, len(weights))
	if weightTotal.Sign() == 0 {
		if total != 0 {
			return nil, 0, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_basis_zero",
				"A nonzero actual invoice line cannot be allocated over a zero estimated-cost basis.",
			)
		}
		return amounts, estimatedTotal, nil
	}

	absTotal := new(big.Int).Abs(big.NewInt(total))
	cumulativeWeight := new(big.Int)
	previousCumulativeAmount := new(big.Int)
	for index, weight := range weights {
		cumulativeWeight.Add(cumulativeWeight, big.NewInt(weight))
		cumulativeAmount := new(big.Int).Mul(absTotal, cumulativeWeight)
		cumulativeAmount.Quo(cumulativeAmount, weightTotal)
		delta := new(big.Int).Sub(cumulativeAmount, previousCumulativeAmount)
		if total < 0 {
			delta.Neg(delta)
		}
		if !delta.IsInt64() {
			return nil, 0, problem.New(
				409,
				"cost_accounting_shared_actual_allocation_amount_overflow",
				"A proportional actual-invoice slice exceeds the supported micros range.",
			)
		}
		amounts[index] = delta.Int64()
		previousCumulativeAmount = cumulativeAmount
	}
	return amounts, estimatedTotal, nil
}

func sharedActualEstimatedSliceSetSHA256(matches []sharedActualMatchRow) (string, error) {
	type digestRow struct {
		EstimatedSliceID     string `json:"estimatedSliceId"`
		TenantID             string `json:"tenantId"`
		AllocationKind       string `json:"allocationKind"`
		EstimateWeightMicros int64  `json:"estimateWeightMicros"`
	}
	rows := make([]digestRow, 0, len(matches))
	for _, match := range matches {
		tenantID := ""
		if match.EstimatedTenantID != nil {
			tenantID = match.EstimatedTenantID.String()
		}
		rows = append(rows, digestRow{
			EstimatedSliceID:     match.EstimatedSliceID.String(),
			TenantID:             tenantID,
			AllocationKind:       match.EstimatedAllocation,
			EstimateWeightMicros: match.EstimatedAmountMicros,
		})
	}
	return sharedActualCanonicalSHA256(rows)
}

func sharedActualSourceLineSetSHA256(
	lines []persistence.BillingSharedActualAllocationLine,
) (string, error) {
	type digestRow struct {
		ActualInvoiceLineID     string `json:"actualInvoiceLineId"`
		SourceAmountMicros      int64  `json:"sourceAmountMicros"`
		EstimatedAmountMicros   int64  `json:"estimatedAmountMicros"`
		EstimatedSliceCount     int64  `json:"estimatedSliceCount"`
		EstimatedSliceSetSHA256 string `json:"estimatedSliceSetSHA256"`
	}
	rows := make([]digestRow, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, digestRow{
			ActualInvoiceLineID:     line.ActualInvoiceLineID.String(),
			SourceAmountMicros:      line.SourceAmountMicros,
			EstimatedAmountMicros:   line.EstimatedAmountMicros,
			EstimatedSliceCount:     line.EstimatedSliceCount,
			EstimatedSliceSetSHA256: line.EstimatedSliceSetSHA256,
		})
	}
	return sharedActualCanonicalSHA256(rows)
}

func sharedActualCanonicalSHA256(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_digest_failed",
			"The shared actual-invoice allocation digest could not be created.",
			err,
		)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func subtractSharedActualInt64(left, right int64) (int64, error) {
	value := new(big.Int).Sub(big.NewInt(left), big.NewInt(right))
	if !value.IsInt64() {
		return 0, problem.New(
			409,
			"cost_accounting_shared_actual_allocation_amount_overflow",
			"The unallocated actual invoice total exceeds the supported micros range.",
		)
	}
	return value.Int64(), nil
}

func deterministicSharedActualAllocationRunID(
	operatorTenantID, importID, targetID uuid.UUID,
) uuid.UUID {
	return uuid.NewSHA1(
		deterministicNamespace,
		[]byte(strings.Join([]string{
			"shared-actual-allocation-run",
			operatorTenantID.String(),
			importID.String(),
			targetID.String(),
			SharedActualAllocationAlgorithmProportionalEstimateV1,
		}, "\x00")),
	)
}

func deterministicSharedActualAllocationLineID(runID, actualLineID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(
		deterministicNamespace,
		[]byte("shared-actual-allocation-line\x00"+runID.String()+"\x00"+actualLineID.String()),
	)
}

func deterministicSharedActualChargeSliceID(lineID, estimatedSliceID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(
		deterministicNamespace,
		[]byte("shared-actual-charge-slice\x00"+lineID.String()+"\x00"+estimatedSliceID.String()),
	)
}

func loadSharedActualAllocationRun(
	ctx context.Context,
	tx *gorm.DB,
	runID uuid.UUID,
) (persistence.BillingSharedActualAllocationRun, bool, error) {
	var run persistence.BillingSharedActualAllocationRun
	err := tx.WithContext(ctx).Where("id = ?", runID).Take(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.BillingSharedActualAllocationRun{}, false, nil
	}
	if err != nil {
		return persistence.BillingSharedActualAllocationRun{}, false, problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_load_failed",
			"The shared actual-invoice allocation run could not be loaded.",
			err,
		)
	}
	return run, true, nil
}

func loadSharedActualAllocationGraph(
	ctx context.Context,
	tx *gorm.DB,
	runID uuid.UUID,
) ([]persistence.BillingSharedActualAllocationLine, []persistence.BillingSharedActualChargeSlice, error) {
	var lines []persistence.BillingSharedActualAllocationLine
	if err := tx.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("actual_invoice_line_id, id").
		Find(&lines).Error; err != nil {
		return nil, nil, problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_lines_load_failed",
			"The shared actual-invoice allocation lines could not be loaded.",
			err,
		)
	}
	var slices []persistence.BillingSharedActualChargeSlice
	if err := tx.WithContext(ctx).
		Table("billing_shared_actual_charge_slices AS actual_slice").
		Select("actual_slice.*").
		Joins("JOIN billing_shared_actual_allocation_lines AS allocation_line ON allocation_line.id = actual_slice.allocation_line_id").
		Where("allocation_line.run_id = ?", runID).
		Order("allocation_line.actual_invoice_line_id, actual_slice.estimated_slice_id, actual_slice.id").
		Find(&slices).Error; err != nil {
		return nil, nil, problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_slices_load_failed",
			"The shared actual-invoice allocation slices could not be loaded.",
			err,
		)
	}
	return lines, slices, nil
}

func rejectOccupiedSharedActualAuthorities(
	ctx context.Context,
	tx *gorm.DB,
	plan sharedActualAllocationPlan,
) error {
	var occupiedLines int64
	if err := tx.WithContext(ctx).Raw(`
		SELECT count(DISTINCT existing_line.id)
		FROM billing_shared_actual_allocation_lines AS existing_line
		JOIN billing_actual_invoice_lines AS actual_line
		  ON actual_line.id = existing_line.actual_invoice_line_id
		JOIN billing_shared_estimated_charge_slices AS estimate_slice
		  ON estimate_slice.charge_kind = actual_line.charge_kind
		 AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
		 AND estimate_slice.billing_period_start_at = actual_line.billing_period_start_at
		 AND estimate_slice.billing_period_end_at = actual_line.billing_period_end_at
		JOIN billing_shared_cost_allocation_runs AS estimate_run
		  ON estimate_run.id = estimate_slice.run_id
		 AND estimate_run.provider = actual_line.provider
		 AND estimate_run.currency_code = actual_line.currency_code
		WHERE actual_line.tenant_id = ?
		  AND actual_line.invoice_import_id = ?
		  AND estimate_run.execution_target_id = ?
	`, plan.Run.OperatorTenantID, plan.Run.InvoiceImportID, plan.Run.ExecutionTargetID).
		Scan(&occupiedLines).Error; err != nil {
		return problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_occupancy_probe_failed",
			"Existing shared actual-invoice allocation lines could not be checked.",
			err,
		)
	}
	if occupiedLines > 0 {
		return problem.New(
			409,
			"cost_accounting_shared_actual_invoice_line_already_allocated",
			"At least one actual invoice line already belongs to another immutable shared allocation.",
		)
	}
	var occupiedSlices int64
	if err := tx.WithContext(ctx).Raw(`
		SELECT count(DISTINCT existing_slice.id)
		FROM billing_shared_actual_charge_slices AS existing_slice
		JOIN billing_shared_estimated_charge_slices AS estimate_slice
		  ON estimate_slice.id = existing_slice.estimated_slice_id
		JOIN billing_shared_cost_allocation_runs AS estimate_run
		  ON estimate_run.id = estimate_slice.run_id
		JOIN billing_actual_invoice_lines AS actual_line
		  ON actual_line.provider = estimate_run.provider
		 AND actual_line.currency_code = estimate_run.currency_code
		 AND actual_line.billing_period_start_at = estimate_slice.billing_period_start_at
		 AND actual_line.billing_period_end_at = estimate_slice.billing_period_end_at
		 AND actual_line.charge_kind = estimate_slice.charge_kind
		 AND actual_line.resource_correlation_key = estimate_slice.resource_correlation_key
		WHERE actual_line.tenant_id = ?
		  AND actual_line.invoice_import_id = ?
		  AND estimate_run.execution_target_id = ?
	`, plan.Run.OperatorTenantID, plan.Run.InvoiceImportID, plan.Run.ExecutionTargetID).
		Scan(&occupiedSlices).Error; err != nil {
		return problem.Wrap(
			500,
			"cost_accounting_shared_actual_allocation_occupancy_probe_failed",
			"Existing shared actual-invoice allocation slices could not be checked.",
			err,
		)
	}
	if occupiedSlices > 0 {
		return problem.New(
			409,
			"cost_accounting_shared_estimate_slice_already_allocated",
			"At least one shared estimate slice already belongs to another immutable actual allocation.",
		)
	}
	return nil
}

func sameSharedActualAllocationRun(
	existing, planned persistence.BillingSharedActualAllocationRun,
) bool {
	return existing.ID == planned.ID &&
		existing.OperatorTenantID == planned.OperatorTenantID &&
		existing.InvoiceImportID == planned.InvoiceImportID &&
		existing.ExecutionTargetID == planned.ExecutionTargetID &&
		existing.LedgerCoverageID == planned.LedgerCoverageID &&
		existing.Provider == planned.Provider &&
		existing.CurrencyCode == planned.CurrencyCode &&
		existing.BillingPeriodStartAt.UTC().Equal(planned.BillingPeriodStartAt.UTC()) &&
		existing.BillingPeriodEndAt.UTC().Equal(planned.BillingPeriodEndAt.UTC()) &&
		existing.AlgorithmVersion == planned.AlgorithmVersion &&
		existing.SourceChecksum == planned.SourceChecksum &&
		existing.SourceScopeAttestationSHA256 == planned.SourceScopeAttestationSHA256 &&
		existing.SourceLineSetSHA256 == planned.SourceLineSetSHA256 &&
		existing.ImportLineCount == planned.ImportLineCount &&
		existing.ImportAmountMicros == planned.ImportAmountMicros &&
		existing.SourceLineCount == planned.SourceLineCount &&
		existing.SourceAmountMicros == planned.SourceAmountMicros &&
		existing.UnallocatedLineCount == planned.UnallocatedLineCount &&
		existing.UnallocatedAmountMicros == planned.UnallocatedAmountMicros &&
		existing.AllocationLineCount == planned.AllocationLineCount &&
		existing.AllocationSliceCount == planned.AllocationSliceCount &&
		existing.AllocatedAmountMicros == planned.AllocatedAmountMicros &&
		existing.State == SharedActualAllocationStateSealed &&
		existing.SealedAt != nil
}

func sameSharedActualAllocationLines(
	existing, planned []persistence.BillingSharedActualAllocationLine,
) bool {
	if len(existing) != len(planned) {
		return false
	}
	existingCopy := append([]persistence.BillingSharedActualAllocationLine(nil), existing...)
	plannedCopy := append([]persistence.BillingSharedActualAllocationLine(nil), planned...)
	sort.Slice(existingCopy, func(i, j int) bool {
		return existingCopy[i].ActualInvoiceLineID.String() < existingCopy[j].ActualInvoiceLineID.String()
	})
	sort.Slice(plannedCopy, func(i, j int) bool {
		return plannedCopy[i].ActualInvoiceLineID.String() < plannedCopy[j].ActualInvoiceLineID.String()
	})
	for index := range existingCopy {
		a, b := existingCopy[index], plannedCopy[index]
		if a.ID != b.ID || a.RunID != b.RunID || a.ActualInvoiceLineID != b.ActualInvoiceLineID ||
			a.SourceAmountMicros != b.SourceAmountMicros || a.EstimatedAmountMicros != b.EstimatedAmountMicros ||
			a.AllocatedAmountMicros != b.AllocatedAmountMicros || a.EstimatedSliceCount != b.EstimatedSliceCount ||
			a.EstimatedSliceSetSHA256 != b.EstimatedSliceSetSHA256 {
			return false
		}
	}
	return true
}

func sameSharedActualChargeSlices(
	existing, planned []persistence.BillingSharedActualChargeSlice,
) bool {
	if len(existing) != len(planned) {
		return false
	}
	existingCopy := append([]persistence.BillingSharedActualChargeSlice(nil), existing...)
	plannedCopy := append([]persistence.BillingSharedActualChargeSlice(nil), planned...)
	sort.Slice(existingCopy, func(i, j int) bool {
		return existingCopy[i].ID.String() < existingCopy[j].ID.String()
	})
	sort.Slice(plannedCopy, func(i, j int) bool {
		return plannedCopy[i].ID.String() < plannedCopy[j].ID.String()
	})
	for index := range existingCopy {
		a, b := existingCopy[index], plannedCopy[index]
		if a.ID != b.ID || a.AllocationLineID != b.AllocationLineID || a.EstimatedSliceID != b.EstimatedSliceID ||
			!sameOptionalUUID(a.TenantID, b.TenantID) || a.AllocationKind != b.AllocationKind ||
			a.EstimateWeightMicros != b.EstimateWeightMicros || a.AmountMicros != b.AmountMicros {
			return false
		}
	}
	return true
}

func viewSharedActualAllocationRun(
	run persistence.BillingSharedActualAllocationRun,
) SharedActualInvoiceAllocationRun {
	view := SharedActualInvoiceAllocationRun{
		ID:                           run.ID,
		OperatorTenantID:             run.OperatorTenantID,
		InvoiceImportID:              run.InvoiceImportID,
		ExecutionTargetID:            run.ExecutionTargetID,
		LedgerCoverageID:             run.LedgerCoverageID,
		Provider:                     run.Provider,
		CurrencyCode:                 run.CurrencyCode,
		BillingPeriodStartAt:         run.BillingPeriodStartAt.UTC(),
		BillingPeriodEndAt:           run.BillingPeriodEndAt.UTC(),
		AlgorithmVersion:             run.AlgorithmVersion,
		SourceChecksum:               run.SourceChecksum,
		SourceScopeAttestationSHA256: run.SourceScopeAttestationSHA256,
		SourceLineSetSHA256:          run.SourceLineSetSHA256,
		ImportLineCount:              run.ImportLineCount,
		ImportAmountMicros:           run.ImportAmountMicros,
		SourceLineCount:              run.SourceLineCount,
		SourceAmountMicros:           run.SourceAmountMicros,
		UnallocatedLineCount:         run.UnallocatedLineCount,
		UnallocatedAmountMicros:      run.UnallocatedAmountMicros,
		AllocationLineCount:          run.AllocationLineCount,
		AllocationSliceCount:         run.AllocationSliceCount,
		AllocatedAmountMicros:        run.AllocatedAmountMicros,
		State:                        run.State,
		CreatedBy:                    run.CreatedBy,
		CreatedAt:                    run.CreatedAt.UTC(),
	}
	if run.SealedAt != nil {
		sealedAt := run.SealedAt.UTC()
		view.SealedAt = &sealedAt
	}
	return view
}

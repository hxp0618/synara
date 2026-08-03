package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	ChargeKindCPU              = "cpu"
	ChargeKindMemory           = "memory"
	ChargeKindEphemeralStorage = "ephemeral-storage"
	ChargeKindRequest          = "request"
	ChargeKindPod              = "pod"

	reconciliationStatePending         = "pending"
	reconciliationStateMatched         = "matched"
	reconciliationStateVariance        = "variance"
	reconciliationStateUnmatchedActual = "unmatched-actual"
)

var deterministicNamespace = uuid.MustParse("6eecfd83-2e09-47f7-b7c4-f29fd2b8d4f0")

type CreateTariffInput struct {
	Provider                   string     `json:"provider"`
	Region                     string     `json:"region"`
	CurrencyCode               string     `json:"currencyCode"`
	Version                    int64      `json:"version"`
	EffectiveStartAt           time.Time  `json:"effectiveStartAt"`
	EffectiveEndAt             *time.Time `json:"effectiveEndAt,omitempty"`
	CPUCoreHourRateMicros      int64      `json:"cpuCoreHourRateMicros"`
	MemoryGiBHourRateMicros    int64      `json:"memoryGiBHourRateMicros"`
	EphemeralGiBHourRateMicros int64      `json:"ephemeralGiBHourRateMicros"`
	RequestRateMicros          int64      `json:"requestRateMicros"`
	PodHourRateMicros          int64      `json:"podHourRateMicros"`
}

type ListTariffsFilter struct {
	Provider     *string
	Region       *string
	CurrencyCode *string
	EffectiveAt  *time.Time
}

type Tariff struct {
	ID                         uuid.UUID  `json:"id"`
	Provider                   string     `json:"provider"`
	Region                     string     `json:"region"`
	CurrencyCode               string     `json:"currencyCode"`
	Version                    int64      `json:"version"`
	EffectiveStartAt           time.Time  `json:"effectiveStartAt"`
	EffectiveEndAt             *time.Time `json:"effectiveEndAt,omitempty"`
	CPUCoreHourRateMicros      int64      `json:"cpuCoreHourRateMicros"`
	MemoryGiBHourRateMicros    int64      `json:"memoryGiBHourRateMicros"`
	EphemeralGiBHourRateMicros int64      `json:"ephemeralGiBHourRateMicros"`
	RequestRateMicros          int64      `json:"requestRateMicros"`
	PodHourRateMicros          int64      `json:"podHourRateMicros"`
	CreatedAt                  time.Time  `json:"createdAt"`
}

type EstimateUsageChargesInput struct {
	Provider             string
	CurrencyCode         string
	WorkerID             uuid.UUID
	WorkerIncarnation    int64
	BillingPeriodStartAt time.Time
	BillingPeriodEndAt   time.Time
}

type ImportActualInvoiceRequest struct {
	TenantID         uuid.UUID
	Provider         string
	ExternalImportID string
}

type ImportActualInvoiceResult struct {
	Import           persistence.BillingActualInvoiceImport
	Lines            []persistence.BillingActualInvoiceLine
	SourceProvenance *InvoiceSourceProvenance
}

type ReconciledActualInvoiceLine struct {
	Line               persistence.BillingActualInvoiceLine
	MatchedEstimateIDs []uuid.UUID
}

type ReconciliationReport struct {
	Import                        persistence.BillingActualInvoiceImport
	Lines                         []ReconciledActualInvoiceLine
	UnmatchedEstimateIDs          []uuid.UUID
	UnmatchedEstimateAmountMicros int64
}

type importedInvoiceMutation struct {
	Result  ImportActualInvoiceResult
	Created bool
}

type reconciliationMutation struct {
	Report  ReconciliationReport
	Changed bool
}

type Service struct {
	db                              *gorm.DB
	adapter                         Adapter
	authorizer                      *authorization.Authorizer
	auditRecorder                   func(context.Context, *gorm.DB, audit.Entry) error
	platformBillingOperatorTenantID uuid.UUID
	configuredImports               map[string]ConfiguredImport
	configuredSharedAllocations     map[string]ConfiguredSharedAllocation
	estimateSweeper                 EstimateSweeper
	sharedAllocationMu              sync.Mutex
	schedulerMu                     sync.Mutex
	schedulerState                  map[schedulerStateKey]scheduledImportState
	sharedSchedulerRunMu            sync.Mutex
	now                             func() time.Time
}

type ServiceOption func(*Service)

func WithConfiguredImports(imports []ConfiguredImport) ServiceOption {
	return func(service *Service) {
		if service == nil {
			return
		}
		service.configuredImports = make(map[string]ConfiguredImport, len(imports))
		for _, configuredImport := range imports {
			configuredImport.ExecutionTargetIDs = append([]uuid.UUID(nil), configuredImport.ExecutionTargetIDs...)
			service.configuredImports[configuredImportKey(configuredImport.TenantID, configuredImport.Provider, configuredImport.ExternalImportID)] = configuredImport
		}
	}
}

func WithConfiguredSharedAllocations(allocations []ConfiguredSharedAllocation) ServiceOption {
	return func(service *Service) {
		if service == nil {
			return
		}
		service.configuredSharedAllocations = make(map[string]ConfiguredSharedAllocation, len(allocations))
		for _, configuredAllocation := range allocations {
			if configuredAllocation.LastPeriodEndAt != nil {
				last := *configuredAllocation.LastPeriodEndAt
				configuredAllocation.LastPeriodEndAt = &last
			}
			service.configuredSharedAllocations[configuredSharedAllocationKey(configuredAllocation)] = configuredAllocation
		}
	}
}

func WithEstimateSweeper(sweeper EstimateSweeper) ServiceOption {
	return func(service *Service) {
		if service == nil {
			return
		}
		service.estimateSweeper = sweeper
	}
}

func WithBuiltInEstimateSweeper() ServiceOption {
	return func(service *Service) {
		if service == nil {
			return
		}
		service.estimateSweeper = service
	}
}

func WithAuditRecorder(recorder func(context.Context, *gorm.DB, audit.Entry) error) ServiceOption {
	return func(service *Service) {
		if service == nil || recorder == nil {
			return
		}
		service.auditRecorder = recorder
	}
}

// WithPlatformBillingOperatorTenant binds platform-global billing mutations,
// including the provider-rate catalog and shared-Target ledger coverage, to
// one explicitly configured operator Tenant. A zero UUID disables those
// authenticated mutation surfaces while leaving internal accounting methods
// available.
func WithPlatformBillingOperatorTenant(tenantID uuid.UUID) ServiceOption {
	return func(service *Service) {
		if service == nil {
			return
		}
		service.platformBillingOperatorTenantID = tenantID
	}
}

// WithTariffOperatorTenant is retained as a compatibility alias for runtime
// configuration and callers that predate shared-Target billing management.
func WithTariffOperatorTenant(tenantID uuid.UUID) ServiceOption {
	return WithPlatformBillingOperatorTenant(tenantID)
}

func NewService(db *gorm.DB, adapter Adapter, options ...ServiceOption) *Service {
	service := &Service{
		db:             db,
		adapter:        adapter,
		authorizer:     authorization.NewAuthorizer(db),
		auditRecorder:  audit.Record,
		schedulerState: make(map[schedulerStateKey]scheduledImportState),
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service
}

func (s *Service) CreateTariff(
	ctx context.Context,
	input CreateTariffInput,
) (persistence.BillingProviderTariff, error) {
	normalized, err := normalizeTariffInput(input)
	if err != nil {
		return persistence.BillingProviderTariff{}, err
	}

	model := persistence.BillingProviderTariff{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var createErr error
		model, createErr = s.createTariffTx(ctx, tx, normalized)
		return createErr
	})
	if err != nil {
		return persistence.BillingProviderTariff{}, err
	}
	return model, nil
}

func (s *Service) ListTariffs(
	ctx context.Context,
	filter ListTariffsFilter,
) ([]persistence.BillingProviderTariff, error) {
	normalized, err := normalizeTariffListFilter(filter)
	if err != nil {
		return nil, err
	}
	return loadTariffs(ctx, s.db, normalized)
}

func (s *Service) EstimateUsageCharges(
	ctx context.Context,
	input EstimateUsageChargesInput,
) ([]persistence.BillingEstimatedUsageCharge, error) {
	provider, err := normalizeProvider(input.Provider)
	if err != nil {
		return nil, err
	}
	currency, err := normalizeCurrency(input.CurrencyCode)
	if err != nil {
		return nil, err
	}
	periodStart, periodEnd, err := normalizeClosedPeriod(input.BillingPeriodStartAt, input.BillingPeriodEndAt)
	if err != nil {
		return nil, err
	}
	if input.WorkerID == uuid.Nil || input.WorkerIncarnation <= 0 {
		return nil, problem.New(400, "invalid_cost_accounting_worker", "workerId and workerIncarnation are required.")
	}

	rows := make([]persistence.BillingEstimatedUsageCharge, 0)
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		fact, loadErr := loadWorkerFactForBilling(ctx, tx, input.WorkerID, input.WorkerIncarnation)
		if loadErr != nil {
			return loadErr
		}
		if fact.TenantID == nil {
			return problem.New(409, "cost_accounting_worker_fact_tenant_unattributed", "The cost-accounting Worker incarnation fact is not attributed to a tenant.")
		}

		usageStart := maxTime(periodStart, fact.RegisteredAt.UTC())
		usageEnd := periodEnd
		if fact.TerminatedAt != nil && fact.TerminatedAt.UTC().Before(usageEnd) {
			usageEnd = fact.TerminatedAt.UTC()
		}
		if !usageEnd.After(usageStart) {
			rows = nil
			return nil
		}

		tariffs, tariffErr := loadCandidateTariffs(ctx, tx, provider, fact.Region, currency, usageStart, usageEnd)
		if tariffErr != nil {
			return tariffErr
		}
		segments, segmentErr := buildChargeSegments(fact.Region, usageStart, usageEnd, tariffs)
		if segmentErr != nil {
			return segmentErr
		}
		requestPlan, requestChargeErr := buildRequestChargePlan(
			ctx,
			tx,
			fact,
			usageStart,
			usageEnd,
			periodStart,
			periodEnd,
			segments,
		)
		if requestChargeErr != nil {
			return requestChargeErr
		}

		persisted := make([]persistence.BillingEstimatedUsageCharge, 0, len(segments)*4+1)
		resourceKey := workerResourceCorrelationKey(fact)
		now := s.now().UTC()
		for index, segment := range segments {
			billableSeconds := wholeSecondsBetween(segment.StartAt, segment.EndAt)
			if billableSeconds <= 0 {
				continue
			}
			timeRows, buildErr := buildTimeBasedEstimateRows(
				fact, segment.Tariff, resourceKey, periodStart, periodEnd, segment.StartAt, segment.EndAt, billableSeconds, now,
			)
			if buildErr != nil {
				return buildErr
			}
			persisted = append(persisted, timeRows...)

			if requestPlan.UseLedger {
				requestRow, ok, requestErr := buildRequestEstimateRow(
					fact, segment.Tariff, resourceKey, periodStart, periodEnd, segment.StartAt, segment.EndAt, billableSeconds, requestPlan.SegmentClaimCounts[index], now,
				)
				if requestErr != nil {
					return requestErr
				}
				if ok {
					persisted = append(persisted, requestRow)
				}
			} else if requestPlan.FallbackAuthorized && index == len(segments)-1 {
				requestRow, ok, requestErr := buildRequestEstimateRow(
					fact, segment.Tariff, resourceKey, periodStart, periodEnd, segment.StartAt, segment.EndAt, billableSeconds, requestPlan.FallbackClaimCount, now,
				)
				if requestErr != nil {
					return requestErr
				}
				if ok {
					persisted = append(persisted, requestRow)
				}
			}
		}

		for _, row := range persisted {
			if err := tx.WithContext(ctx).
				Clauses(clause.OnConflict{DoNothing: true}).
				Create(&row).Error; err != nil {
				return problem.Wrap(409, "cost_accounting_estimate_create_failed", "The estimated usage cost could not be created.", err)
			}
			rows = append(rows, row)
		}

		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].UsageStartAt.Equal(rows[j].UsageStartAt) {
				return rows[i].UsageStartAt.Before(rows[j].UsageStartAt)
			}
			if rows[i].ChargeKind != rows[j].ChargeKind {
				return chargeKindOrder(rows[i].ChargeKind) < chargeKindOrder(rows[j].ChargeKind)
			}
			return rows[i].ID.String() < rows[j].ID.String()
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Service) ImportActualInvoice(
	ctx context.Context,
	request ImportActualInvoiceRequest,
) (ImportActualInvoiceResult, error) {
	normalizedRequest, normalizedInvoice, checksum, err := s.prepareActualInvoiceImport(ctx, request)
	if err != nil {
		return ImportActualInvoiceResult{}, err
	}
	result := ImportActualInvoiceResult{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		mutation, importErr := s.importActualInvoiceTx(ctx, tx, normalizedRequest, normalizedInvoice, checksum)
		result = mutation.Result
		return importErr
	})
	if err != nil {
		return ImportActualInvoiceResult{}, err
	}
	return result, nil
}

func (s *Service) prepareActualInvoiceImport(
	ctx context.Context,
	request ImportActualInvoiceRequest,
) (ImportActualInvoiceRequest, ImportedActualInvoice, string, error) {
	tenantID, err := normalizeBillingTenantID(request.TenantID)
	if err != nil {
		return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", err
	}
	provider, err := normalizeProvider(request.Provider)
	if err != nil {
		return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", err
	}
	request.TenantID = tenantID
	request.Provider = provider
	request.ExternalImportID = strings.TrimSpace(request.ExternalImportID)
	if request.ExternalImportID == "" {
		return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_import", "externalImportId is required.")
	}
	if s.adapter == nil {
		return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", problem.New(500, "cost_accounting_adapter_unavailable", "The cost source adapter is not configured.")
	}

	imported, err := s.adapter.FetchActualInvoice(ctx, request)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvoiceNotFound):
			return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", problem.New(404, "cost_accounting_invoice_not_found", "The requested provider cost invoice import was not found.")
		default:
			return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", problem.Wrap(502, "cost_accounting_invoice_fetch_failed", "The provider cost invoice import could not be fetched.", err)
		}
	}

	normalizedInvoice, checksum, err := normalizeImportedInvoice(request, imported)
	if err != nil {
		return ImportActualInvoiceRequest{}, ImportedActualInvoice{}, "", err
	}
	return request, normalizedInvoice, checksum, nil
}

func (s *Service) importActualInvoiceTx(
	ctx context.Context,
	tx *gorm.DB,
	request ImportActualInvoiceRequest,
	normalizedInvoice ImportedActualInvoice,
	checksum string,
) (importedInvoiceMutation, error) {
	now := s.now().UTC()
	model := persistence.BillingActualInvoiceImport{
		ID:                   deterministicImportID(request.TenantID, request.Provider, normalizedInvoice.ExternalImportID),
		TenantID:             request.TenantID,
		Provider:             request.Provider,
		ExternalImportID:     normalizedInvoice.ExternalImportID,
		BillingPeriodStartAt: normalizedInvoice.BillingPeriodStartAt,
		BillingPeriodEndAt:   normalizedInvoice.BillingPeriodEndAt,
		CurrencyCode:         normalizedInvoice.CurrencyCode,
		SourceChecksum:       checksum,
		ImportedAt:           now,
		CreatedAt:            now,
	}
	if err := acquireBillingInvoiceImportIdentityLock(
		ctx,
		tx,
		model.TenantID,
		model.Provider,
		model.ExternalImportID,
	); err != nil {
		return importedInvoiceMutation{}, problem.Wrap(
			500,
			"cost_accounting_invoice_import_coordination_failed",
			"The provider cost invoice import could not be coordinated.",
			err,
		)
	}

	var existing persistence.BillingActualInvoiceImport
	existingErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND provider = ? AND external_import_id = ?", model.TenantID, model.Provider, model.ExternalImportID).
		Take(&existing).Error
	switch {
	case existingErr == nil:
		if existing.SourceChecksum != checksum {
			return importedInvoiceMutation{}, problem.New(409, "cost_accounting_invoice_import_conflict", "The provider cost invoice import external ID is already bound to different contents.")
		}
		lines, lineErr := loadActualInvoiceLines(ctx, tx, existing.TenantID, existing.ID)
		if lineErr != nil {
			return importedInvoiceMutation{}, lineErr
		}
		return importedInvoiceMutation{Result: ImportActualInvoiceResult{
			Import: existing, Lines: lines, SourceProvenance: cloneInvoiceSourceProvenance(normalizedInvoice.SourceProvenance),
		}}, nil
	case !errors.Is(existingErr, gorm.ErrRecordNotFound):
		return importedInvoiceMutation{}, problem.Wrap(500, "cost_accounting_invoice_import_load_failed", "The provider cost invoice import could not be loaded.", existingErr)
	}

	if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
		return importedInvoiceMutation{}, problem.Wrap(409, "cost_accounting_invoice_import_create_failed", "The provider cost invoice import could not be created.", err)
	}
	lines := make([]persistence.BillingActualInvoiceLine, 0, len(normalizedInvoice.Lines))
	for _, line := range normalizedInvoice.Lines {
		lines = append(lines, persistence.BillingActualInvoiceLine{
			ID:                          deterministicLineID(model.TenantID, model.ID, line.ExternalLineID),
			TenantID:                    model.TenantID,
			InvoiceImportID:             model.ID,
			ExternalLineID:              line.ExternalLineID,
			Provider:                    request.Provider,
			CurrencyCode:                line.CurrencyCode,
			ChargeKind:                  line.ChargeKind,
			ResourceCorrelationKey:      line.ResourceCorrelationKey,
			BillingPeriodStartAt:        line.BillingPeriodStartAt,
			BillingPeriodEndAt:          line.BillingPeriodEndAt,
			AmountMicros:                line.AmountMicros,
			ReconciliationState:         reconciliationStatePending,
			MatchedEstimateCount:        0,
			MatchedEstimateAmountMicros: 0,
			CreatedAt:                   now,
		})
	}
	if len(lines) > 0 {
		if err := tx.WithContext(ctx).Create(&lines).Error; err != nil {
			return importedInvoiceMutation{}, problem.Wrap(409, "cost_accounting_invoice_line_create_failed", "The provider cost invoice lines could not be created.", err)
		}
	}
	return importedInvoiceMutation{
		Result: ImportActualInvoiceResult{
			Import: model, Lines: lines, SourceProvenance: cloneInvoiceSourceProvenance(normalizedInvoice.SourceProvenance),
		},
		Created: true,
	}, nil
}

func acquireBillingInvoiceImportIdentityLock(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	provider string,
	externalImportID string,
) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	lockKey := strings.Join([]string{
		"synara:billing-actual-invoice-import",
		tenantID.String(),
		provider,
		externalImportID,
	}, "\x1f")
	return tx.WithContext(ctx).
		Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", lockKey).
		Error
}

func (s *Service) ReconcileActualInvoiceImport(
	ctx context.Context,
	tenantID uuid.UUID,
	importID uuid.UUID,
) (ReconciliationReport, error) {
	tenantID, err := normalizeBillingTenantID(tenantID)
	if err != nil {
		return ReconciliationReport{}, err
	}
	if importID == uuid.Nil {
		return ReconciliationReport{}, problem.New(400, "invalid_cost_accounting_invoice_import", "importId is required.")
	}

	report := ReconciliationReport{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		mutation, reconcileErr := s.reconcileActualInvoiceImportTx(ctx, tx, tenantID, importID)
		report = mutation.Report
		return reconcileErr
	})
	if err != nil {
		return ReconciliationReport{}, err
	}
	return report, nil
}

func (s *Service) reconcileActualInvoiceImportTx(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	importID uuid.UUID,
) (reconciliationMutation, error) {
	var invoice persistence.BillingActualInvoiceImport
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ?", tenantID, importID).
		Take(&invoice).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return reconciliationMutation{}, problem.New(404, "cost_accounting_invoice_import_not_found", "The provider cost invoice import was not found.")
		}
		return reconciliationMutation{}, problem.Wrap(500, "cost_accounting_invoice_import_load_failed", "The provider cost invoice import could not be loaded.", err)
	}
	lines, err := loadActualInvoiceLines(ctx, tx, invoice.TenantID, invoice.ID)
	if err != nil {
		return reconciliationMutation{}, err
	}
	report := ReconciliationReport{Import: invoice}
	changed := false

	now := s.now().UTC()
	matchedEstimateIDs := make(map[uuid.UUID]struct{})
	reconciled := make([]ReconciledActualInvoiceLine, 0, len(lines))
	for _, line := range lines {
		estimates, err := loadEstimateMatches(ctx, tx, line)
		if err != nil {
			return reconciliationMutation{}, err
		}
		estimateIDs := make([]uuid.UUID, 0, len(estimates))
		var estimateSum int64
		for _, estimate := range estimates {
			estimateIDs = append(estimateIDs, estimate.ID)
			matchedEstimateIDs[estimate.ID] = struct{}{}
			estimateSum, err = addInt64Checked(estimateSum, estimate.AmountMicros)
			if err != nil {
				return reconciliationMutation{}, err
			}
		}

		state := reconciliationStateUnmatchedActual
		if len(estimates) > 0 {
			if estimateSum == line.AmountMicros {
				state = reconciliationStateMatched
			} else {
				state = reconciliationStateVariance
			}
		}

		lineChanged := line.ReconciliationState != state ||
			line.MatchedEstimateCount != len(estimates) ||
			line.MatchedEstimateAmountMicros != estimateSum ||
			line.ReconciledAt == nil
		if lineChanged {
			updates := map[string]any{
				"reconciliation_state":           state,
				"matched_estimate_count":         len(estimates),
				"matched_estimate_amount_micros": estimateSum,
				"reconciled_at":                  now,
			}
			if err := tx.WithContext(ctx).
				Model(&persistence.BillingActualInvoiceLine{}).
				Where("tenant_id = ? AND id = ?", invoice.TenantID, line.ID).
				Updates(updates).Error; err != nil {
				return reconciliationMutation{}, problem.Wrap(500, "cost_accounting_reconciliation_update_failed", "The provider cost invoice reconciliation state could not be updated.", err)
			}
			changed = true
			line.ReconciledAt = &now
		}

		line.ReconciliationState = state
		line.MatchedEstimateCount = len(estimates)
		line.MatchedEstimateAmountMicros = estimateSum
		reconciled = append(reconciled, ReconciledActualInvoiceLine{
			Line:               line,
			MatchedEstimateIDs: estimateIDs,
		})
	}

	unmatched, err := loadUnmatchedEstimates(ctx, tx, invoice, matchedEstimateIDs)
	if err != nil {
		return reconciliationMutation{}, err
	}
	report.Lines = reconciled
	report.UnmatchedEstimateIDs = make([]uuid.UUID, 0, len(unmatched))
	for _, estimate := range unmatched {
		report.UnmatchedEstimateIDs = append(report.UnmatchedEstimateIDs, estimate.ID)
		report.UnmatchedEstimateAmountMicros, err = addInt64Checked(report.UnmatchedEstimateAmountMicros, estimate.AmountMicros)
		if err != nil {
			return reconciliationMutation{}, err
		}
	}
	return reconciliationMutation{Report: report, Changed: changed}, nil
}

type chargeSegment struct {
	StartAt time.Time
	EndAt   time.Time
	Tariff  persistence.BillingProviderTariff
}

type requestChargePlan struct {
	UseLedger          bool
	SegmentClaimCounts []int64
	FallbackAuthorized bool
	FallbackClaimCount int64
}

func buildTimeBasedEstimateRows(
	fact persistence.WorkerIncarnationFact,
	tariff persistence.BillingProviderTariff,
	resourceKey string,
	billingPeriodStartAt, billingPeriodEndAt, usageStartAt, usageEndAt time.Time,
	billableSeconds int64,
	createdAt time.Time,
) ([]persistence.BillingEstimatedUsageCharge, error) {
	rows := make([]persistence.BillingEstimatedUsageCharge, 0, 4)
	for _, candidate := range []struct {
		kind  string
		rate  int64
		build func() (int64, bool, error)
	}{
		{
			kind: ChargeKindCPU,
			rate: tariff.CPUCoreHourRateMicros,
			build: func() (int64, bool, error) {
				if fact.RequestedCPUMillicores == nil || tariff.CPUCoreHourRateMicros <= 0 {
					return 0, false, nil
				}
				amount, err := multiplyDivideRound(
					[]int64{tariff.CPUCoreHourRateMicros, *fact.RequestedCPUMillicores, billableSeconds},
					1000*3600,
				)
				return amount, true, err
			},
		},
		{
			kind: ChargeKindMemory,
			rate: tariff.MemoryGiBHourRateMicros,
			build: func() (int64, bool, error) {
				if fact.RequestedMemoryBytes == nil || tariff.MemoryGiBHourRateMicros <= 0 {
					return 0, false, nil
				}
				amount, err := multiplyDivideRound(
					[]int64{tariff.MemoryGiBHourRateMicros, *fact.RequestedMemoryBytes, billableSeconds},
					(1<<30)*3600,
				)
				return amount, true, err
			},
		},
		{
			kind: ChargeKindEphemeralStorage,
			rate: tariff.EphemeralGiBHourRateMicros,
			build: func() (int64, bool, error) {
				if fact.RequestedEphemeralStorageBytes == nil || tariff.EphemeralGiBHourRateMicros <= 0 {
					return 0, false, nil
				}
				amount, err := multiplyDivideRound(
					[]int64{tariff.EphemeralGiBHourRateMicros, *fact.RequestedEphemeralStorageBytes, billableSeconds},
					(1<<30)*3600,
				)
				return amount, true, err
			},
		},
		{
			kind: ChargeKindPod,
			rate: tariff.PodHourRateMicros,
			build: func() (int64, bool, error) {
				if tariff.PodHourRateMicros <= 0 {
					return 0, false, nil
				}
				amount, err := multiplyDivideRound(
					[]int64{tariff.PodHourRateMicros, billableSeconds},
					3600,
				)
				return amount, true, err
			},
		},
	} {
		amount, ok, err := candidate.build()
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rows = append(rows, newEstimatedChargeRow(
			fact,
			tariff,
			candidate.kind,
			resourceKey,
			billingPeriodStartAt,
			billingPeriodEndAt,
			usageStartAt,
			usageEndAt,
			billableSeconds,
			0,
			candidate.rate,
			amount,
			createdAt,
		))
	}
	return rows, nil
}

func buildRequestEstimateRow(
	fact persistence.WorkerIncarnationFact,
	tariff persistence.BillingProviderTariff,
	resourceKey string,
	billingPeriodStartAt, billingPeriodEndAt, usageStartAt, usageEndAt time.Time,
	billableSeconds int64,
	claimCount int64,
	createdAt time.Time,
) (persistence.BillingEstimatedUsageCharge, bool, error) {
	if tariff.RequestRateMicros <= 0 || claimCount <= 0 {
		return persistence.BillingEstimatedUsageCharge{}, false, nil
	}
	amount, err := multiplyDivideRound([]int64{tariff.RequestRateMicros, claimCount}, 1)
	if err != nil {
		return persistence.BillingEstimatedUsageCharge{}, false, err
	}
	return newEstimatedChargeRow(
		fact,
		tariff,
		ChargeKindRequest,
		resourceKey,
		billingPeriodStartAt,
		billingPeriodEndAt,
		usageStartAt,
		usageEndAt,
		billableSeconds,
		claimCount,
		tariff.RequestRateMicros,
		amount,
		createdAt,
	), true, nil
}

func newEstimatedChargeRow(
	fact persistence.WorkerIncarnationFact,
	tariff persistence.BillingProviderTariff,
	chargeKind, resourceKey string,
	billingPeriodStartAt, billingPeriodEndAt, usageStartAt, usageEndAt time.Time,
	billableSeconds, claimCount, rateMicros, amountMicros int64,
	createdAt time.Time,
) persistence.BillingEstimatedUsageCharge {
	row := persistence.BillingEstimatedUsageCharge{
		TenantID:                       fact.TenantID,
		ExecutionTargetID:              fact.ExecutionTargetID,
		TargetKind:                     fact.TargetKind,
		WorkerID:                       fact.WorkerID,
		WorkerIncarnation:              fact.WorkerIncarnation,
		TariffID:                       tariff.ID,
		Provider:                       tariff.Provider,
		Region:                         fact.Region,
		CurrencyCode:                   tariff.CurrencyCode,
		ChargeKind:                     chargeKind,
		ResourceCorrelationKey:         resourceKey,
		BillingPeriodStartAt:           billingPeriodStartAt,
		BillingPeriodEndAt:             billingPeriodEndAt,
		UsageStartAt:                   usageStartAt,
		UsageEndAt:                     usageEndAt,
		BillableSeconds:                billableSeconds,
		ClaimCount:                     claimCount,
		RequestedCPUMillicores:         cloneOptionalInt64(fact.RequestedCPUMillicores),
		RequestedMemoryBytes:           cloneOptionalInt64(fact.RequestedMemoryBytes),
		RequestedEphemeralStorageBytes: cloneOptionalInt64(fact.RequestedEphemeralStorageBytes),
		RateMicros:                     rateMicros,
		AmountMicros:                   amountMicros,
		CreatedAt:                      createdAt,
	}
	row.ID = deterministicEstimatedChargeID(row)
	return row
}

func buildChargeSegments(
	workerRegion string,
	usageStartAt, usageEndAt time.Time,
	tariffs []persistence.BillingProviderTariff,
) ([]chargeSegment, error) {
	if len(tariffs) == 0 {
		return nil, problem.New(409, "cost_accounting_tariff_missing", "No cost tariff covers the requested usage window.")
	}

	boundaries := []time.Time{usageStartAt, usageEndAt}
	for _, tariff := range tariffs {
		if tariff.EffectiveStartAt.UTC().After(usageStartAt) && tariff.EffectiveStartAt.UTC().Before(usageEndAt) {
			boundaries = append(boundaries, tariff.EffectiveStartAt.UTC())
		}
		if tariff.EffectiveEndAt != nil {
			end := tariff.EffectiveEndAt.UTC()
			if end.After(usageStartAt) && end.Before(usageEndAt) {
				boundaries = append(boundaries, end)
			}
		}
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].Before(boundaries[j]) })
	boundaries = dedupeTimes(boundaries)

	segments := make([]chargeSegment, 0, len(boundaries)-1)
	for index := 0; index < len(boundaries)-1; index++ {
		startAt, endAt := boundaries[index], boundaries[index+1]
		if !endAt.After(startAt) {
			continue
		}
		tariff, ok := selectTariff(workerRegion, startAt, tariffs)
		if !ok {
			return nil, problem.New(409, "cost_accounting_tariff_missing", "A cost tariff gap exists inside the requested usage window.")
		}
		segments = append(segments, chargeSegment{StartAt: startAt, EndAt: endAt, Tariff: tariff})
	}
	return segments, nil
}

func selectTariff(
	workerRegion string,
	at time.Time,
	tariffs []persistence.BillingProviderTariff,
) (persistence.BillingProviderTariff, bool) {
	var global *persistence.BillingProviderTariff
	for index := range tariffs {
		tariff := tariffs[index]
		if tariff.EffectiveStartAt.UTC().After(at) {
			continue
		}
		if tariff.EffectiveEndAt != nil && !tariff.EffectiveEndAt.UTC().After(at) {
			continue
		}
		if tariff.Region == workerRegion {
			return tariff, true
		}
		if tariff.Region == "" && global == nil {
			global = &tariff
		}
	}
	if global != nil {
		return *global, true
	}
	return persistence.BillingProviderTariff{}, false
}

func loadCandidateTariffs(
	ctx context.Context,
	tx *gorm.DB,
	provider, workerRegion, currency string,
	usageStartAt, usageEndAt time.Time,
) ([]persistence.BillingProviderTariff, error) {
	query := tx.WithContext(ctx).
		Where("provider = ? AND currency_code = ?", provider, currency).
		Where("effective_start_at < ?", usageEndAt).
		Where("(effective_end_at IS NULL OR effective_end_at > ?)", usageStartAt)
	if strings.TrimSpace(workerRegion) == "" {
		query = query.Where("region = ''")
	} else {
		query = query.Where("region IN ?", []string{workerRegion, ""})
	}
	var tariffs []persistence.BillingProviderTariff
	if err := query.Order("effective_start_at ASC, version ASC").Find(&tariffs).Error; err != nil {
		return nil, problem.Wrap(500, "cost_accounting_tariff_load_failed", "The cost tariffs could not be loaded.", err)
	}
	return tariffs, nil
}

func authoritativeRequestChargeClaimCount(
	fact persistence.WorkerIncarnationFact,
	billingPeriodStartAt, billingPeriodEndAt time.Time,
	segments []chargeSegment,
) (int64, bool, error) {
	if fact.ClaimCount <= 0 || len(segments) == 0 {
		return 0, false, nil
	}
	var requestRate *int64
	requestChargeEnabled := false
	for _, segment := range segments {
		rate := segment.Tariff.RequestRateMicros
		if requestRate == nil {
			candidate := rate
			requestRate = &candidate
		} else if *requestRate != rate {
			return 0, false, problem.New(
				409,
				"cost_accounting_request_charge_delta_unavailable",
				"The cost-accounting Worker incarnation fact does not provide authoritative per-period request charges across tariff changes.",
			)
		}
		if rate > 0 {
			requestChargeEnabled = true
		}
	}
	if !requestChargeEnabled {
		return 0, false, nil
	}
	if fact.RegisteredAt.UTC().Before(billingPeriodStartAt) || fact.TerminatedAt == nil || fact.TerminatedAt.UTC().After(billingPeriodEndAt) {
		return 0, false, problem.New(
			409,
			"cost_accounting_request_charge_delta_unavailable",
			"The cost-accounting Worker incarnation fact does not provide authoritative per-period request charges outside a fully enclosed incarnation lifetime.",
		)
	}
	return fact.ClaimCount, true, nil
}

func buildRequestChargePlan(
	ctx context.Context,
	tx *gorm.DB,
	fact persistence.WorkerIncarnationFact,
	usageStartAt, usageEndAt, billingPeriodStartAt, billingPeriodEndAt time.Time,
	segments []chargeSegment,
) (requestChargePlan, error) {
	plan := requestChargePlan{}
	if fact.ClaimCount <= 0 || len(segments) == 0 {
		return plan, nil
	}

	totalClaimFacts, periodClaimFacts, ledgerAvailable, err := loadWorkerClaimFactsForBilling(
		ctx,
		tx,
		fact.WorkerID,
		fact.WorkerIncarnation,
		usageStartAt,
		usageEndAt,
		fact.TerminatedAt != nil && fact.TerminatedAt.UTC().Equal(usageEndAt),
	)
	if err != nil {
		return requestChargePlan{}, err
	}
	if ledgerAvailable && totalClaimFacts == fact.ClaimCount {
		plan.UseLedger = true
		plan.SegmentClaimCounts = make([]int64, len(segments))
		for _, claim := range periodClaimFacts {
			index, ok := findChargeSegmentForClaim(
				claim.ClaimedAt.UTC(),
				segments,
				fact.TerminatedAt != nil && fact.TerminatedAt.UTC().Equal(usageEndAt),
			)
			if !ok {
				return requestChargePlan{}, problem.New(
					500,
					"cost_accounting_request_claim_segment_missing",
					"The authoritative Worker claim ledger row did not match any cost tariff segment.",
				)
			}
			plan.SegmentClaimCounts[index]++
		}
		return plan, nil
	}

	fallbackCount, fallbackAuthorized, err := authoritativeRequestChargeClaimCount(
		fact,
		billingPeriodStartAt,
		billingPeriodEndAt,
		segments,
	)
	if err != nil {
		return requestChargePlan{}, err
	}
	plan.FallbackAuthorized = fallbackAuthorized
	plan.FallbackClaimCount = fallbackCount
	return plan, nil
}

func loadWorkerClaimFactsForBilling(
	ctx context.Context,
	tx *gorm.DB,
	workerID uuid.UUID,
	workerIncarnation int64,
	usageStartAt, usageEndAt time.Time,
	includeUsageEnd bool,
) (int64, []persistence.WorkerClaimFact, bool, error) {
	if !workerClaimFactsAvailableForBilling(tx) {
		return 0, nil, false, nil
	}

	var total int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerClaimFact{}).
		Where("worker_id = ? AND worker_incarnation = ?", workerID, workerIncarnation).
		Count(&total).Error; err != nil {
		return 0, nil, true, problem.Wrap(
			500,
			"cost_accounting_worker_claim_fact_count_failed",
			"The authoritative Worker claim ledger could not be counted.",
			err,
		)
	}

	claims := make([]persistence.WorkerClaimFact, 0)
	if !usageEndAt.After(usageStartAt) {
		return total, claims, true, nil
	}
	query := tx.WithContext(ctx).
		Where("worker_id = ? AND worker_incarnation = ?", workerID, workerIncarnation).
		Where("claimed_at >= ?", usageStartAt)
	if includeUsageEnd {
		query = query.Where("claimed_at <= ?", usageEndAt)
	} else {
		query = query.Where("claimed_at < ?", usageEndAt)
	}
	if err := query.Order("claimed_at ASC, id ASC").Find(&claims).Error; err != nil {
		return 0, nil, true, problem.Wrap(
			500,
			"cost_accounting_worker_claim_fact_load_failed",
			"The authoritative Worker claim ledger could not be loaded.",
			err,
		)
	}
	return total, claims, true, nil
}

func findChargeSegmentForClaim(
	claimedAt time.Time,
	segments []chargeSegment,
	includeFinalEnd bool,
) (int, bool) {
	for index, segment := range segments {
		if (claimedAt.Equal(segment.StartAt) || claimedAt.After(segment.StartAt)) && claimedAt.Before(segment.EndAt) {
			return index, true
		}
	}
	if includeFinalEnd && len(segments) > 0 && claimedAt.Equal(segments[len(segments)-1].EndAt) {
		return len(segments) - 1, true
	}
	return 0, false
}

func workerClaimFactsAvailableForBilling(tx *gorm.DB) bool {
	// Focused SQLite tests may create a reduced billing schema without the
	// authoritative claim ledger table.
	return tx != nil && (tx.Dialector.Name() != "sqlite" || tx.Migrator().HasTable(&persistence.WorkerClaimFact{}))
}

func loadWorkerFactForBilling(
	ctx context.Context,
	tx *gorm.DB,
	workerID uuid.UUID,
	workerIncarnation int64,
) (persistence.WorkerIncarnationFact, error) {
	var fact persistence.WorkerIncarnationFact
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ? AND worker_incarnation = ?", workerID, workerIncarnation).
		Take(&fact).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerIncarnationFact{}, problem.New(404, "cost_accounting_worker_fact_not_found", "The cost-accounting Worker incarnation fact was not found.")
	}
	if err != nil {
		return persistence.WorkerIncarnationFact{}, problem.Wrap(500, "cost_accounting_worker_fact_load_failed", "The cost-accounting Worker incarnation fact could not be loaded.", err)
	}
	return fact, nil
}

func loadActualInvoiceLines(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	importID uuid.UUID,
) ([]persistence.BillingActualInvoiceLine, error) {
	var lines []persistence.BillingActualInvoiceLine
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND invoice_import_id = ?", tenantID, importID).
		Order("external_line_id ASC").
		Find(&lines).Error; err != nil {
		return nil, problem.Wrap(500, "cost_accounting_invoice_line_load_failed", "The provider cost invoice lines could not be loaded.", err)
	}
	return lines, nil
}

func loadEstimateMatches(
	ctx context.Context,
	tx *gorm.DB,
	line persistence.BillingActualInvoiceLine,
) ([]persistence.BillingEstimatedUsageCharge, error) {
	var estimates []persistence.BillingEstimatedUsageCharge
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND provider = ? AND currency_code = ? AND charge_kind = ? AND resource_correlation_key = ?",
			line.TenantID, line.Provider, line.CurrencyCode, line.ChargeKind, line.ResourceCorrelationKey).
		Where("billing_period_start_at = ? AND billing_period_end_at = ?", line.BillingPeriodStartAt, line.BillingPeriodEndAt).
		Order("usage_start_at ASC, id ASC").
		Find(&estimates).Error; err != nil {
		return nil, problem.Wrap(500, "cost_accounting_reconciliation_load_failed", "The estimated usage costs could not be loaded for reconciliation.", err)
	}
	return estimates, nil
}

func loadUnmatchedEstimates(
	ctx context.Context,
	tx *gorm.DB,
	invoice persistence.BillingActualInvoiceImport,
	matched map[uuid.UUID]struct{},
) ([]persistence.BillingEstimatedUsageCharge, error) {
	var estimates []persistence.BillingEstimatedUsageCharge
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND provider = ? AND currency_code = ?", invoice.TenantID, invoice.Provider, invoice.CurrencyCode).
		Where("billing_period_start_at = ? AND billing_period_end_at = ?", invoice.BillingPeriodStartAt, invoice.BillingPeriodEndAt).
		Order("usage_start_at ASC, charge_kind ASC, id ASC").
		Find(&estimates).Error; err != nil {
		return nil, problem.Wrap(500, "cost_accounting_unmatched_estimate_load_failed", "The unmatched estimated usage costs could not be loaded.", err)
	}
	if len(matched) == 0 {
		return estimates, nil
	}
	filtered := make([]persistence.BillingEstimatedUsageCharge, 0, len(estimates))
	for _, estimate := range estimates {
		if _, found := matched[estimate.ID]; found {
			continue
		}
		filtered = append(filtered, estimate)
	}
	return filtered, nil
}

func normalizeTariffInput(input CreateTariffInput) (CreateTariffInput, error) {
	provider, err := normalizeProvider(input.Provider)
	if err != nil {
		return CreateTariffInput{}, err
	}
	region, err := normalizeRegion(input.Region)
	if err != nil {
		return CreateTariffInput{}, err
	}
	currency, err := normalizeCurrency(input.CurrencyCode)
	if err != nil {
		return CreateTariffInput{}, err
	}
	startAt, endAt, err := normalizeOptionalEndInterval(input.EffectiveStartAt, input.EffectiveEndAt)
	if err != nil {
		return CreateTariffInput{}, problem.New(400, "invalid_cost_accounting_tariff_effective_interval", "effectiveStartAt and effectiveEndAt must form a non-empty interval.")
	}
	if input.Version <= 0 {
		return CreateTariffInput{}, problem.New(400, "invalid_cost_accounting_tariff_version", "version must be positive.")
	}
	for _, field := range []struct {
		value int64
		name  string
	}{
		{value: input.CPUCoreHourRateMicros, name: "cpuCoreHourRateMicros"},
		{value: input.MemoryGiBHourRateMicros, name: "memoryGiBHourRateMicros"},
		{value: input.EphemeralGiBHourRateMicros, name: "ephemeralGiBHourRateMicros"},
		{value: input.RequestRateMicros, name: "requestRateMicros"},
		{value: input.PodHourRateMicros, name: "podHourRateMicros"},
	} {
		if field.value < 0 {
			return CreateTariffInput{}, problem.New(400, "invalid_cost_accounting_tariff_rate", field.name+" must be zero or positive.")
		}
	}
	input.Provider = provider
	input.Region = region
	input.CurrencyCode = currency
	input.EffectiveStartAt = startAt
	input.EffectiveEndAt = endAt
	return input, nil
}

func normalizeTariffListFilter(filter ListTariffsFilter) (ListTariffsFilter, error) {
	normalized := ListTariffsFilter{}
	if filter.Provider != nil {
		provider, err := normalizeProvider(*filter.Provider)
		if err != nil {
			return ListTariffsFilter{}, err
		}
		normalized.Provider = &provider
	}
	if filter.Region != nil {
		region, err := normalizeRegion(*filter.Region)
		if err != nil {
			return ListTariffsFilter{}, err
		}
		normalized.Region = &region
	}
	if filter.CurrencyCode != nil {
		currency, err := normalizeCurrency(*filter.CurrencyCode)
		if err != nil {
			return ListTariffsFilter{}, err
		}
		normalized.CurrencyCode = &currency
	}
	if filter.EffectiveAt != nil {
		effectiveAt := filter.EffectiveAt.UTC()
		normalized.EffectiveAt = &effectiveAt
	}
	return normalized, nil
}

func normalizeImportedInvoice(
	request ImportActualInvoiceRequest,
	imported ImportedActualInvoice,
) (ImportedActualInvoice, string, error) {
	externalImportID := strings.TrimSpace(imported.ExternalImportID)
	if externalImportID == "" {
		externalImportID = strings.TrimSpace(request.ExternalImportID)
	}
	if externalImportID == "" {
		return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_import", "The provider cost invoice import is missing externalImportId.")
	}
	periodStart, periodEnd, err := normalizeClosedPeriod(imported.BillingPeriodStartAt, imported.BillingPeriodEndAt)
	if err != nil {
		return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_period", "The provider cost invoice import period is invalid.")
	}
	currency, err := normalizeCurrency(imported.CurrencyCode)
	if err != nil {
		return ImportedActualInvoice{}, "", err
	}
	if imported.SourceProvenance != nil {
		checksum := strings.ToLower(strings.TrimSpace(imported.SourceProvenance.BundleChecksum))
		decoded, decodeErr := hex.DecodeString(checksum)
		if decodeErr != nil || len(decoded) != sha256.Size || imported.SourceProvenance.FilteredAdjustmentCount < 0 {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_source_provenance", "Cost source provenance is invalid.")
		}
		imported.SourceProvenance.BundleChecksum = checksum
	}

	lines := make([]ImportedActualInvoiceLine, 0, len(imported.Lines))
	resourceKeys := make(map[string]struct{}, len(imported.Lines))
	lineIDs := make(map[string]struct{}, len(imported.Lines))
	for _, rawLine := range imported.Lines {
		line := rawLine
		line.ExternalLineID = strings.TrimSpace(line.ExternalLineID)
		if line.ExternalLineID == "" {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_line", "Each provider cost invoice line must have externalLineId.")
		}
		if _, exists := lineIDs[line.ExternalLineID]; exists {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_line", "Provider cost invoice lines must not repeat externalLineId.")
		}
		lineIDs[line.ExternalLineID] = struct{}{}

		chargeKind, err := normalizeChargeKind(line.ChargeKind)
		if err != nil {
			return ImportedActualInvoice{}, "", err
		}
		line.ChargeKind = chargeKind
		line.ResourceCorrelationKey = normalizeResourceCorrelationKey(line.ResourceCorrelationKey)
		if line.ResourceCorrelationKey == "" {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_resource_correlation", "Provider cost invoice lines must include resourceCorrelationKey.")
		}
		if len(line.ResourceCorrelationKey) > 512 {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_resource_correlation", "resourceCorrelationKey must not exceed 512 characters.")
		}
		if line.BillingPeriodStartAt.IsZero() && line.BillingPeriodEndAt.IsZero() {
			line.BillingPeriodStartAt = periodStart
			line.BillingPeriodEndAt = periodEnd
		}
		lineStart, lineEnd, err := normalizeClosedPeriod(line.BillingPeriodStartAt, line.BillingPeriodEndAt)
		if err != nil {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_line_period", "The provider cost invoice line period is invalid.")
		}
		if !lineStart.Equal(periodStart) || !lineEnd.Equal(periodEnd) {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_line_period", "Provider cost invoice line periods must match the import period exactly.")
		}
		line.BillingPeriodStartAt = lineStart
		line.BillingPeriodEndAt = lineEnd
		if strings.TrimSpace(line.CurrencyCode) == "" {
			line.CurrencyCode = currency
		}
		lineCurrency, err := normalizeCurrency(line.CurrencyCode)
		if err != nil {
			return ImportedActualInvoice{}, "", err
		}
		if lineCurrency != currency {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_line_currency", "Provider cost invoice line currency must match the import currency.")
		}
		line.CurrencyCode = lineCurrency

		deduplicationKey := line.ResourceCorrelationKey + "\x00" + line.ChargeKind + "\x00" + line.CurrencyCode
		if _, exists := resourceKeys[deduplicationKey]; exists {
			return ImportedActualInvoice{}, "", problem.New(400, "invalid_cost_accounting_invoice_line", "Provider cost invoice lines must not repeat resourceCorrelationKey and chargeKind inside one import.")
		}
		resourceKeys[deduplicationKey] = struct{}{}
		lines = append(lines, line)
	}

	sort.Slice(lines, func(i, j int) bool { return lines[i].ExternalLineID < lines[j].ExternalLineID })
	normalized := ImportedActualInvoice{
		ExternalImportID:     externalImportID,
		BillingPeriodStartAt: periodStart,
		BillingPeriodEndAt:   periodEnd,
		CurrencyCode:         currency,
		Lines:                lines,
		SourceProvenance:     cloneInvoiceSourceProvenance(imported.SourceProvenance),
	}
	return normalized, checksumImportedInvoice(request.Provider, normalized), nil
}

func checksumImportedInvoice(provider string, imported ImportedActualInvoice) string {
	hasher := sha256.New()
	writeChecksumPart(hasher, strings.ToLower(strings.TrimSpace(provider)))
	writeChecksumPart(hasher, imported.ExternalImportID)
	writeChecksumPart(hasher, imported.BillingPeriodStartAt.UTC().Format(time.RFC3339Nano))
	writeChecksumPart(hasher, imported.BillingPeriodEndAt.UTC().Format(time.RFC3339Nano))
	writeChecksumPart(hasher, imported.CurrencyCode)
	if imported.SourceProvenance != nil {
		writeChecksumPart(hasher, imported.SourceProvenance.BundleChecksum)
		writeChecksumPart(hasher, fmt.Sprintf("%d", imported.SourceProvenance.FilteredAdjustmentCount))
		writeChecksumPart(hasher, fmt.Sprintf("%d", imported.SourceProvenance.FilteredAdjustmentAmountMicros))
	}
	for _, line := range imported.Lines {
		writeChecksumPart(hasher, line.ExternalLineID)
		writeChecksumPart(hasher, line.ChargeKind)
		writeChecksumPart(hasher, line.ResourceCorrelationKey)
		writeChecksumPart(hasher, line.BillingPeriodStartAt.UTC().Format(time.RFC3339Nano))
		writeChecksumPart(hasher, line.BillingPeriodEndAt.UTC().Format(time.RFC3339Nano))
		writeChecksumPart(hasher, line.CurrencyCode)
		writeChecksumPart(hasher, fmt.Sprintf("%d", line.AmountMicros))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func cloneInvoiceSourceProvenance(source *InvoiceSourceProvenance) *InvoiceSourceProvenance {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Chunks = append([]InvoiceSourceObject(nil), source.Chunks...)
	return &cloned
}

func writeChecksumPart(hasher interface{ Write([]byte) (int, error) }, value string) {
	_, _ = hasher.Write([]byte(value))
	_, _ = hasher.Write([]byte{0})
}

func billingTariffOverlaps(
	ctx context.Context,
	tx *gorm.DB,
	candidate persistence.BillingProviderTariff,
	excludeID *uuid.UUID,
) (bool, error) {
	query := tx.WithContext(ctx).
		Model(&persistence.BillingProviderTariff{}).
		Where("provider = ? AND region = ? AND currency_code = ?",
			candidate.Provider, candidate.Region, candidate.CurrencyCode).
		Where("(effective_end_at IS NULL OR effective_end_at > ?)", candidate.EffectiveStartAt)
	if candidate.EffectiveEndAt != nil {
		query = query.Where("effective_start_at < ?", candidate.EffectiveEndAt.UTC())
	}
	if excludeID != nil {
		query = query.Where("id <> ?", *excludeID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, problem.Wrap(500, "cost_accounting_tariff_overlap_probe_failed", "The cost tariff overlap check could not be completed.", err)
	}
	return count > 0, nil
}

func deterministicTariffID(input CreateTariffInput) uuid.UUID {
	name := strings.Join([]string{
		input.Provider,
		input.Region,
		input.CurrencyCode,
		fmt.Sprintf("%d", input.Version),
		input.EffectiveStartAt.UTC().Format(time.RFC3339Nano),
		nullableTimeString(input.EffectiveEndAt),
	}, "\x00")
	return uuid.NewSHA1(deterministicNamespace, []byte(name))
}

func deterministicEstimatedChargeID(row persistence.BillingEstimatedUsageCharge) uuid.UUID {
	name := strings.Join([]string{
		nullableUUIDString(row.TenantID),
		row.WorkerID.String(),
		fmt.Sprintf("%d", row.WorkerIncarnation),
		row.TariffID.String(),
		row.ChargeKind,
		row.BillingPeriodStartAt.UTC().Format(time.RFC3339Nano),
		row.BillingPeriodEndAt.UTC().Format(time.RFC3339Nano),
		row.UsageStartAt.UTC().Format(time.RFC3339Nano),
		row.UsageEndAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")
	return uuid.NewSHA1(deterministicNamespace, []byte(name))
}

func deterministicImportID(tenantID uuid.UUID, provider, externalImportID string) uuid.UUID {
	return uuid.NewSHA1(deterministicNamespace, []byte(strings.Join([]string{
		tenantID.String(),
		strings.ToLower(strings.TrimSpace(provider)),
		strings.TrimSpace(externalImportID),
	}, "\x00")))
}

func deterministicLineID(tenantID uuid.UUID, importID uuid.UUID, externalLineID string) uuid.UUID {
	return uuid.NewSHA1(deterministicNamespace, []byte(strings.Join([]string{
		tenantID.String(),
		importID.String(),
		strings.TrimSpace(externalLineID),
	}, "\x00")))
}

func normalizeBillingTenantID(tenantID uuid.UUID) (uuid.UUID, error) {
	if tenantID == uuid.Nil {
		return uuid.Nil, problem.New(400, "invalid_cost_accounting_tenant", "tenantId is required.")
	}
	return tenantID, nil
}

func normalizeProvider(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || len(normalized) > 64 {
		return "", problem.New(400, "invalid_cost_accounting_provider", "provider must be between 1 and 64 characters.")
	}
	for _, character := range normalized {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return "", problem.New(400, "invalid_cost_accounting_provider", "provider must use lowercase letters, digits, hyphen, or underscore.")
		}
	}
	return normalized, nil
}

func normalizeCurrency(value string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if len(normalized) != 3 {
		return "", problem.New(400, "invalid_cost_accounting_currency", "currencyCode must be a 3-letter ISO currency code.")
	}
	for _, character := range normalized {
		if character < 'A' || character > 'Z' {
			return "", problem.New(400, "invalid_cost_accounting_currency", "currencyCode must be a 3-letter ISO currency code.")
		}
	}
	return normalized, nil
}

func normalizeChargeKind(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case ChargeKindCPU, ChargeKindMemory, ChargeKindEphemeralStorage, ChargeKindRequest, ChargeKindPod:
		return strings.TrimSpace(value), nil
	default:
		return "", problem.New(400, "invalid_cost_accounting_charge_kind", "chargeKind must be one of cpu, memory, ephemeral-storage, request, or pod.")
	}
}

func normalizeRegion(value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if len(normalized) > 160 {
		return "", problem.New(400, "invalid_cost_accounting_region", "region must be 160 characters or fewer.")
	}
	return normalized, nil
}

func normalizeResourceCorrelationKey(value string) string {
	return strings.TrimSpace(value)
}

func normalizeOptionalEndInterval(startAt time.Time, endAt *time.Time) (time.Time, *time.Time, error) {
	if startAt.IsZero() {
		return time.Time{}, nil, problem.New(400, "invalid_cost_accounting_period", "startAt is required.")
	}
	start := startAt.UTC()
	if endAt == nil {
		return start, nil, nil
	}
	end := endAt.UTC()
	if !end.After(start) {
		return time.Time{}, nil, problem.New(400, "invalid_cost_accounting_period", "endAt must be after startAt.")
	}
	return start, &end, nil
}

func normalizeClosedPeriod(startAt, endAt time.Time) (time.Time, time.Time, error) {
	if startAt.IsZero() || endAt.IsZero() {
		return time.Time{}, time.Time{}, problem.New(400, "invalid_cost_accounting_period", "billingPeriodStartAt and billingPeriodEndAt are required.")
	}
	start := startAt.UTC()
	end := endAt.UTC()
	if !end.After(start) {
		return time.Time{}, time.Time{}, problem.New(400, "invalid_cost_accounting_period", "billingPeriodEndAt must be after billingPeriodStartAt.")
	}
	return start, end, nil
}

func nullableTimeString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func nullableUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func chargeKindOrder(value string) int {
	switch value {
	case ChargeKindCPU:
		return 1
	case ChargeKindMemory:
		return 2
	case ChargeKindEphemeralStorage:
		return 3
	case ChargeKindRequest:
		return 4
	case ChargeKindPod:
		return 5
	default:
		return 99
	}
}

func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func dedupeTimes(values []time.Time) []time.Time {
	if len(values) <= 1 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value.Equal(result[len(result)-1]) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func workerResourceCorrelationKey(fact persistence.WorkerIncarnationFact) string {
	parts := []string{
		normalizeCorrelationPart(fact.TargetKind),
		normalizeCorrelationPart(fact.ClusterID),
		normalizeCorrelationPart(fact.Region),
		normalizeCorrelationPart(fact.Namespace),
		normalizeCorrelationPart(fact.PodName),
		normalizeCorrelationPart(strings.ToLower(fact.InstanceUID)),
	}
	return strings.Join(parts, ":")
}

func normalizeCorrelationPart(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "-"
	}
	return trimmed
}

func wholeSecondsBetween(startAt, endAt time.Time) int64 {
	if !endAt.After(startAt) {
		return 0
	}
	return int64(endAt.Sub(startAt) / time.Second)
}

func cloneOptionalInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func multiplyDivideRound(parts []int64, denominator int64) (int64, error) {
	if denominator <= 0 {
		return 0, problem.New(500, "cost_accounting_amount_denominator_invalid", "The cost amount denominator is invalid.")
	}
	numerator := big.NewInt(1)
	for _, part := range parts {
		numerator.Mul(numerator, big.NewInt(part))
	}
	sign := numerator.Sign()
	if sign < 0 {
		numerator.Neg(numerator)
	}
	denominatorBig := big.NewInt(denominator)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominatorBig, remainder)
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(denominatorBig) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if sign < 0 {
		quotient.Neg(quotient)
	}
	if !quotient.IsInt64() {
		return 0, problem.New(409, "cost_accounting_amount_overflow", "The cost amount could not be represented safely.")
	}
	return quotient.Int64(), nil
}

func addInt64Checked(left, right int64) (int64, error) {
	sum := big.NewInt(left)
	sum.Add(sum, big.NewInt(right))
	if !sum.IsInt64() {
		return 0, problem.New(409, "cost_accounting_amount_overflow", "The cost amount could not be represented safely.")
	}
	return sum.Int64(), nil
}

func (s *Service) createTariffTx(
	ctx context.Context,
	tx *gorm.DB,
	input CreateTariffInput,
) (persistence.BillingProviderTariff, error) {
	model := persistence.BillingProviderTariff{
		ID:                         deterministicTariffID(input),
		Provider:                   input.Provider,
		Region:                     input.Region,
		CurrencyCode:               input.CurrencyCode,
		Version:                    input.Version,
		EffectiveStartAt:           input.EffectiveStartAt,
		EffectiveEndAt:             input.EffectiveEndAt,
		CPUCoreHourRateMicros:      input.CPUCoreHourRateMicros,
		MemoryGiBHourRateMicros:    input.MemoryGiBHourRateMicros,
		EphemeralGiBHourRateMicros: input.EphemeralGiBHourRateMicros,
		RequestRateMicros:          input.RequestRateMicros,
		PodHourRateMicros:          input.PodHourRateMicros,
		CreatedAt:                  s.now().UTC(),
	}
	lockKey := strings.Join(
		[]string{"synara:billing-provider-tariff", model.Provider, model.Region, model.CurrencyCode},
		"\x1f",
	)
	locked, err := persistence.TryTransactionAdvisoryLock(ctx, tx, lockKey)
	if err != nil {
		return persistence.BillingProviderTariff{}, problem.Wrap(
			500,
			"cost_accounting_tariff_coordination_failed",
			"The cost tariff catalog could not be coordinated.",
			err,
		)
	}
	if !locked {
		return persistence.BillingProviderTariff{}, problem.New(
			409,
			"cost_accounting_tariff_catalog_busy",
			"Another cost tariff mutation is in progress for this provider, region, and currency.",
		)
	}

	conflict, err := loadTariffByVersion(ctx, tx, model.Provider, model.Region, model.CurrencyCode, model.Version)
	if err != nil {
		return persistence.BillingProviderTariff{}, err
	}
	if conflict != nil {
		return persistence.BillingProviderTariff{}, problem.New(
			409,
			"cost_accounting_tariff_version_conflict",
			"The cost tariff version already exists for this provider, region, and currency.",
		)
	}
	overlaps, overlapErr := billingTariffOverlaps(ctx, tx, model, nil)
	if overlapErr != nil {
		return persistence.BillingProviderTariff{}, overlapErr
	}
	if overlaps {
		return persistence.BillingProviderTariff{}, problem.New(409, "cost_accounting_tariff_overlap", "The cost tariff overlaps an existing effective interval.")
	}
	if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return persistence.BillingProviderTariff{}, problem.New(
				409,
				"cost_accounting_tariff_version_conflict",
				"The cost tariff version already exists for this provider, region, and currency.",
			)
		}
		return persistence.BillingProviderTariff{}, problem.Wrap(409, "cost_accounting_tariff_create_failed", "The cost tariff could not be created.", err)
	}
	return model, nil
}

func loadTariffs(
	ctx context.Context,
	db *gorm.DB,
	filter ListTariffsFilter,
) ([]persistence.BillingProviderTariff, error) {
	query := db.WithContext(ctx).Model(&persistence.BillingProviderTariff{})
	if filter.Provider != nil {
		query = query.Where("provider = ?", *filter.Provider)
	}
	if filter.Region != nil {
		query = query.Where("region = ?", *filter.Region)
	}
	if filter.CurrencyCode != nil {
		query = query.Where("currency_code = ?", *filter.CurrencyCode)
	}
	if filter.EffectiveAt != nil {
		query = query.
			Where("effective_start_at <= ?", *filter.EffectiveAt).
			Where("(effective_end_at IS NULL OR effective_end_at > ?)", *filter.EffectiveAt)
	}
	var tariffs []persistence.BillingProviderTariff
	if err := query.
		Order("provider ASC, region ASC, currency_code ASC, effective_start_at ASC, version ASC").
		Find(&tariffs).Error; err != nil {
		return nil, problem.Wrap(500, "cost_accounting_tariff_load_failed", "The cost tariffs could not be loaded.", err)
	}
	return tariffs, nil
}

func loadTariffByVersion(
	ctx context.Context,
	tx *gorm.DB,
	provider, region, currency string,
	version int64,
) (*persistence.BillingProviderTariff, error) {
	var item persistence.BillingProviderTariff
	err := tx.WithContext(ctx).
		Where("provider = ? AND region = ? AND currency_code = ? AND version = ?", provider, region, currency, version).
		Take(&item).Error
	switch {
	case err == nil:
		return &item, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil
	default:
		return nil, problem.Wrap(500, "cost_accounting_tariff_load_failed", "The cost tariffs could not be loaded.", err)
	}
}

func viewTariff(model persistence.BillingProviderTariff) Tariff {
	return Tariff{
		ID:                         model.ID,
		Provider:                   model.Provider,
		Region:                     model.Region,
		CurrencyCode:               model.CurrencyCode,
		Version:                    model.Version,
		EffectiveStartAt:           model.EffectiveStartAt,
		EffectiveEndAt:             model.EffectiveEndAt,
		CPUCoreHourRateMicros:      model.CPUCoreHourRateMicros,
		MemoryGiBHourRateMicros:    model.MemoryGiBHourRateMicros,
		EphemeralGiBHourRateMicros: model.EphemeralGiBHourRateMicros,
		RequestRateMicros:          model.RequestRateMicros,
		PodHourRateMicros:          model.PodHourRateMicros,
		CreatedAt:                  model.CreatedAt,
	}
}

func viewTariffs(models []persistence.BillingProviderTariff) []Tariff {
	items := make([]Tariff, 0, len(models))
	for _, model := range models {
		items = append(items, viewTariff(model))
	}
	return items
}

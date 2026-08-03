package billing

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// Tariffs are a shared platform-wide catalog today. The tenant boundary here
// authorizes who may manage that catalog; it does not change row ownership.
func (s *Service) ListTariffsAuthorized(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	filter ListTariffsFilter,
) ([]Tariff, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CostManage); err != nil {
		return nil, err
	}
	items, err := s.ListTariffs(ctx, filter)
	if err != nil {
		return nil, err
	}
	return viewTariffs(items), nil
}

func (s *Service) CreateTariffAuthorized(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateTariffInput,
	requestID, ipAddress string,
) (Tariff, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Tariff{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CostManage); err != nil {
		return Tariff{}, err
	}
	if s.platformBillingOperatorTenantID == uuid.Nil {
		return Tariff{}, problem.New(
			503,
			"cost_accounting_tariff_management_unavailable",
			"Cost tariff management requires an explicitly configured platform operator Tenant.",
		)
	}
	if tenantID != s.platformBillingOperatorTenantID {
		return Tariff{}, problem.New(
			403,
			"cost_accounting_tariff_operator_forbidden",
			"This Tenant is not the configured platform cost-accounting operator.",
		)
	}
	normalized, err := normalizeTariffInput(input)
	if err != nil {
		return Tariff{}, err
	}

	created := persistence.BillingProviderTariff{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var createErr error
		created, createErr = s.createTariffTx(ctx, tx, normalized)
		if createErr != nil {
			return createErr
		}
		return s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID, Action: "cost_accounting.tariff_created",
			ResourceType: "cost_accounting_provider_tariff", ResourceID: &created.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"provider": created.Provider, "region": created.Region, "currencyCode": created.CurrencyCode,
				"version": created.Version, "effectiveStartAt": created.EffectiveStartAt, "effectiveEndAt": created.EffectiveEndAt,
			},
		})
	})
	if err != nil {
		return Tariff{}, err
	}
	return viewTariff(created), nil
}

func (s *Service) ImportConfiguredInvoiceAuthorized(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	provider string,
	externalImportID string,
	requestID, ipAddress string,
) (ConfiguredInvoiceImportResult, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ConfiguredInvoiceImportResult{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CostManage); err != nil {
		return ConfiguredInvoiceImportResult{}, err
	}
	configuredImport, err := s.requireConfiguredImport(tenantID, provider, externalImportID)
	if err != nil {
		return ConfiguredInvoiceImportResult{}, err
	}
	result, err := s.importConfiguredInvoiceWithAudit(ctx, configuredImport, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID, Action: "cost_accounting.invoice_import_triggered",
		RequestID: requestID, IPAddress: ipAddress,
	}, false)
	if err != nil {
		return ConfiguredInvoiceImportResult{}, err
	}
	response := ConfiguredInvoiceImportResult{
		Import: result.Result.Import,
		Lines:  result.Result.Lines,
	}
	if configuredImport.EstimateAfterImport {
		sweepResult, sweepErr := s.runEstimateSweep(
			ctx,
			configuredImport,
			result.Result.Import,
			"billing-import-api:"+principal.UserID.String(),
		)
		response.EstimateSweep = configuredEstimateSweepOutcome(sweepResult, sweepErr)
		if sweepErr != nil {
			// The invoice import and its audit entry are already committed. Return
			// that durable identity with an explicit retry state instead of
			// misrepresenting the post-commit sweep failure as an import rollback.
			return response, nil
		}
	}
	return response, nil
}

type ConfiguredInvoiceImportResult struct {
	Import        persistence.BillingActualInvoiceImport
	Lines         []persistence.BillingActualInvoiceLine
	EstimateSweep *ConfiguredEstimateSweepOutcome `json:",omitempty"`
}

type ConfiguredEstimateSweepOutcome struct {
	Status            string
	WorkerCount       int
	EstimateCount     int
	FailedWorkerCount int
	ErrorCode         string `json:",omitempty"`
	ErrorMessage      string `json:",omitempty"`
}

func configuredEstimateSweepOutcome(
	result ScheduledEstimateSweepResult,
	err error,
) *ConfiguredEstimateSweepOutcome {
	outcome := &ConfiguredEstimateSweepOutcome{
		Status:            "completed",
		WorkerCount:       result.WorkerCount,
		EstimateCount:     result.EstimateCount,
		FailedWorkerCount: result.FailedWorkerCount,
	}
	if err == nil {
		return outcome
	}
	outcome.Status = "retry-required"
	outcome.ErrorCode = "cost_accounting_estimate_sweep_failed"
	outcome.ErrorMessage = "The cost estimate sweep did not complete and must be retried."
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		outcome.ErrorCode = apiError.Code
		outcome.ErrorMessage = apiError.Message
	}
	return outcome
}

func (s *Service) ReconcileActualInvoiceImportAuthorized(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	importID uuid.UUID,
	requestID, ipAddress string,
) (ReconciliationReport, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ReconciliationReport{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CostManage); err != nil {
		return ReconciliationReport{}, err
	}
	report, err := s.reconcileActualInvoiceImportWithAudit(ctx, tenantID, importID, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID, Action: "cost_accounting.invoice_reconciled",
		RequestID: requestID, IPAddress: ipAddress,
	}, false)
	if err != nil {
		return ReconciliationReport{}, err
	}
	return report.Report, nil
}

func (s *Service) importConfiguredInvoiceScheduled(
	ctx context.Context,
	configuredImport ConfiguredImport,
	requestID string,
) (importedInvoiceMutation, error) {
	return s.importConfiguredInvoiceWithAudit(ctx, configuredImport, audit.Entry{
		TenantID: configuredImport.TenantID, ActorType: "system", Action: "cost_accounting.invoice_import_scheduled",
		RequestID: requestID,
	}, true)
}

func (s *Service) reconcileActualInvoiceImportScheduled(
	ctx context.Context,
	configuredImport ConfiguredImport,
	importID uuid.UUID,
	requestID string,
) (reconciliationMutation, error) {
	return s.reconcileActualInvoiceImportWithAudit(ctx, configuredImport.TenantID, importID, audit.Entry{
		TenantID: configuredImport.TenantID, ActorType: "system", Action: "cost_accounting.invoice_reconciled_scheduled",
		RequestID: requestID,
	}, true)
}

func (s *Service) importConfiguredInvoiceWithAudit(
	ctx context.Context,
	configuredImport ConfiguredImport,
	entry audit.Entry,
	auditOnMutationOnly bool,
) (importedInvoiceMutation, error) {
	request := ImportActualInvoiceRequest{
		TenantID: configuredImport.TenantID, Provider: configuredImport.Provider, ExternalImportID: configuredImport.ExternalImportID,
	}
	normalizedRequest, normalizedInvoice, checksum, err := s.prepareActualInvoiceImport(ctx, request)
	if err != nil {
		return importedInvoiceMutation{}, err
	}

	result := importedInvoiceMutation{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var importErr error
		result, importErr = s.importActualInvoiceTx(ctx, tx, normalizedRequest, normalizedInvoice, checksum)
		if importErr != nil {
			return importErr
		}
		if auditOnMutationOnly && !result.Created {
			return nil
		}
		return s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID: entry.TenantID, ActorType: entry.ActorType, ActorID: entry.ActorID, Action: entry.Action,
			ResourceType: "cost_accounting_actual_invoice_import", ResourceID: &result.Result.Import.ID,
			RequestID: entry.RequestID, IPAddress: entry.IPAddress,
			Metadata: map[string]any{
				"provider": result.Result.Import.Provider, "externalImportId": result.Result.Import.ExternalImportID,
				"billingPeriodStartAt": result.Result.Import.BillingPeriodStartAt, "billingPeriodEndAt": result.Result.Import.BillingPeriodEndAt,
				"currencyCode": result.Result.Import.CurrencyCode, "lineCount": len(result.Result.Lines),
				"sourceProvenance": result.Result.SourceProvenance,
			},
		})
	})
	if err != nil {
		return importedInvoiceMutation{}, err
	}
	return result, nil
}

func (s *Service) reconcileActualInvoiceImportWithAudit(
	ctx context.Context,
	tenantID uuid.UUID,
	importID uuid.UUID,
	entry audit.Entry,
	auditOnMutationOnly bool,
) (reconciliationMutation, error) {
	report := reconciliationMutation{}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var reconcileErr error
		report, reconcileErr = s.reconcileActualInvoiceImportTx(ctx, tx, tenantID, importID)
		if reconcileErr != nil {
			return reconcileErr
		}
		if auditOnMutationOnly && !report.Changed {
			return nil
		}
		return s.recordAuditTx(ctx, tx, audit.Entry{
			TenantID: entry.TenantID, ActorType: entry.ActorType, ActorID: entry.ActorID, Action: entry.Action,
			ResourceType: "cost_accounting_actual_invoice_import", ResourceID: &report.Report.Import.ID,
			RequestID: entry.RequestID, IPAddress: entry.IPAddress,
			Metadata: map[string]any{
				"provider": report.Report.Import.Provider, "externalImportId": report.Report.Import.ExternalImportID,
				"unmatchedEstimateCount":        len(report.Report.UnmatchedEstimateIDs),
				"unmatchedEstimateAmountMicros": report.Report.UnmatchedEstimateAmountMicros,
			},
		})
	})
	if err != nil {
		return reconciliationMutation{}, err
	}
	return report, nil
}

func (s *Service) requireConfiguredImport(
	tenantID uuid.UUID,
	provider string,
	externalImportID string,
) (ConfiguredImport, error) {
	if len(s.configuredImports) == 0 {
		return ConfiguredImport{}, problem.New(404, "cost_accounting_import_not_configured", "The provider cost invoice import is not configured.")
	}
	normalizedProvider, err := normalizeProvider(provider)
	if err != nil {
		return ConfiguredImport{}, err
	}
	key := configuredImportKey(tenantID, normalizedProvider, strings.TrimSpace(externalImportID))
	configuredImport, ok := s.configuredImports[key]
	if !ok {
		return ConfiguredImport{}, problem.New(404, "cost_accounting_import_not_configured", "The provider cost invoice import is not configured.")
	}
	return configuredImport, nil
}

func (s *Service) recordAuditTx(ctx context.Context, tx *gorm.DB, entry audit.Entry) error {
	if s.auditRecorder == nil {
		return audit.Record(ctx, tx, entry)
	}
	return s.auditRecorder(ctx, tx, entry)
}

type EstimateSweeper interface {
	SweepImportedInvoice(context.Context, ScheduledEstimateSweepRequest) (ScheduledEstimateSweepResult, error)
}

type ScheduledEstimateSweepRequest struct {
	Import             persistence.BillingActualInvoiceImport
	TenantID           uuid.UUID
	Provider           string
	ExecutionTargetIDs []uuid.UUID
	RequestedBy        string
}

type ScheduledEstimateSweepResult struct {
	WorkerCount       int
	EstimateCount     int
	FailedWorkerCount int
}

func (s *Service) runEstimateSweep(
	ctx context.Context,
	configuredImport ConfiguredImport,
	imported persistence.BillingActualInvoiceImport,
	requestedBy string,
) (ScheduledEstimateSweepResult, error) {
	if !configuredImport.EstimateAfterImport {
		return ScheduledEstimateSweepResult{}, nil
	}
	if s.estimateSweeper == nil {
		return ScheduledEstimateSweepResult{}, problem.New(
			500,
			"cost_accounting_estimate_sweeper_unavailable",
			"The configured cost estimate sweep is not available in this Control Plane build.",
		)
	}
	return s.estimateSweeper.SweepImportedInvoice(ctx, ScheduledEstimateSweepRequest{
		Import: imported, TenantID: configuredImport.TenantID, Provider: configuredImport.Provider,
		ExecutionTargetIDs: append([]uuid.UUID(nil), configuredImport.ExecutionTargetIDs...),
		RequestedBy:        requestedBy,
	})
}

package persistence

import (
	"time"

	"github.com/google/uuid"
)

type BillingProviderTariff struct {
	ID                         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Provider                   string     `gorm:"column:provider;uniqueIndex:uq_billing_provider_tariffs_version,priority:1;index:idx_billing_provider_tariffs_lookup,priority:1"`
	Region                     string     `gorm:"column:region;uniqueIndex:uq_billing_provider_tariffs_version,priority:2;index:idx_billing_provider_tariffs_lookup,priority:2"`
	CurrencyCode               string     `gorm:"column:currency_code;uniqueIndex:uq_billing_provider_tariffs_version,priority:3;index:idx_billing_provider_tariffs_lookup,priority:3"`
	Version                    int64      `gorm:"column:version;uniqueIndex:uq_billing_provider_tariffs_version,priority:4"`
	EffectiveStartAt           time.Time  `gorm:"column:effective_start_at;index:idx_billing_provider_tariffs_lookup,priority:4"`
	EffectiveEndAt             *time.Time `gorm:"column:effective_end_at"`
	CPUCoreHourRateMicros      int64      `gorm:"column:cpu_core_hour_rate_micros"`
	MemoryGiBHourRateMicros    int64      `gorm:"column:memory_gib_hour_rate_micros"`
	EphemeralGiBHourRateMicros int64      `gorm:"column:ephemeral_gib_hour_rate_micros"`
	RequestRateMicros          int64      `gorm:"column:request_rate_micros"`
	PodHourRateMicros          int64      `gorm:"column:pod_hour_rate_micros"`
	CreatedAt                  time.Time  `gorm:"column:created_at"`
}

func (BillingProviderTariff) TableName() string { return "billing_provider_tariffs" }

type BillingEstimatedUsageCharge struct {
	ID                             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	TenantID                       *uuid.UUID `gorm:"column:tenant_id;type:uuid;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:1;index:idx_billing_estimated_usage_charges_reconcile,priority:1"`
	ExecutionTargetID              uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind                     string     `gorm:"column:target_kind"`
	WorkerID                       uuid.UUID  `gorm:"column:worker_id;type:uuid;index:idx_billing_estimated_usage_charges_worker,priority:1"`
	WorkerIncarnation              int64      `gorm:"column:worker_incarnation;index:idx_billing_estimated_usage_charges_worker,priority:2"`
	TariffID                       uuid.UUID  `gorm:"column:tariff_id;type:uuid;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:4;index"`
	Provider                       string     `gorm:"column:provider;index:idx_billing_estimated_usage_charges_reconcile,priority:2"`
	Region                         string     `gorm:"column:region"`
	CurrencyCode                   string     `gorm:"column:currency_code;index:idx_billing_estimated_usage_charges_reconcile,priority:3"`
	ChargeKind                     string     `gorm:"column:charge_kind;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:5;index:idx_billing_estimated_usage_charges_reconcile,priority:5"`
	ResourceCorrelationKey         string     `gorm:"column:resource_correlation_key;index:idx_billing_estimated_usage_charges_reconcile,priority:4"`
	BillingPeriodStartAt           time.Time  `gorm:"column:billing_period_start_at;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:6;index:idx_billing_estimated_usage_charges_reconcile,priority:6"`
	BillingPeriodEndAt             time.Time  `gorm:"column:billing_period_end_at;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:7;index:idx_billing_estimated_usage_charges_reconcile,priority:7"`
	UsageStartAt                   time.Time  `gorm:"column:usage_start_at;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:8"`
	UsageEndAt                     time.Time  `gorm:"column:usage_end_at;uniqueIndex:uq_billing_estimated_usage_charges_identity,priority:9"`
	BillableSeconds                int64      `gorm:"column:billable_seconds"`
	ClaimCount                     int64      `gorm:"column:claim_count"`
	RequestedCPUMillicores         *int64     `gorm:"column:requested_cpu_millicores"`
	RequestedMemoryBytes           *int64     `gorm:"column:requested_memory_bytes"`
	RequestedEphemeralStorageBytes *int64     `gorm:"column:requested_ephemeral_storage_bytes"`
	RateMicros                     int64      `gorm:"column:rate_micros"`
	AmountMicros                   int64      `gorm:"column:amount_micros"`
	CreatedAt                      time.Time  `gorm:"column:created_at"`
}

func (BillingEstimatedUsageCharge) TableName() string { return "billing_estimated_usage_charges" }

type BillingActualInvoiceImport struct {
	ID                   uuid.UUID `gorm:"column:id;type:uuid;primaryKey;uniqueIndex:uq_billing_actual_invoice_imports_tenant_id,priority:2"`
	TenantID             uuid.UUID `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_billing_actual_invoice_imports_external,priority:1;uniqueIndex:uq_billing_actual_invoice_imports_tenant_id,priority:1;index:idx_billing_actual_invoice_imports_period,priority:1"`
	Provider             string    `gorm:"column:provider;uniqueIndex:uq_billing_actual_invoice_imports_external,priority:2;index:idx_billing_actual_invoice_imports_period,priority:2"`
	ExternalImportID     string    `gorm:"column:external_import_id;uniqueIndex:uq_billing_actual_invoice_imports_external,priority:3"`
	BillingPeriodStartAt time.Time `gorm:"column:billing_period_start_at;index:idx_billing_actual_invoice_imports_period,priority:3"`
	BillingPeriodEndAt   time.Time `gorm:"column:billing_period_end_at;index:idx_billing_actual_invoice_imports_period,priority:4"`
	CurrencyCode         string    `gorm:"column:currency_code"`
	SourceChecksum       string    `gorm:"column:source_checksum"`
	ImportedAt           time.Time `gorm:"column:imported_at"`
	CreatedAt            time.Time `gorm:"column:created_at"`
}

func (BillingActualInvoiceImport) TableName() string { return "billing_actual_invoice_imports" }

type BillingActualInvoiceLine struct {
	ID                          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey;uniqueIndex:uq_billing_actual_invoice_lines_tenant_id,priority:2"`
	TenantID                    uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null;uniqueIndex:uq_billing_actual_invoice_lines_external,priority:1;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:1;uniqueIndex:uq_billing_actual_invoice_lines_tenant_id,priority:1;index:idx_billing_actual_invoice_lines_reconcile,priority:1"`
	InvoiceImportID             uuid.UUID  `gorm:"column:invoice_import_id;type:uuid;uniqueIndex:uq_billing_actual_invoice_lines_external,priority:2;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:2"`
	ExternalLineID              string     `gorm:"column:external_line_id;uniqueIndex:uq_billing_actual_invoice_lines_external,priority:3"`
	Provider                    string     `gorm:"column:provider;index:idx_billing_actual_invoice_lines_reconcile,priority:2"`
	CurrencyCode                string     `gorm:"column:currency_code;index:idx_billing_actual_invoice_lines_reconcile,priority:3;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:7"`
	ChargeKind                  string     `gorm:"column:charge_kind;index:idx_billing_actual_invoice_lines_reconcile,priority:5;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:4"`
	ResourceCorrelationKey      string     `gorm:"column:resource_correlation_key;index:idx_billing_actual_invoice_lines_reconcile,priority:4;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:3"`
	BillingPeriodStartAt        time.Time  `gorm:"column:billing_period_start_at;index:idx_billing_actual_invoice_lines_reconcile,priority:6;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:5"`
	BillingPeriodEndAt          time.Time  `gorm:"column:billing_period_end_at;index:idx_billing_actual_invoice_lines_reconcile,priority:7;uniqueIndex:uq_billing_actual_invoice_lines_resource,priority:6"`
	AmountMicros                int64      `gorm:"column:amount_micros"`
	ReconciliationState         string     `gorm:"column:reconciliation_state;index:idx_billing_actual_invoice_lines_reconcile,priority:8"`
	MatchedEstimateCount        int        `gorm:"column:matched_estimate_count"`
	MatchedEstimateAmountMicros int64      `gorm:"column:matched_estimate_amount_micros"`
	ReconciledAt                *time.Time `gorm:"column:reconciled_at"`
	CreatedAt                   time.Time  `gorm:"column:created_at"`
}

func (BillingActualInvoiceLine) TableName() string { return "billing_actual_invoice_lines" }

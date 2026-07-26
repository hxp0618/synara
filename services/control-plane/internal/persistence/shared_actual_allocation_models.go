package persistence

import (
	"time"

	"github.com/google/uuid"
)

type BillingSharedActualAllocationRun struct {
	ID                           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID             uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null;uniqueIndex:uq_billing_shared_actual_allocation_runs_identity,priority:1"`
	InvoiceImportID              uuid.UUID  `gorm:"column:invoice_import_id;type:uuid;not null;uniqueIndex:uq_billing_shared_actual_allocation_runs_identity,priority:2"`
	ExecutionTargetID            uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null;uniqueIndex:uq_billing_shared_actual_allocation_runs_identity,priority:3;index:idx_billing_shared_actual_allocation_runs_target_period,priority:1"`
	LedgerCoverageID             uuid.UUID  `gorm:"column:ledger_coverage_id;type:uuid;not null"`
	Provider                     string     `gorm:"column:provider;not null"`
	CurrencyCode                 string     `gorm:"column:currency_code;not null"`
	BillingPeriodStartAt         time.Time  `gorm:"column:billing_period_start_at;not null;index:idx_billing_shared_actual_allocation_runs_target_period,priority:2"`
	BillingPeriodEndAt           time.Time  `gorm:"column:billing_period_end_at;not null;index:idx_billing_shared_actual_allocation_runs_target_period,priority:3"`
	AlgorithmVersion             string     `gorm:"column:algorithm_version;not null;uniqueIndex:uq_billing_shared_actual_allocation_runs_identity,priority:4"`
	SourceChecksum               string     `gorm:"column:source_checksum;not null"`
	SourceScopeAttestationSHA256 string     `gorm:"column:source_scope_attestation_sha256;not null"`
	SourceLineSetSHA256          string     `gorm:"column:source_line_set_sha256;not null"`
	ImportLineCount              int64      `gorm:"column:import_line_count;not null"`
	ImportAmountMicros           int64      `gorm:"column:import_amount_micros;not null"`
	SourceLineCount              int64      `gorm:"column:source_line_count;not null"`
	SourceAmountMicros           int64      `gorm:"column:source_amount_micros;not null"`
	UnallocatedLineCount         int64      `gorm:"column:unallocated_line_count;not null"`
	UnallocatedAmountMicros      int64      `gorm:"column:unallocated_amount_micros;not null"`
	AllocationLineCount          int64      `gorm:"column:allocation_line_count;not null"`
	AllocationSliceCount         int64      `gorm:"column:allocation_slice_count;not null"`
	AllocatedAmountMicros        int64      `gorm:"column:allocated_amount_micros;not null"`
	State                        string     `gorm:"column:state;not null"`
	CreatedBy                    uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	CreatedAt                    time.Time  `gorm:"column:created_at;not null"`
	SealedAt                     *time.Time `gorm:"column:sealed_at"`
}

func (BillingSharedActualAllocationRun) TableName() string {
	return "billing_shared_actual_allocation_runs"
}

type BillingSharedActualAllocationLine struct {
	ID                      uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	RunID                   uuid.UUID `gorm:"column:run_id;type:uuid;not null;index"`
	ActualInvoiceLineID     uuid.UUID `gorm:"column:actual_invoice_line_id;type:uuid;not null;uniqueIndex"`
	SourceAmountMicros      int64     `gorm:"column:source_amount_micros;not null"`
	EstimatedAmountMicros   int64     `gorm:"column:estimated_amount_micros;not null"`
	AllocatedAmountMicros   int64     `gorm:"column:allocated_amount_micros;not null"`
	EstimatedSliceCount     int64     `gorm:"column:estimated_slice_count;not null"`
	EstimatedSliceSetSHA256 string    `gorm:"column:estimated_slice_set_sha256;not null"`
	CreatedAt               time.Time `gorm:"column:created_at;not null"`
}

func (BillingSharedActualAllocationLine) TableName() string {
	return "billing_shared_actual_allocation_lines"
}

type BillingSharedActualChargeSlice struct {
	ID                   uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AllocationLineID     uuid.UUID  `gorm:"column:allocation_line_id;type:uuid;not null;uniqueIndex:uq_billing_shared_actual_charge_slices_semantic,priority:1;index"`
	EstimatedSliceID     uuid.UUID  `gorm:"column:estimated_slice_id;type:uuid;not null;uniqueIndex;uniqueIndex:uq_billing_shared_actual_charge_slices_semantic,priority:2"`
	TenantID             *uuid.UUID `gorm:"column:tenant_id;type:uuid;index"`
	AllocationKind       string     `gorm:"column:allocation_kind;not null"`
	EstimateWeightMicros int64      `gorm:"column:estimate_weight_micros;not null"`
	AmountMicros         int64      `gorm:"column:amount_micros;not null"`
	CreatedAt            time.Time  `gorm:"column:created_at;not null"`
}

func (BillingSharedActualChargeSlice) TableName() string {
	return "billing_shared_actual_charge_slices"
}

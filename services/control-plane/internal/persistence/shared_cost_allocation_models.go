package persistence

import (
	"time"

	"github.com/google/uuid"
)

type BillingSharedTargetLedgerCoverage struct {
	ID                          uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ExecutionTargetID           uuid.UUID `gorm:"column:execution_target_id;type:uuid;not null;uniqueIndex"`
	CompleteFromAt              time.Time `gorm:"column:complete_from_at;not null"`
	MinimumWriterVersion        string    `gorm:"column:minimum_writer_version;not null"`
	DeploymentAttestationSHA256 string    `gorm:"column:deployment_attestation_sha256;not null"`
	SealedAt                    time.Time `gorm:"column:sealed_at;not null"`
	SealedBy                    uuid.UUID `gorm:"column:sealed_by;type:uuid;not null"`
}

func (BillingSharedTargetLedgerCoverage) TableName() string {
	return "billing_shared_target_ledger_coverages"
}

type BillingSharedCostAllocationRun struct {
	ID                     uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ExecutionTargetID      uuid.UUID `gorm:"column:execution_target_id;type:uuid;not null"`
	WorkerID               uuid.UUID `gorm:"column:worker_id;type:uuid;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:1"`
	WorkerIncarnation      int64     `gorm:"column:worker_incarnation;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:2"`
	LedgerCoverageID       uuid.UUID `gorm:"column:ledger_coverage_id;type:uuid;not null"`
	Provider               string    `gorm:"column:provider;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:3"`
	Region                 string    `gorm:"column:region;not null"`
	CurrencyCode           string    `gorm:"column:currency_code;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:4"`
	BillingPeriodStartAt   time.Time `gorm:"column:billing_period_start_at;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:5"`
	BillingPeriodEndAt     time.Time `gorm:"column:billing_period_end_at;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:6"`
	AlgorithmVersion       string    `gorm:"column:algorithm_version;not null;uniqueIndex:uq_billing_shared_cost_allocation_runs_identity,priority:7"`
	UsageStartAt           time.Time `gorm:"column:usage_start_at;not null"`
	UsageEndAt             time.Time `gorm:"column:usage_end_at;not null"`
	ClaimCount             int64     `gorm:"column:claim_count;not null"`
	ReleaseCount           int64     `gorm:"column:release_count;not null"`
	LedgerSHA256           string    `gorm:"column:ledger_sha256;not null"`
	TenantAllocatedSeconds int64     `gorm:"column:tenant_allocated_seconds;not null"`
	PlatformIdleSeconds    int64     `gorm:"column:platform_idle_seconds;not null"`
	CreatedAt              time.Time `gorm:"column:created_at;not null"`
}

func (BillingSharedCostAllocationRun) TableName() string {
	return "billing_shared_cost_allocation_runs"
}

type BillingSharedEstimatedChargeSlice struct {
	ID                             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	RunID                          uuid.UUID  `gorm:"column:run_id;type:uuid;not null;index"`
	TenantID                       *uuid.UUID `gorm:"column:tenant_id;type:uuid;index"`
	ClaimFactID                    *uuid.UUID `gorm:"column:claim_fact_id;type:uuid;index"`
	TariffID                       uuid.UUID  `gorm:"column:tariff_id;type:uuid;not null;index"`
	ChargeKind                     string     `gorm:"column:charge_kind;not null"`
	AllocationKind                 string     `gorm:"column:allocation_kind;not null"`
	ResourceCorrelationKey         string     `gorm:"column:resource_correlation_key;not null"`
	BillingPeriodStartAt           time.Time  `gorm:"column:billing_period_start_at;not null"`
	BillingPeriodEndAt             time.Time  `gorm:"column:billing_period_end_at;not null"`
	UsageStartAt                   time.Time  `gorm:"column:usage_start_at;not null"`
	UsageEndAt                     time.Time  `gorm:"column:usage_end_at;not null"`
	BillableSeconds                int64      `gorm:"column:billable_seconds;not null"`
	ClaimCount                     int64      `gorm:"column:claim_count;not null"`
	RequestedCPUMillicores         *int64     `gorm:"column:requested_cpu_millicores"`
	RequestedMemoryBytes           *int64     `gorm:"column:requested_memory_bytes"`
	RequestedEphemeralStorageBytes *int64     `gorm:"column:requested_ephemeral_storage_bytes"`
	RateMicros                     int64      `gorm:"column:rate_micros;not null"`
	AmountMicros                   int64      `gorm:"column:amount_micros;not null"`
	CreatedAt                      time.Time  `gorm:"column:created_at;not null"`
}

func (BillingSharedEstimatedChargeSlice) TableName() string {
	return "billing_shared_estimated_charge_slices"
}

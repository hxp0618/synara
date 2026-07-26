package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerIncarnationMetricRollup struct {
	BucketDay                            time.Time `gorm:"column:bucket_day;type:date;primaryKey"`
	TargetKind                           string    `gorm:"column:target_kind;primaryKey"`
	PoolMode                             string    `gorm:"column:pool_mode;primaryKey"`
	CapacityClass                        string    `gorm:"column:capacity_class;primaryKey"`
	FactCount                            int64     `gorm:"column:fact_count"`
	RunSeconds                           float64   `gorm:"column:run_seconds"`
	ActiveSeconds                        float64   `gorm:"column:active_seconds"`
	IdleSeconds                          float64   `gorm:"column:idle_seconds"`
	RequestedCPUSeconds                  float64   `gorm:"column:requested_cpu_seconds"`
	RequestedMemoryByteSeconds           float64   `gorm:"column:requested_memory_byte_seconds"`
	RequestedEphemeralStorageByteSeconds float64   `gorm:"column:requested_ephemeral_storage_byte_seconds"`
	CreatedAt                            time.Time `gorm:"column:created_at"`
	UpdatedAt                            time.Time `gorm:"column:updated_at"`
}

func (WorkerIncarnationMetricRollup) TableName() string {
	return "worker_incarnation_metric_rollups"
}

type WorkerIncarnationMetricRollupEntry struct {
	WorkerID          uuid.UUID  `gorm:"column:worker_id;type:uuid;primaryKey"`
	WorkerIncarnation int64      `gorm:"column:worker_incarnation;primaryKey"`
	TerminalAt        time.Time  `gorm:"column:terminal_at"`
	BucketDay         time.Time  `gorm:"column:bucket_day;type:date"`
	RolledUpAt        *time.Time `gorm:"column:rolled_up_at"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (WorkerIncarnationMetricRollupEntry) TableName() string {
	return "worker_incarnation_metric_rollup_entries"
}

type ExecutionGenerationMetricRollup struct {
	BucketDay       time.Time `gorm:"column:bucket_day;type:date;primaryKey"`
	MetricKind      string    `gorm:"column:metric_kind;primaryKey"`
	TargetKind      string    `gorm:"column:target_kind;primaryKey"`
	RecoveryReason  string    `gorm:"column:recovery_reason;primaryKey"`
	Outcome         string    `gorm:"column:outcome;primaryKey"`
	WarmPoolMode    string    `gorm:"column:warm_pool_mode;primaryKey"`
	WarmPoolResult  string    `gorm:"column:warm_pool_result;primaryKey"`
	FailureClass    string    `gorm:"column:failure_class;primaryKey"`
	HistogramBucket int       `gorm:"column:histogram_bucket;primaryKey"`
	SampleCount     int64     `gorm:"column:sample_count"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

func (ExecutionGenerationMetricRollup) TableName() string {
	return "execution_generation_metric_rollups"
}

type ExecutionGenerationMetricRollupEntry struct {
	TenantID            uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ExecutionID         uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey"`
	Generation          int64      `gorm:"column:generation;primaryKey"`
	DispatchRequestedAt time.Time  `gorm:"column:dispatch_requested_at"`
	TerminalAt          time.Time  `gorm:"column:terminal_at"`
	BucketDay           time.Time  `gorm:"column:bucket_day;type:date"`
	RolledUpAt          *time.Time `gorm:"column:rolled_up_at"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

func (ExecutionGenerationMetricRollupEntry) TableName() string {
	return "execution_generation_metric_rollup_entries"
}

type ExecutionGenerationPodFailureMetricRollupEntry struct {
	TenantID        uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ExecutionID     uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey"`
	Generation      int64      `gorm:"column:generation;primaryKey"`
	FailureClass    string     `gorm:"column:failure_class;primaryKey"`
	FirstObservedAt time.Time  `gorm:"column:first_observed_at"`
	BucketDay       time.Time  `gorm:"column:bucket_day;type:date"`
	RolledUpAt      *time.Time `gorm:"column:rolled_up_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (ExecutionGenerationPodFailureMetricRollupEntry) TableName() string {
	return "execution_generation_pod_failure_metric_rollup_entries"
}

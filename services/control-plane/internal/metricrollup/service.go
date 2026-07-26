package metricrollup

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/metricfacts"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// Keep the original key stable across rolling deploys. Migration 081 expands
// the same transaction-fenced cycle beyond Worker facts.
const metricRollupCycleLock = "synara:worker-incarnation-metric-rollup-cycle"

type Summary struct {
	ProcessedFacts           int
	UpdatedBuckets           int
	ProcessedWorkerFacts     int
	ProcessedGenerationFacts int
	ProcessedPodFailureFacts int
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

type pendingWorkerIncarnationSample struct {
	WorkerID                       uuid.UUID  `gorm:"column:worker_id"`
	WorkerIncarnation              int64      `gorm:"column:worker_incarnation"`
	EntryTerminalAt                time.Time  `gorm:"column:entry_terminal_at"`
	EntryBucketDay                 time.Time  `gorm:"column:entry_bucket_day"`
	TargetKind                     string     `gorm:"column:target_kind"`
	PoolMode                       *string    `gorm:"column:pool_mode"`
	CapacityClass                  *string    `gorm:"column:capacity_class"`
	CurrentState                   string     `gorm:"column:current_state"`
	RegisteredAt                   time.Time  `gorm:"column:registered_at"`
	StateChangedAt                 time.Time  `gorm:"column:state_changed_at"`
	TerminatedAt                   *time.Time `gorm:"column:terminated_at"`
	AccumulatedActiveSeconds       int64      `gorm:"column:accumulated_active_seconds"`
	AccumulatedIdleSeconds         int64      `gorm:"column:accumulated_idle_seconds"`
	RequestedCPUMillicores         *int64     `gorm:"column:requested_cpu_millicores"`
	RequestedMemoryBytes           *int64     `gorm:"column:requested_memory_bytes"`
	RequestedEphemeralStorageBytes *int64     `gorm:"column:requested_ephemeral_storage_bytes"`
}

func (sample pendingWorkerIncarnationSample) metricSample() metricfacts.WorkerIncarnationSample {
	return metricfacts.WorkerIncarnationSample{
		TargetKind: sample.TargetKind, PoolMode: sample.PoolMode, CapacityClass: sample.CapacityClass,
		CurrentState: sample.CurrentState, RegisteredAt: sample.RegisteredAt, StateChangedAt: sample.StateChangedAt,
		TerminatedAt:                   sample.TerminatedAt,
		AccumulatedActiveSeconds:       sample.AccumulatedActiveSeconds,
		AccumulatedIdleSeconds:         sample.AccumulatedIdleSeconds,
		RequestedCPUMillicores:         sample.RequestedCPUMillicores,
		RequestedMemoryBytes:           sample.RequestedMemoryBytes,
		RequestedEphemeralStorageBytes: sample.RequestedEphemeralStorageBytes,
	}
}

type workerRollupKey struct {
	BucketDay     time.Time
	TargetKind    string
	PoolMode      string
	CapacityClass string
}

type workerRollupContribution struct {
	FactCount                            int64
	RunSeconds                           float64
	ActiveSeconds                        float64
	IdleSeconds                          float64
	RequestedCPUSeconds                  float64
	RequestedMemoryByteSeconds           float64
	RequestedEphemeralStorageByteSeconds float64
}

func (s *Service) RunOnce(ctx context.Context, batchSize int) (Summary, error) {
	if s == nil || s.db == nil {
		return Summary{}, problem.New(500, "metric_rollup_service_unavailable", "Metric rollup storage is unavailable.")
	}
	if batchSize <= 0 || batchSize > 10000 {
		return Summary{}, problem.New(400, "invalid_metric_rollup_batch_size", "Metric rollup batch size must be between 1 and 10000.")
	}
	now := s.now().UTC()
	var summary Summary
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		acquired, err := persistence.TryTransactionAdvisoryLock(ctx, tx, metricRollupCycleLock)
		if err != nil {
			return problem.Wrap(500, "metric_rollup_lock_failed", "Metric rollup coordination failed.", err)
		}
		if !acquired {
			return nil
		}
		workerSummary, err := s.rollupWorkerIncarnations(ctx, tx, now, batchSize)
		if err != nil {
			return err
		}
		generationSummary, err := s.rollupExecutionGenerations(ctx, tx, now, batchSize)
		if err != nil {
			return err
		}
		failureSummary, err := s.rollupExecutionPodFailures(ctx, tx, now, batchSize)
		if err != nil {
			return err
		}
		summary.ProcessedWorkerFacts = workerSummary.ProcessedFacts
		summary.ProcessedGenerationFacts = generationSummary.ProcessedFacts
		summary.ProcessedPodFailureFacts = failureSummary.ProcessedFacts
		summary.ProcessedFacts = workerSummary.ProcessedFacts + generationSummary.ProcessedFacts + failureSummary.ProcessedFacts
		summary.UpdatedBuckets = workerSummary.UpdatedBuckets + generationSummary.UpdatedBuckets + failureSummary.UpdatedBuckets
		return nil
	})
	return summary, err
}

func (s *Service) rollupWorkerIncarnations(
	ctx context.Context,
	tx *gorm.DB,
	now time.Time,
	batchSize int,
) (Summary, error) {
	if !tx.Migrator().HasTable(&persistence.WorkerIncarnationMetricRollup{}) ||
		!tx.Migrator().HasTable(&persistence.WorkerIncarnationMetricRollupEntry{}) {
		return Summary{}, nil
	}
	var samples []pendingWorkerIncarnationSample
	query := tx.WithContext(ctx).
		Table("worker_incarnation_metric_rollup_entries AS entry").
		Select(`entry.worker_id, entry.worker_incarnation,
			entry.terminal_at AS entry_terminal_at, entry.bucket_day AS entry_bucket_day,
			fact.target_kind, fact.pool_mode, fact.capacity_class, fact.current_state,
			fact.registered_at, fact.state_changed_at, fact.terminated_at,
			fact.accumulated_active_seconds, fact.accumulated_idle_seconds,
			fact.requested_cpu_millicores, fact.requested_memory_bytes,
			fact.requested_ephemeral_storage_bytes`).
		Joins(`JOIN worker_incarnation_facts AS fact
			ON fact.worker_id = entry.worker_id
			AND fact.worker_incarnation = entry.worker_incarnation`).
		Where("entry.rolled_up_at IS NULL").
		Order("entry.terminal_at, entry.worker_id, entry.worker_incarnation").
		Limit(batchSize)
	query = persistence.WithLocking(query, "UPDATE", "SKIP LOCKED")
	if err := query.Scan(&samples).Error; err != nil {
		return Summary{}, problem.Wrap(500, "metric_rollup_pending_load_failed", "Pending Worker metric facts could not be loaded.", err)
	}
	if len(samples) == 0 {
		return Summary{}, nil
	}
	completedAt := now
	for _, sample := range samples {
		if sample.EntryTerminalAt.After(completedAt) {
			completedAt = sample.EntryTerminalAt
		}
	}
	grouped := make(map[workerRollupKey]workerRollupContribution)
	for _, sample := range samples {
		if sample.CurrentState != "terminated" || sample.TerminatedAt == nil ||
			!sample.TerminatedAt.Equal(sample.EntryTerminalAt) {
			return Summary{}, problem.New(500, "metric_rollup_source_invalid", "A pending Worker metric fact is not an immutable terminal incarnation.")
		}
		bucketDay := utcDay(sample.EntryTerminalAt)
		if !sameUTCDay(sample.EntryBucketDay, bucketDay) {
			return Summary{}, problem.New(500, "metric_rollup_source_invalid", "A pending Worker metric fact has an invalid UTC bucket.")
		}
		metricSample := sample.metricSample()
		dimensions := metricfacts.WorkerIncarnationMetricDimensions(metricSample)
		contribution := metricfacts.WorkerIncarnationMetricContribution(metricSample, sample.EntryTerminalAt)
		if !validContribution(contribution) {
			return Summary{}, problem.New(500, "metric_rollup_source_invalid", "A pending Worker metric fact produced an invalid contribution.")
		}
		key := workerRollupKey{
			BucketDay: bucketDay, TargetKind: dimensions.TargetKind,
			PoolMode: dimensions.PoolMode, CapacityClass: dimensions.CapacityClass,
		}
		total := grouped[key]
		total.FactCount++
		total.RunSeconds += contribution.RunSeconds
		total.ActiveSeconds += contribution.ActiveSeconds
		total.IdleSeconds += contribution.IdleSeconds
		total.RequestedCPUSeconds += contribution.RequestedCPUSeconds
		total.RequestedMemoryByteSeconds += contribution.RequestedMemoryByteSeconds
		total.RequestedEphemeralStorageByteSeconds += contribution.RequestedStorageByteSeconds
		if !validRollupContribution(total) {
			return Summary{}, problem.New(500, "metric_rollup_source_invalid", "Pending Worker metric facts overflowed a rollup contribution.")
		}
		grouped[key] = total
	}
	for key, contribution := range grouped {
		row := persistence.WorkerIncarnationMetricRollup{
			BucketDay: key.BucketDay, TargetKind: key.TargetKind,
			PoolMode: key.PoolMode, CapacityClass: key.CapacityClass,
			FactCount:  contribution.FactCount,
			RunSeconds: contribution.RunSeconds, ActiveSeconds: contribution.ActiveSeconds,
			IdleSeconds: contribution.IdleSeconds, RequestedCPUSeconds: contribution.RequestedCPUSeconds,
			RequestedMemoryByteSeconds:           contribution.RequestedMemoryByteSeconds,
			RequestedEphemeralStorageByteSeconds: contribution.RequestedEphemeralStorageByteSeconds,
			CreatedAt:                            completedAt, UpdatedAt: completedAt,
		}
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "bucket_day"}, {Name: "target_kind"},
				{Name: "pool_mode"}, {Name: "capacity_class"},
			},
			DoUpdates: clause.Assignments(workerRollupIncrementAssignments(completedAt)),
		}).Create(&row).Error; err != nil {
			return Summary{}, problem.Wrap(500, "metric_rollup_write_failed", "A Worker metric rollup bucket could not be updated.", err)
		}
	}
	condition, arguments := exactWorkerEntryCondition(samples)
	result := tx.WithContext(ctx).
		Model(&persistence.WorkerIncarnationMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").
		Where(condition, arguments...).
		Updates(map[string]any{"rolled_up_at": completedAt, "updated_at": completedAt})
	if result.Error != nil {
		return Summary{}, problem.Wrap(500, "metric_rollup_entries_complete_failed", "Worker metric rollup entries could not be completed.", result.Error)
	}
	if result.RowsAffected != int64(len(samples)) {
		return Summary{}, problem.New(409, "metric_rollup_entries_conflict", "Worker metric rollup entries changed during aggregation.")
	}
	return Summary{ProcessedFacts: len(samples), UpdatedBuckets: len(grouped)}, nil
}

type pendingExecutionGenerationSample struct {
	TenantID                 uuid.UUID  `gorm:"column:tenant_id"`
	ExecutionID              uuid.UUID  `gorm:"column:execution_id"`
	Generation               int64      `gorm:"column:generation"`
	EntryDispatchRequestedAt time.Time  `gorm:"column:entry_dispatch_requested_at"`
	EntryTerminalAt          time.Time  `gorm:"column:entry_terminal_at"`
	EntryBucketDay           time.Time  `gorm:"column:entry_bucket_day"`
	RecoveryReason           string     `gorm:"column:recovery_reason"`
	TargetKind               string     `gorm:"column:target_kind"`
	WarmPoolMode             string     `gorm:"column:warm_pool_mode"`
	WarmPoolResult           string     `gorm:"column:warm_pool_result"`
	DispatchRequestedAt      *time.Time `gorm:"column:dispatch_requested_at"`
	ProviderReadyAt          *time.Time `gorm:"column:provider_ready_at"`
	PodProvisioningAt        *time.Time `gorm:"column:pod_provisioning_started_at"`
	PodRunningAt             *time.Time `gorm:"column:pod_running_at"`
	TerminalAt               *time.Time `gorm:"column:terminal_at"`
	TerminalOutcome          *string    `gorm:"column:terminal_outcome"`
}

func (sample pendingExecutionGenerationSample) metricSample() metricfacts.ExecutionGenerationSample {
	return metricfacts.ExecutionGenerationSample{
		RecoveryReason: sample.RecoveryReason, TargetKind: sample.TargetKind,
		WarmPoolMode: sample.WarmPoolMode, WarmPoolResult: sample.WarmPoolResult,
		DispatchRequestedAt: sample.DispatchRequestedAt, ProviderReadyAt: sample.ProviderReadyAt,
		PodProvisioningAt: sample.PodProvisioningAt, PodRunningAt: sample.PodRunningAt,
		TerminalOutcome: sample.TerminalOutcome,
	}
}

type pendingExecutionPodFailureSample struct {
	TenantID             uuid.UUID `gorm:"column:tenant_id"`
	ExecutionID          uuid.UUID `gorm:"column:execution_id"`
	Generation           int64     `gorm:"column:generation"`
	FailureClass         string    `gorm:"column:failure_class"`
	EntryFirstObservedAt time.Time `gorm:"column:entry_first_observed_at"`
	EntryBucketDay       time.Time `gorm:"column:entry_bucket_day"`
	FirstObservedAt      time.Time `gorm:"column:first_observed_at"`
	TargetKind           string    `gorm:"column:target_kind"`
}

type generationRollupKey struct {
	BucketDay time.Time
	metricfacts.GenerationMetricContribution
}

func (s *Service) rollupExecutionGenerations(
	ctx context.Context,
	tx *gorm.DB,
	now time.Time,
	batchSize int,
) (Summary, error) {
	if !tx.Migrator().HasTable(&persistence.ExecutionGenerationMetricRollup{}) ||
		!tx.Migrator().HasTable(&persistence.ExecutionGenerationMetricRollupEntry{}) {
		return Summary{}, nil
	}
	var samples []pendingExecutionGenerationSample
	query := tx.WithContext(ctx).
		Table("execution_generation_metric_rollup_entries AS entry").
		Select(`entry.tenant_id, entry.execution_id, entry.generation,
			entry.dispatch_requested_at AS entry_dispatch_requested_at,
			entry.terminal_at AS entry_terminal_at, entry.bucket_day AS entry_bucket_day,
			fact.recovery_reason, fact.target_kind, fact.warm_pool_mode, fact.warm_pool_result,
			fact.dispatch_requested_at, fact.provider_ready_at,
			fact.pod_provisioning_started_at, fact.pod_running_at,
			fact.terminal_at, fact.terminal_outcome`).
		Joins(`JOIN execution_generation_facts AS fact
			ON fact.tenant_id = entry.tenant_id
			AND fact.execution_id = entry.execution_id
			AND fact.generation = entry.generation`).
		Where("entry.rolled_up_at IS NULL").
		Order("entry.terminal_at, entry.tenant_id, entry.execution_id, entry.generation").
		Limit(batchSize)
	query = persistence.WithLocking(query, "UPDATE", "SKIP LOCKED")
	if err := query.Scan(&samples).Error; err != nil {
		return Summary{}, problem.Wrap(500, "generation_metric_rollup_pending_load_failed", "Pending Execution generation metric facts could not be loaded.", err)
	}
	if len(samples) == 0 {
		return Summary{}, nil
	}
	completedAt := now
	grouped := make(map[generationRollupKey]int64)
	for _, sample := range samples {
		if sample.EntryTerminalAt.After(completedAt) {
			completedAt = sample.EntryTerminalAt
		}
		if sample.DispatchRequestedAt == nil || sample.TerminalAt == nil || sample.TerminalOutcome == nil ||
			!sample.DispatchRequestedAt.Equal(sample.EntryDispatchRequestedAt) ||
			!sample.TerminalAt.Equal(sample.EntryTerminalAt) {
			return Summary{}, problem.New(500, "generation_metric_rollup_source_invalid", "A pending Execution generation metric fact is not sealed terminal truth.")
		}
		bucketDay := utcDay(sample.EntryDispatchRequestedAt)
		if !sameUTCDay(sample.EntryBucketDay, bucketDay) {
			return Summary{}, problem.New(500, "generation_metric_rollup_source_invalid", "A pending Execution generation metric fact has an invalid UTC bucket.")
		}
		for _, contribution := range metricfacts.ExecutionGenerationMetricContributions(sample.metricSample()) {
			if contribution.HistogramBucket >= 0 &&
				!metricfacts.ValidDurationHistogramBucket(contribution.HistogramBucket) {
				return Summary{}, problem.New(500, "generation_metric_rollup_source_invalid", "An Execution generation duration exceeded the supported histogram.")
			}
			grouped[generationRollupKey{BucketDay: bucketDay, GenerationMetricContribution: contribution}]++
		}
	}
	if err := writeGenerationRollupBuckets(ctx, tx, grouped, completedAt); err != nil {
		return Summary{}, err
	}
	condition, arguments := exactGenerationEntryCondition(samples)
	result := tx.WithContext(ctx).
		Model(&persistence.ExecutionGenerationMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").
		Where(condition, arguments...).
		Updates(map[string]any{"rolled_up_at": completedAt, "updated_at": completedAt})
	if result.Error != nil {
		return Summary{}, problem.Wrap(500, "generation_metric_rollup_entries_complete_failed", "Execution generation metric rollup entries could not be completed.", result.Error)
	}
	if result.RowsAffected != int64(len(samples)) {
		return Summary{}, problem.New(409, "generation_metric_rollup_entries_conflict", "Execution generation metric rollup entries changed during aggregation.")
	}
	return Summary{ProcessedFacts: len(samples), UpdatedBuckets: len(grouped)}, nil
}

func (s *Service) rollupExecutionPodFailures(
	ctx context.Context,
	tx *gorm.DB,
	now time.Time,
	batchSize int,
) (Summary, error) {
	if !tx.Migrator().HasTable(&persistence.ExecutionGenerationMetricRollup{}) ||
		!tx.Migrator().HasTable(&persistence.ExecutionGenerationPodFailureMetricRollupEntry{}) {
		return Summary{}, nil
	}
	var samples []pendingExecutionPodFailureSample
	query := tx.WithContext(ctx).
		Table("execution_generation_pod_failure_metric_rollup_entries AS entry").
		Select(`entry.tenant_id, entry.execution_id, entry.generation, entry.failure_class,
			entry.first_observed_at AS entry_first_observed_at, entry.bucket_day AS entry_bucket_day,
			failure.first_observed_at, generation.target_kind`).
		Joins(`JOIN execution_generation_pod_failure_facts AS failure
			ON failure.tenant_id = entry.tenant_id
			AND failure.execution_id = entry.execution_id
			AND failure.generation = entry.generation
			AND failure.failure_class = entry.failure_class`).
		Joins(`JOIN execution_generation_facts AS generation
			ON generation.tenant_id = entry.tenant_id
			AND generation.execution_id = entry.execution_id
			AND generation.generation = entry.generation`).
		Where("entry.rolled_up_at IS NULL").
		Order("entry.first_observed_at, entry.tenant_id, entry.execution_id, entry.generation, entry.failure_class").
		Limit(batchSize)
	query = persistence.WithLocking(query, "UPDATE", "SKIP LOCKED")
	if err := query.Scan(&samples).Error; err != nil {
		return Summary{}, problem.Wrap(500, "pod_failure_metric_rollup_pending_load_failed", "Pending Execution Pod failure metric facts could not be loaded.", err)
	}
	if len(samples) == 0 {
		return Summary{}, nil
	}
	completedAt := now
	grouped := make(map[generationRollupKey]int64)
	for _, sample := range samples {
		if sample.EntryFirstObservedAt.After(completedAt) {
			completedAt = sample.EntryFirstObservedAt
		}
		if !sample.FirstObservedAt.Equal(sample.EntryFirstObservedAt) {
			return Summary{}, problem.New(500, "pod_failure_metric_rollup_source_invalid", "A pending Execution Pod failure metric fact changed identity.")
		}
		bucketDay := utcDay(sample.EntryFirstObservedAt)
		if !sameUTCDay(sample.EntryBucketDay, bucketDay) {
			return Summary{}, problem.New(500, "pod_failure_metric_rollup_source_invalid", "A pending Execution Pod failure metric fact has an invalid UTC bucket.")
		}
		contribution := metricfacts.PodFailureMetricContribution(sample.TargetKind, sample.FailureClass)
		grouped[generationRollupKey{BucketDay: bucketDay, GenerationMetricContribution: contribution}]++
	}
	if err := writeGenerationRollupBuckets(ctx, tx, grouped, completedAt); err != nil {
		return Summary{}, err
	}
	condition, arguments := exactPodFailureEntryCondition(samples)
	result := tx.WithContext(ctx).
		Model(&persistence.ExecutionGenerationPodFailureMetricRollupEntry{}).
		Where("rolled_up_at IS NULL").
		Where(condition, arguments...).
		Updates(map[string]any{"rolled_up_at": completedAt, "updated_at": completedAt})
	if result.Error != nil {
		return Summary{}, problem.Wrap(500, "pod_failure_metric_rollup_entries_complete_failed", "Execution Pod failure metric rollup entries could not be completed.", result.Error)
	}
	if result.RowsAffected != int64(len(samples)) {
		return Summary{}, problem.New(409, "pod_failure_metric_rollup_entries_conflict", "Execution Pod failure metric rollup entries changed during aggregation.")
	}
	return Summary{ProcessedFacts: len(samples), UpdatedBuckets: len(grouped)}, nil
}

func writeGenerationRollupBuckets(
	ctx context.Context,
	tx *gorm.DB,
	grouped map[generationRollupKey]int64,
	completedAt time.Time,
) error {
	keys := make([]generationRollupKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return generationRollupKeyOrder(keys[i]) < generationRollupKeyOrder(keys[j]) })
	for _, key := range keys {
		contribution := key.GenerationMetricContribution
		row := persistence.ExecutionGenerationMetricRollup{
			BucketDay: key.BucketDay, MetricKind: contribution.MetricKind,
			TargetKind: contribution.TargetKind, RecoveryReason: contribution.RecoveryReason,
			Outcome: contribution.Outcome, WarmPoolMode: contribution.WarmPoolMode,
			WarmPoolResult: contribution.WarmPoolResult, FailureClass: contribution.FailureClass,
			HistogramBucket: contribution.HistogramBucket, SampleCount: grouped[key],
			CreatedAt: completedAt, UpdatedAt: completedAt,
		}
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "bucket_day"}, {Name: "metric_kind"}, {Name: "target_kind"},
				{Name: "recovery_reason"}, {Name: "outcome"}, {Name: "warm_pool_mode"},
				{Name: "warm_pool_result"}, {Name: "failure_class"}, {Name: "histogram_bucket"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"sample_count": gorm.Expr("execution_generation_metric_rollups.sample_count + excluded.sample_count"),
				"updated_at":   completedAt,
			}),
		}).Create(&row).Error; err != nil {
			return problem.Wrap(500, "generation_metric_rollup_write_failed", "An Execution generation metric rollup bucket could not be updated.", err)
		}
	}
	return nil
}

func generationRollupKeyOrder(key generationRollupKey) string {
	return strings.Join([]string{
		key.BucketDay.UTC().Format("2006-01-02"), key.MetricKind, key.TargetKind,
		key.RecoveryReason, key.Outcome, key.WarmPoolMode, key.WarmPoolResult,
		key.FailureClass, string(rune(key.HistogramBucket + 1)),
	}, "\x00")
}

func exactGenerationEntryCondition(samples []pendingExecutionGenerationSample) (string, []any) {
	parts := make([]string, 0, len(samples))
	arguments := make([]any, 0, len(samples)*3)
	for _, sample := range samples {
		parts = append(parts, "(tenant_id = ? AND execution_id = ? AND generation = ?)")
		arguments = append(arguments, sample.TenantID, sample.ExecutionID, sample.Generation)
	}
	return "(" + strings.Join(parts, " OR ") + ")", arguments
}

func exactPodFailureEntryCondition(samples []pendingExecutionPodFailureSample) (string, []any) {
	parts := make([]string, 0, len(samples))
	arguments := make([]any, 0, len(samples)*4)
	for _, sample := range samples {
		parts = append(parts, "(tenant_id = ? AND execution_id = ? AND generation = ? AND failure_class = ?)")
		arguments = append(arguments, sample.TenantID, sample.ExecutionID, sample.Generation, sample.FailureClass)
	}
	return "(" + strings.Join(parts, " OR ") + ")", arguments
}

func workerRollupIncrementAssignments(now time.Time) map[string]any {
	const table = "worker_incarnation_metric_rollups"
	return map[string]any{
		"fact_count":                               gorm.Expr(table + ".fact_count + excluded.fact_count"),
		"run_seconds":                              gorm.Expr(table + ".run_seconds + excluded.run_seconds"),
		"active_seconds":                           gorm.Expr(table + ".active_seconds + excluded.active_seconds"),
		"idle_seconds":                             gorm.Expr(table + ".idle_seconds + excluded.idle_seconds"),
		"requested_cpu_seconds":                    gorm.Expr(table + ".requested_cpu_seconds + excluded.requested_cpu_seconds"),
		"requested_memory_byte_seconds":            gorm.Expr(table + ".requested_memory_byte_seconds + excluded.requested_memory_byte_seconds"),
		"requested_ephemeral_storage_byte_seconds": gorm.Expr(table + ".requested_ephemeral_storage_byte_seconds + excluded.requested_ephemeral_storage_byte_seconds"),
		"updated_at":                               now,
	}
}

func exactWorkerEntryCondition(samples []pendingWorkerIncarnationSample) (string, []any) {
	parts := make([]string, 0, len(samples))
	arguments := make([]any, 0, len(samples)*2)
	for _, sample := range samples {
		parts = append(parts, "(worker_id = ? AND worker_incarnation = ?)")
		arguments = append(arguments, sample.WorkerID, sample.WorkerIncarnation)
	}
	return "(" + strings.Join(parts, " OR ") + ")", arguments
}

func utcDay(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func sameUTCDay(left, right time.Time) bool {
	return utcDay(left).Equal(utcDay(right))
}

func validContribution(contribution metricfacts.WorkerIncarnationContribution) bool {
	for _, value := range []float64{
		contribution.RunSeconds,
		contribution.ActiveSeconds,
		contribution.IdleSeconds,
		contribution.RequestedCPUSeconds,
		contribution.RequestedMemoryByteSeconds,
		contribution.RequestedStorageByteSeconds,
	} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func validRollupContribution(contribution workerRollupContribution) bool {
	if contribution.FactCount <= 0 {
		return false
	}
	for _, value := range []float64{
		contribution.RunSeconds,
		contribution.ActiveSeconds,
		contribution.IdleSeconds,
		contribution.RequestedCPUSeconds,
		contribution.RequestedMemoryByteSeconds,
		contribution.RequestedEphemeralStorageByteSeconds,
	} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

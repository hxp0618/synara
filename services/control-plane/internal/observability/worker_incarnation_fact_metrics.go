package observability

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/metricfacts"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const workerIncarnationMetricSelect = `target_kind, pool_mode, capacity_class, current_state,
	registered_at, state_changed_at, terminated_at,
	accumulated_active_seconds, accumulated_idle_seconds,
	requested_cpu_millicores, requested_memory_bytes,
	requested_ephemeral_storage_bytes`

const workerIncarnationMetricSelectFromFact = `fact.target_kind, fact.pool_mode, fact.capacity_class, fact.current_state,
	fact.registered_at, fact.state_changed_at, fact.terminated_at,
	fact.accumulated_active_seconds, fact.accumulated_idle_seconds,
	fact.requested_cpu_millicores, fact.requested_memory_bytes,
	fact.requested_ephemeral_storage_bytes`

type workerIncarnationRollupTotals struct {
	TargetKind                           string  `gorm:"column:target_kind"`
	PoolMode                             string  `gorm:"column:pool_mode"`
	CapacityClass                        string  `gorm:"column:capacity_class"`
	FactCount                            int64   `gorm:"column:fact_count"`
	RunSeconds                           float64 `gorm:"column:run_seconds"`
	ActiveSeconds                        float64 `gorm:"column:active_seconds"`
	IdleSeconds                          float64 `gorm:"column:idle_seconds"`
	RequestedCPUSeconds                  float64 `gorm:"column:requested_cpu_seconds"`
	RequestedMemoryByteSeconds           float64 `gorm:"column:requested_memory_byte_seconds"`
	RequestedEphemeralStorageByteSeconds float64 `gorm:"column:requested_ephemeral_storage_byte_seconds"`
}

func (r *Registry) writeWorkerIncarnationFactMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	if !r.db.Migrator().HasTable("worker_incarnation_facts") {
		return nil
	}
	var options []*sql.TxOptions
	if r.db.Dialector.Name() == "postgres" {
		options = append(options, &sql.TxOptions{
			Isolation: sql.LevelRepeatableRead,
			ReadOnly:  true,
		})
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return writeWorkerIncarnationFactMetricsSnapshot(ctx, tx, output, now)
	}, options...)
}

func writeWorkerIncarnationFactMetricsSnapshot(
	ctx context.Context,
	db *gorm.DB,
	output *bytes.Buffer,
	now time.Time,
) error {
	counts := make(map[string]int64)
	runSeconds := make(map[string]float64)
	activeSeconds := make(map[string]float64)
	idleSeconds := make(map[string]float64)
	cpuSeconds := make(map[string]float64)
	memoryByteSeconds := make(map[string]float64)
	ephemeralStorageByteSeconds := make(map[string]float64)

	addContribution := func(
		dimensions metricfacts.WorkerIncarnationDimensions,
		factCount int64,
		contribution metricfacts.WorkerIncarnationContribution,
	) {
		labelSet := labels(map[string]string{
			"target_kind":    boundedTargetKind(dimensions.TargetKind),
			"mode":           dimensions.PoolMode,
			"capacity_class": dimensions.CapacityClass,
			"state":          dimensions.State,
		})
		counts[labelSet] += factCount
		runSeconds[labelSet] += contribution.RunSeconds
		activeSeconds[labelSet] += contribution.ActiveSeconds
		idleSeconds[labelSet] += contribution.IdleSeconds
		cpuSeconds[labelSet] += contribution.RequestedCPUSeconds
		memoryByteSeconds[labelSet] += contribution.RequestedMemoryByteSeconds
		ephemeralStorageByteSeconds[labelSet] += contribution.RequestedStorageByteSeconds
	}
	addRawSamples := func(samples []metricfacts.WorkerIncarnationSample) {
		for _, sample := range samples {
			addContribution(
				metricfacts.WorkerIncarnationMetricDimensions(sample),
				1,
				metricfacts.WorkerIncarnationMetricContribution(sample, now),
			)
		}
	}

	rollupsAvailable := db.Migrator().HasTable(&persistence.WorkerIncarnationMetricRollup{}) &&
		db.Migrator().HasTable(&persistence.WorkerIncarnationMetricRollupEntry{})
	pendingFacts := int64(0)
	rollupBuckets := int64(0)
	if rollupsAvailable {
		if err := db.WithContext(ctx).Model(&persistence.WorkerIncarnationMetricRollup{}).
			Count(&rollupBuckets).Error; err != nil {
			return fmt.Errorf("count Worker incarnation metric rollup buckets: %w", err)
		}
		var rollups []workerIncarnationRollupTotals
		if err := db.WithContext(ctx).Table("worker_incarnation_metric_rollups").
			Select(`target_kind, pool_mode, capacity_class,
				SUM(fact_count) AS fact_count,
				SUM(run_seconds) AS run_seconds,
				SUM(active_seconds) AS active_seconds,
				SUM(idle_seconds) AS idle_seconds,
				SUM(requested_cpu_seconds) AS requested_cpu_seconds,
				SUM(requested_memory_byte_seconds) AS requested_memory_byte_seconds,
				SUM(requested_ephemeral_storage_byte_seconds) AS requested_ephemeral_storage_byte_seconds`).
			Group("target_kind, pool_mode, capacity_class").
			Order("target_kind, pool_mode, capacity_class").
			Find(&rollups).Error; err != nil {
			return fmt.Errorf("collect Worker incarnation metric rollups: %w", err)
		}
		for _, rollup := range rollups {
			addContribution(
				metricfacts.WorkerIncarnationDimensions{
					TargetKind: rollup.TargetKind, PoolMode: rollup.PoolMode,
					CapacityClass: rollup.CapacityClass, State: "terminated",
				},
				rollup.FactCount,
				metricfacts.WorkerIncarnationContribution{
					RunSeconds: rollup.RunSeconds, ActiveSeconds: rollup.ActiveSeconds,
					IdleSeconds: rollup.IdleSeconds, RequestedCPUSeconds: rollup.RequestedCPUSeconds,
					RequestedMemoryByteSeconds:  rollup.RequestedMemoryByteSeconds,
					RequestedStorageByteSeconds: rollup.RequestedEphemeralStorageByteSeconds,
				},
			)
		}

		var liveSamples []metricfacts.WorkerIncarnationSample
		if err := db.WithContext(ctx).Table("worker_incarnation_facts").
			Select(workerIncarnationMetricSelect).
			Where("current_state <> ?", "terminated").
			Find(&liveSamples).Error; err != nil {
			return fmt.Errorf("collect nonterminal Worker incarnation metrics: %w", err)
		}
		addRawSamples(liveSamples)

		var pendingSamples []metricfacts.WorkerIncarnationSample
		if err := db.WithContext(ctx).Table("worker_incarnation_metric_rollup_entries AS entry").
			Select(workerIncarnationMetricSelectFromFact).
			Joins(`JOIN worker_incarnation_facts AS fact
				ON fact.worker_id = entry.worker_id
				AND fact.worker_incarnation = entry.worker_incarnation`).
			Where("entry.rolled_up_at IS NULL").
			Find(&pendingSamples).Error; err != nil {
			return fmt.Errorf("collect pending Worker incarnation rollup metrics: %w", err)
		}
		pendingFacts = int64(len(pendingSamples))
		addRawSamples(pendingSamples)
	} else {
		var samples []metricfacts.WorkerIncarnationSample
		if err := db.WithContext(ctx).Table("worker_incarnation_facts").
			Select(workerIncarnationMetricSelect).
			Find(&samples).Error; err != nil {
			return fmt.Errorf("collect durable Worker incarnation metrics: %w", err)
		}
		addRawSamples(samples)
	}

	writeHelp(output, "synara_worker_incarnation_facts", "Retained physical Worker or Pod incarnation facts from exact terminal rollups plus authoritative pending and nonterminal facts.", "gauge")
	for _, labelSet := range sortedMetricLabelSets(counts) {
		fmt.Fprintf(output, "synara_worker_incarnation_facts%s %d\n", labelSet, counts[labelSet])
	}
	writeHelp(output, "synara_worker_incarnation_run_seconds", "Retained physical Worker or Pod runtime seconds derived from durable registration and termination timestamps.", "gauge")
	for _, labelSet := range sortedFloatMetricLabelSets(runSeconds) {
		fmt.Fprintf(output, "synara_worker_incarnation_run_seconds%s %s\n", labelSet, formatFloat(runSeconds[labelSet]))
	}
	writeHelp(output, "synara_worker_incarnation_active_seconds", "Retained active Execution seconds derived from durable Worker lifecycle transitions.", "gauge")
	for _, labelSet := range sortedFloatMetricLabelSets(activeSeconds) {
		fmt.Fprintf(output, "synara_worker_incarnation_active_seconds%s %s\n", labelSet, formatFloat(activeSeconds[labelSet]))
	}
	writeHelp(output, "synara_worker_incarnation_idle_seconds", "Retained idle seconds derived from durable Worker lifecycle facts; active and terminating time is excluded.", "gauge")
	for _, labelSet := range sortedFloatMetricLabelSets(idleSeconds) {
		fmt.Fprintf(output, "synara_worker_incarnation_idle_seconds%s %s\n", labelSet, formatFloat(idleSeconds[labelSet]))
	}
	writeHelp(output, "synara_worker_incarnation_requested_cpu_seconds", "Requested CPU core-seconds proxy derived from requested millicores multiplied by durable runtime seconds; this is not billable currency cost.", "gauge")
	for _, labelSet := range sortedFloatMetricLabelSets(cpuSeconds) {
		fmt.Fprintf(output, "synara_worker_incarnation_requested_cpu_seconds%s %s\n", labelSet, formatFloat(cpuSeconds[labelSet]))
	}
	writeHelp(output, "synara_worker_incarnation_requested_memory_byte_seconds", "Requested memory byte-seconds proxy derived from requested memory bytes multiplied by durable runtime seconds; this is not billable currency cost.", "gauge")
	for _, labelSet := range sortedFloatMetricLabelSets(memoryByteSeconds) {
		fmt.Fprintf(output, "synara_worker_incarnation_requested_memory_byte_seconds%s %s\n", labelSet, formatFloat(memoryByteSeconds[labelSet]))
	}
	writeHelp(output, "synara_worker_incarnation_requested_ephemeral_storage_byte_seconds", "Requested ephemeral-storage byte-seconds proxy derived from durable runtime seconds; this is not billable currency cost.", "gauge")
	for _, labelSet := range sortedFloatMetricLabelSets(ephemeralStorageByteSeconds) {
		fmt.Fprintf(
			output,
			"synara_worker_incarnation_requested_ephemeral_storage_byte_seconds%s %s\n",
			labelSet,
			formatFloat(ephemeralStorageByteSeconds[labelSet]),
		)
	}
	if rollupsAvailable {
		writeHelp(output, "synara_metric_rollup_pending_facts", "Durable facts still awaiting exact metric rollup membership completion.", "gauge")
		fmt.Fprintf(output, "synara_metric_rollup_pending_facts%s %d\n", labels(map[string]string{"kind": "worker-incarnation"}), pendingFacts)
		writeHelp(output, "synara_metric_rollup_buckets", "Durable metric rollup bucket inventory.", "gauge")
		fmt.Fprintf(output, "synara_metric_rollup_buckets%s %d\n", labels(map[string]string{"kind": "worker-incarnation"}), rollupBuckets)
	}
	return nil
}

func sortedFloatMetricLabelSets(values map[string]float64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

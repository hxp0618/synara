package observability

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type durableWorkerIncarnationMetricSample struct {
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

func (r *Registry) writeWorkerIncarnationFactMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	if !r.db.Migrator().HasTable("worker_incarnation_facts") {
		return nil
	}
	samples := make([]durableWorkerIncarnationMetricSample, 0)
	if err := r.db.WithContext(ctx).Table("worker_incarnation_facts").
		Select(`target_kind, pool_mode, capacity_class, current_state,
			registered_at, state_changed_at, terminated_at,
			accumulated_active_seconds, accumulated_idle_seconds,
			requested_cpu_millicores, requested_memory_bytes,
			requested_ephemeral_storage_bytes`).
		Find(&samples).Error; err != nil {
		return fmt.Errorf("collect durable Worker incarnation metrics: %w", err)
	}

	counts := make(map[string]int64)
	runSeconds := make(map[string]float64)
	activeSeconds := make(map[string]float64)
	idleSeconds := make(map[string]float64)
	cpuSeconds := make(map[string]float64)
	memoryByteSeconds := make(map[string]float64)
	ephemeralStorageByteSeconds := make(map[string]float64)

	for _, sample := range samples {
		labelSet := labels(map[string]string{
			"target_kind":    boundedTargetKind(sample.TargetKind),
			"mode":           boundedWorkerPoolMetricMode(sample.PoolMode),
			"capacity_class": boundedWorkerCapacityClass(sample.CapacityClass),
			"state":          boundedWorkerLifecycleState(sample.CurrentState),
		})
		counts[labelSet]++

		runEnd := now
		if sample.TerminatedAt != nil && sample.TerminatedAt.Before(runEnd) {
			runEnd = *sample.TerminatedAt
		}
		runtimeSeconds := 0.0
		if !runEnd.Before(sample.RegisteredAt) {
			runtimeSeconds = runEnd.Sub(sample.RegisteredAt).Seconds()
		}
		runSeconds[labelSet] += runtimeSeconds

		activeTotal := float64(sample.AccumulatedActiveSeconds)
		if sample.CurrentState == "active" && !now.Before(sample.StateChangedAt) {
			activeTotal += now.Sub(sample.StateChangedAt).Seconds()
		}
		activeSeconds[labelSet] += activeTotal

		idleTotal := float64(sample.AccumulatedIdleSeconds)
		if sample.CurrentState == "idle" && !now.Before(sample.StateChangedAt) {
			idleTotal += now.Sub(sample.StateChangedAt).Seconds()
		}
		idleSeconds[labelSet] += idleTotal

		if sample.RequestedCPUMillicores != nil {
			cpuSeconds[labelSet] += runtimeSeconds * (float64(*sample.RequestedCPUMillicores) / 1000.0)
		}
		if sample.RequestedMemoryBytes != nil {
			memoryByteSeconds[labelSet] += runtimeSeconds * float64(*sample.RequestedMemoryBytes)
		}
		if sample.RequestedEphemeralStorageBytes != nil {
			ephemeralStorageByteSeconds[labelSet] += runtimeSeconds * float64(*sample.RequestedEphemeralStorageBytes)
		}
	}

	writeHelp(output, "synara_worker_incarnation_facts", "Retained immutable physical Worker or Pod incarnation facts by target kind, pool mode, capacity class, and current state.", "gauge")
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
		fmt.Fprintf(output, "synara_worker_incarnation_requested_ephemeral_storage_byte_seconds%s %s\n", labelSet, formatFloat(ephemeralStorageByteSeconds[labelSet]))
	}
	return nil
}

func boundedWorkerPoolMetricMode(value *string) string {
	mode := ""
	if value != nil {
		mode = strings.TrimSpace(*value)
	}
	switch mode {
	case "resident", "per-execution", "warm":
		return mode
	case "":
		return "unassigned"
	default:
		return "other"
	}
}

func boundedWorkerCapacityClass(value *string) string {
	capacity := ""
	if value != nil {
		capacity = strings.TrimSpace(*value)
	}
	switch capacity {
	case "standard", "interactive":
		return capacity
	case "":
		return "unassigned"
	default:
		return "other"
	}
}

func boundedWorkerLifecycleState(value string) string {
	switch strings.TrimSpace(value) {
	case "idle", "active", "draining", "offline", "terminated":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

func sortedFloatMetricLabelSets(values map[string]float64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

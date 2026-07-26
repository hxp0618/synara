package metricfacts

import (
	"strings"
	"time"
)

type WorkerIncarnationSample struct {
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

type WorkerIncarnationDimensions struct {
	TargetKind    string
	PoolMode      string
	CapacityClass string
	State         string
}

type WorkerIncarnationContribution struct {
	RunSeconds                  float64
	ActiveSeconds               float64
	IdleSeconds                 float64
	RequestedCPUSeconds         float64
	RequestedMemoryByteSeconds  float64
	RequestedStorageByteSeconds float64
}

func WorkerIncarnationMetricDimensions(sample WorkerIncarnationSample) WorkerIncarnationDimensions {
	return WorkerIncarnationDimensions{
		TargetKind:    strings.TrimSpace(sample.TargetKind),
		PoolMode:      BoundedWorkerPoolMode(sample.PoolMode),
		CapacityClass: BoundedWorkerCapacityClass(sample.CapacityClass),
		State:         BoundedWorkerLifecycleState(sample.CurrentState),
	}
}

func WorkerIncarnationMetricContribution(
	sample WorkerIncarnationSample,
	now time.Time,
) WorkerIncarnationContribution {
	now = now.UTC()
	runEnd := now
	if sample.TerminatedAt != nil && sample.TerminatedAt.Before(runEnd) {
		runEnd = sample.TerminatedAt.UTC()
	}
	runtimeSeconds := 0.0
	if !runEnd.Before(sample.RegisteredAt) {
		runtimeSeconds = runEnd.Sub(sample.RegisteredAt).Seconds()
	}

	activeSeconds := float64(sample.AccumulatedActiveSeconds)
	if sample.CurrentState == "active" && !now.Before(sample.StateChangedAt) {
		activeSeconds += now.Sub(sample.StateChangedAt).Seconds()
	}
	idleSeconds := float64(sample.AccumulatedIdleSeconds)
	if sample.CurrentState == "idle" && !now.Before(sample.StateChangedAt) {
		idleSeconds += now.Sub(sample.StateChangedAt).Seconds()
	}

	contribution := WorkerIncarnationContribution{
		RunSeconds: runtimeSeconds, ActiveSeconds: activeSeconds, IdleSeconds: idleSeconds,
	}
	if sample.RequestedCPUMillicores != nil {
		contribution.RequestedCPUSeconds = runtimeSeconds * (float64(*sample.RequestedCPUMillicores) / 1000.0)
	}
	if sample.RequestedMemoryBytes != nil {
		contribution.RequestedMemoryByteSeconds = runtimeSeconds * float64(*sample.RequestedMemoryBytes)
	}
	if sample.RequestedEphemeralStorageBytes != nil {
		contribution.RequestedStorageByteSeconds = runtimeSeconds * float64(*sample.RequestedEphemeralStorageBytes)
	}
	return contribution
}

func BoundedWorkerPoolMode(value *string) string {
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

func BoundedWorkerCapacityClass(value *string) string {
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

func BoundedWorkerLifecycleState(value string) string {
	switch strings.TrimSpace(value) {
	case "idle", "active", "draining", "offline", "terminated":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

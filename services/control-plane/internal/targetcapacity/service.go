package targetcapacity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	minObservationTTL = 10 * time.Second
	maxObservationTTL = time.Hour
)

type Vector struct {
	Request   *int64
	Total     *int64
	Allocated *int64
	Available *int64
}

type Observation struct {
	TenantID          uuid.UUID
	ExecutionTargetID uuid.UUID
	TargetKind        string
	Source            string
	TotalPods         int64
	AllocatedPods     int64
	AvailablePods     int64
	SchedulableUnits  int64
	CPU               Vector
	Memory            Vector
	EphemeralStorage  Vector
	GPUResourceName   *string
	GPU               Vector
	ObservedAt        time.Time
	TTL               time.Duration
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Observe(ctx context.Context, input Observation) (persistence.ExecutionTargetCapacity, error) {
	if input.TenantID == uuid.Nil || input.ExecutionTargetID == uuid.Nil || strings.TrimSpace(input.TargetKind) != "kubernetes" {
		return persistence.ExecutionTargetCapacity{}, problem.New(400, "invalid_target_capacity_scope", "A tenant-owned Kubernetes Execution Target is required.")
	}
	input.TargetKind = strings.TrimSpace(input.TargetKind)
	input.Source = strings.TrimSpace(input.Source)
	if input.Source == "" || len(input.Source) > 160 {
		return persistence.ExecutionTargetCapacity{}, problem.New(400, "invalid_target_capacity_source", "Execution Target capacity source must be between 1 and 160 characters.")
	}
	if input.TTL < minObservationTTL || input.TTL > maxObservationTTL {
		return persistence.ExecutionTargetCapacity{}, problem.New(400, "invalid_target_capacity_ttl", "Execution Target capacity TTL must be between 10 seconds and 1 hour.")
	}
	if input.TotalPods < 0 || input.AllocatedPods < 0 || input.AvailablePods != remaining(input.TotalPods, input.AllocatedPods) ||
		input.SchedulableUnits < 0 || input.SchedulableUnits > input.AvailablePods {
		return persistence.ExecutionTargetCapacity{}, problem.New(400, "invalid_target_capacity_pods", "Execution Target Pod capacity counters are invalid.")
	}
	for _, vector := range []Vector{input.CPU, input.Memory, input.EphemeralStorage} {
		if !validVector(vector) {
			return persistence.ExecutionTargetCapacity{}, problem.New(400, "invalid_target_capacity_vector", "Execution Target resource capacity vector is invalid.")
		}
	}
	gpuName := normalizeGPUResourceName(input.GPUResourceName)
	if (gpuName == nil) != emptyVector(input.GPU) || (gpuName != nil && (!validVector(input.GPU) || input.GPU.Request == nil)) {
		return persistence.ExecutionTargetCapacity{}, problem.New(400, "invalid_target_capacity_gpu", "Execution Target GPU capacity requires a resource name and a complete non-zero request vector.")
	}
	observedAt := input.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = s.now()
	}

	var result persistence.ExecutionTargetCapacity
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var target persistence.ExecutionTarget
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND tenant_id = ? AND kind = ?", input.ExecutionTargetID, input.TenantID, input.TargetKind).
			Take(&target).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "execution_target_not_found", "Execution Target not found.")
		}
		if err != nil {
			return problem.Wrap(500, "execution_target_capacity_target_load_failed", "Execution Target capacity scope could not be loaded.", err)
		}
		if target.Status != "active" {
			return problem.New(409, "execution_target_capacity_target_inactive", "Execution Target capacity can only be published for an active target.")
		}

		var current persistence.ExecutionTargetCapacity
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_target_id = ?", input.ExecutionTargetID).Take(&current).Error
		now := s.now()
		version := int64(1)
		if err == nil {
			if !observedAt.After(current.ObservedAt) {
				return problem.New(409, "execution_target_capacity_observation_stale", "Execution Target capacity observedAt must advance monotonically.")
			}
			version = current.Version + 1
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "execution_target_capacity_load_failed", "Execution Target capacity authority could not be loaded.", err)
		}
		result = modelFromObservation(input, gpuName, observedAt, observedAt.Add(input.TTL), version, now)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if createErr := tx.WithContext(ctx).Create(&result).Error; createErr != nil {
				return problem.Wrap(409, "execution_target_capacity_create_rejected", "Execution Target capacity authority could not be created.", createErr)
			}
			return nil
		}
		updates := capacityUpdates(result)
		updated := tx.WithContext(ctx).Model(&persistence.ExecutionTargetCapacity{}).
			Where("execution_target_id = ? AND version = ?", input.ExecutionTargetID, current.Version).
			Updates(updates)
		if updated.Error != nil {
			return problem.Wrap(409, "execution_target_capacity_update_rejected", "Execution Target capacity authority could not be advanced.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return problem.New(409, "execution_target_capacity_version_conflict", "Execution Target capacity authority changed concurrently; retry the observation.")
		}
		return nil
	})
	return result, err
}

func validVector(vector Vector) bool {
	if emptyVector(vector) {
		return true
	}
	if vector.Request != nil && *vector.Request <= 0 {
		return false
	}
	if vector.Total == nil && vector.Allocated == nil && vector.Available == nil {
		return vector.Request != nil
	}
	if vector.Total == nil || vector.Allocated == nil || vector.Available == nil {
		return false
	}
	return *vector.Total >= 0 && *vector.Allocated >= 0 && *vector.Available == remaining(*vector.Total, *vector.Allocated)
}

func emptyVector(vector Vector) bool {
	return vector.Request == nil && vector.Total == nil && vector.Allocated == nil && vector.Available == nil
}

func remaining(total, allocated int64) int64 {
	if allocated >= total {
		return 0
	}
	return total - allocated
}

func normalizeGPUResourceName(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	return &normalized
}

func modelFromObservation(
	input Observation,
	gpuName *string,
	observedAt, expiresAt time.Time,
	version int64,
	updatedAt time.Time,
) persistence.ExecutionTargetCapacity {
	return persistence.ExecutionTargetCapacity{
		ExecutionTargetID: input.ExecutionTargetID, TenantID: input.TenantID,
		TargetKind: input.TargetKind, Source: input.Source,
		TotalPods: input.TotalPods, AllocatedPods: input.AllocatedPods,
		AvailablePods: input.AvailablePods, SchedulableUnits: input.SchedulableUnits,
		PodRequestCPUMillicores: input.CPU.Request, TotalCPUMillicores: input.CPU.Total,
		AllocatedCPUMillicores: input.CPU.Allocated, AvailableCPUMillicores: input.CPU.Available,
		PodRequestMemoryBytes: input.Memory.Request, TotalMemoryBytes: input.Memory.Total,
		AllocatedMemoryBytes: input.Memory.Allocated, AvailableMemoryBytes: input.Memory.Available,
		PodRequestEphemeralStorageBytes: input.EphemeralStorage.Request,
		TotalEphemeralStorageBytes:      input.EphemeralStorage.Total,
		AllocatedEphemeralStorageBytes:  input.EphemeralStorage.Allocated,
		AvailableEphemeralStorageBytes:  input.EphemeralStorage.Available,
		GPUResourceName:                 gpuName, PodRequestGPUUnits: input.GPU.Request,
		TotalGPUUnits: input.GPU.Total, AllocatedGPUUnits: input.GPU.Allocated,
		AvailableGPUUnits: input.GPU.Available,
		ObservedAt:        observedAt, ExpiresAt: expiresAt, Version: version, UpdatedAt: updatedAt,
	}
}

func capacityUpdates(model persistence.ExecutionTargetCapacity) map[string]any {
	return map[string]any{
		"source": model.Source, "total_pods": model.TotalPods, "allocated_pods": model.AllocatedPods,
		"available_pods": model.AvailablePods, "schedulable_units": model.SchedulableUnits,
		"pod_request_cpu_millicores": model.PodRequestCPUMillicores,
		"total_cpu_millicores":       model.TotalCPUMillicores, "allocated_cpu_millicores": model.AllocatedCPUMillicores,
		"available_cpu_millicores": model.AvailableCPUMillicores,
		"pod_request_memory_bytes": model.PodRequestMemoryBytes,
		"total_memory_bytes":       model.TotalMemoryBytes, "allocated_memory_bytes": model.AllocatedMemoryBytes,
		"available_memory_bytes":              model.AvailableMemoryBytes,
		"pod_request_ephemeral_storage_bytes": model.PodRequestEphemeralStorageBytes,
		"total_ephemeral_storage_bytes":       model.TotalEphemeralStorageBytes,
		"allocated_ephemeral_storage_bytes":   model.AllocatedEphemeralStorageBytes,
		"available_ephemeral_storage_bytes":   model.AvailableEphemeralStorageBytes,
		"gpu_resource_name":                   model.GPUResourceName, "pod_request_gpu_units": model.PodRequestGPUUnits,
		"total_gpu_units": model.TotalGPUUnits, "allocated_gpu_units": model.AllocatedGPUUnits,
		"available_gpu_units": model.AvailableGPUUnits,
		"observed_at":         model.ObservedAt, "expires_at": model.ExpiresAt,
		"version": model.Version, "updated_at": model.UpdatedAt,
	}
}

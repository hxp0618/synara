package executiontargets

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/targetcapacity"
)

const managedKubernetesTargetCapacityPublisherPrefix = "managed-kubernetes-target-capacity-publisher:"

type ManagedKubernetesTargetCapacityObservation struct {
	TenantID          uuid.UUID
	ExecutionTargetID uuid.UUID
	TotalPods         int64
	AllocatedPods     int64
	AvailablePods     int64
	SchedulableUnits  int64
	CPU               targetcapacity.Vector
	Memory            targetcapacity.Vector
	EphemeralStorage  targetcapacity.Vector
	GPUResourceName   *string
	GPU               targetcapacity.Vector
	ObservedAt        time.Time
}

type ManagedKubernetesTargetCapacityObserver func(context.Context, ManagedKubernetesTargetCapacityObservation) error

func kubernetesTargetCapacityFromQuota(
	tenantID, targetID uuid.UUID,
	configuration kubernetesTargetConfiguration,
	quota kubernetesResourceQuota,
) (ManagedKubernetesTargetCapacityObservation, error) {
	totalPods, err := requiredKubernetesCapacityQuantity(quota.Hard, "pods", 1)
	if err != nil {
		return ManagedKubernetesTargetCapacityObservation{}, err
	}
	allocatedPods, err := requiredKubernetesCapacityQuantity(quota.Used, "pods", 1)
	if err != nil {
		return ManagedKubernetesTargetCapacityObservation{}, err
	}
	if int64(configuration.MaxActivePods) < *totalPods {
		*totalPods = int64(configuration.MaxActivePods)
	}
	availablePods := capacityRemaining(*totalPods, *allocatedPods)
	observation := ManagedKubernetesTargetCapacityObservation{
		TenantID: tenantID, ExecutionTargetID: targetID,
		TotalPods: *totalPods, AllocatedPods: *allocatedPods,
		AvailablePods: availablePods, SchedulableUnits: availablePods,
	}
	observation.CPU, err = kubernetesCapacityVector(
		configuration.CPURequest, configuration.QuotaCPURequests,
		quota.Hard, quota.Used, "requests.cpu", 1000,
	)
	if err != nil {
		return ManagedKubernetesTargetCapacityObservation{}, err
	}
	observation.Memory, err = kubernetesCapacityVector(
		configuration.MemoryRequest, configuration.QuotaMemoryRequests,
		quota.Hard, quota.Used, "requests.memory", 1,
	)
	if err != nil {
		return ManagedKubernetesTargetCapacityObservation{}, err
	}
	observation.EphemeralStorage, err = kubernetesCapacityVector(
		configuration.EphemeralStorageRequest, configuration.QuotaEphemeralStorage,
		quota.Hard, quota.Used, "requests.ephemeral-storage", 1,
	)
	if err != nil {
		return ManagedKubernetesTargetCapacityObservation{}, err
	}
	if configuration.GPUResourceName != "" {
		gpuName := configuration.GPUResourceName
		observation.GPUResourceName = &gpuName
		observation.GPU, err = kubernetesCapacityVector(
			configuration.GPURequest, configuration.QuotaGPURequests,
			quota.Hard, quota.Used, "requests."+configuration.GPUResourceName, 1,
		)
		if err != nil {
			return ManagedKubernetesTargetCapacityObservation{}, err
		}
	}
	for _, vector := range []targetcapacity.Vector{observation.CPU, observation.Memory, observation.EphemeralStorage, observation.GPU} {
		if vector.Request == nil || vector.Available == nil {
			continue
		}
		units := *vector.Available / *vector.Request
		if units < observation.SchedulableUnits {
			observation.SchedulableUnits = units
		}
	}
	return observation, nil
}

func kubernetesCapacityVector(
	configuredRequest, configuredQuota string,
	hard, used map[string]string,
	key string,
	scale int64,
) (targetcapacity.Vector, error) {
	request, err := parseKubernetesRequestedQuantity(configuredRequest, scale)
	if err != nil {
		return targetcapacity.Vector{}, problem.New(502, "kubernetes_capacity_request_invalid", "Configured Kubernetes Pod resource request is invalid.")
	}
	if strings.TrimSpace(configuredQuota) == "" {
		return targetcapacity.Vector{Request: request}, nil
	}
	total, err := requiredKubernetesCapacityQuantity(hard, key, scale)
	if err != nil {
		return targetcapacity.Vector{}, err
	}
	allocated, err := requiredKubernetesCapacityQuantity(used, key, scale)
	if err != nil {
		return targetcapacity.Vector{}, err
	}
	available := capacityRemaining(*total, *allocated)
	return targetcapacity.Vector{Request: request, Total: total, Allocated: allocated, Available: &available}, nil
}

func requiredKubernetesCapacityQuantity(values map[string]string, key string, scale int64) (*int64, error) {
	raw, found := values[key]
	if !found {
		return nil, problem.New(
			502,
			"kubernetes_resource_quota_status_incomplete",
			fmt.Sprintf("Kubernetes ResourceQuota status is missing %s.", key),
		)
	}
	if strings.TrimSpace(raw) == "0" {
		zero := int64(0)
		return &zero, nil
	}
	value, err := parseKubernetesRequestedQuantity(raw, scale)
	if err != nil || value == nil {
		return nil, problem.New(502, "kubernetes_resource_quota_status_invalid", "Kubernetes ResourceQuota status contains an invalid resource quantity.")
	}
	return value, nil
}

func capacityRemaining(total, allocated int64) int64 {
	if allocated >= total {
		return 0
	}
	return total - allocated
}

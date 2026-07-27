package executiontargets

import (
	"testing"

	"github.com/google/uuid"
)

func TestKubernetesTargetCapacityUsesResourceQuotaVectors(t *testing.T) {
	configuration := kubernetesTargetConfiguration{
		MaxActivePods: 10,
		CPURequest:    "500m", QuotaCPURequests: "4",
		MemoryRequest: "1Gi", QuotaMemoryRequests: "8Gi",
		EphemeralStorageRequest: "2Gi", QuotaEphemeralStorage: "20Gi",
		GPUResourceName: "nvidia.com/gpu", GPURequest: "1", QuotaGPURequests: "4",
	}
	quota := kubernetesResourceQuota{
		Hard: map[string]string{
			"pods": "10", "requests.cpu": "4", "requests.memory": "8Gi",
			"requests.ephemeral-storage": "20Gi", "requests.nvidia.com/gpu": "4",
		},
		Used: map[string]string{
			"pods": "3", "requests.cpu": "1500m", "requests.memory": "3Gi",
			"requests.ephemeral-storage": "6Gi", "requests.nvidia.com/gpu": "1",
		},
	}
	observation, err := kubernetesTargetCapacityFromQuota(uuid.New(), uuid.New(), configuration, quota)
	if err != nil {
		t.Fatal(err)
	}
	if observation.TotalPods != 10 || observation.AllocatedPods != 3 || observation.AvailablePods != 7 ||
		observation.SchedulableUnits != 3 {
		t.Fatalf("Pod-equivalent capacity = %#v", observation)
	}
	if observation.CPU.Total == nil || *observation.CPU.Total != 4_000 ||
		observation.CPU.Available == nil || *observation.CPU.Available != 2_500 {
		t.Fatalf("CPU capacity = %#v", observation.CPU)
	}
	if observation.Memory.Available == nil || *observation.Memory.Available != 5*(1<<30) ||
		observation.EphemeralStorage.Available == nil || *observation.EphemeralStorage.Available != 14*(1<<30) {
		t.Fatalf("storage/memory capacity = memory:%#v ephemeral:%#v", observation.Memory, observation.EphemeralStorage)
	}
	if observation.GPUResourceName == nil || *observation.GPUResourceName != "nvidia.com/gpu" ||
		observation.GPU.Available == nil || *observation.GPU.Available != 3 {
		t.Fatalf("GPU capacity = name:%v vector:%#v", observation.GPUResourceName, observation.GPU)
	}
}

func TestKubernetesTargetCapacityFailsClosedOnIncompleteQuotaStatus(t *testing.T) {
	_, err := kubernetesTargetCapacityFromQuota(uuid.New(), uuid.New(), kubernetesTargetConfiguration{
		MaxActivePods: 2, CPURequest: "100m", QuotaCPURequests: "1",
	}, kubernetesResourceQuota{
		Hard: map[string]string{"pods": "2", "requests.cpu": "1"},
		Used: map[string]string{"pods": "0"},
	})
	if err == nil {
		t.Fatal("incomplete Kubernetes ResourceQuota status was accepted")
	}
}

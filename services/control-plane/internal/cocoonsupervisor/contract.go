package cocoonsupervisor

import "time"

const WorkerAttestationVersion = 1

// WorkerAttestation is the one-shot host-to-agentd capability binding. The
// supervisor writes it only after the exact Pod UID, VM ID, vsock transport and
// Workspace mount have been verified; agentd consumes and deletes the file.
type WorkerAttestation struct {
	Version            int       `json:"version"`
	PodUID             string    `json:"podUid"`
	VMID               string    `json:"vmId"`
	SupervisorInstance string    `json:"supervisorInstance"`
	ObservedAt         time.Time `json:"observedAt"`
	HostSupervisor     string    `json:"hostSupervisor"`
	ProviderTransport  string    `json:"providerTransport"`
	IsolationProfile   string    `json:"isolationProfile"`
}

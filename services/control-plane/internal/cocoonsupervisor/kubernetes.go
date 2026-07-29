package cocoonsupervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	targetLabel            = "synara.io/execution-target-id"
	executionLabel         = "synara.io/execution-id"
	generationLabel        = "synara.io/generation"
	workerModeLabel        = "synara.io/worker-mode"
	allocationBackendLabel = "synara.io/allocation-backend"
	assignedExecutionLabel = "synara.io/assigned-execution-id"
	sharedMemoryAnnotation = "vm.cocoonstack.io/shared-memory"
	vmIDAnnotation         = "vm.cocoonstack.io/id"

	maximumCommandOutputBytes = 4 << 20
)

type Pod struct {
	Name               string
	Namespace          string
	UID                uuid.UUID
	VMID               string
	ExecutionID        uuid.UUID
	Generation         int64
	ServiceAccountName string
	GuestImage         string
}

type podList struct {
	Items []podListItem `json:"items"`
}

type podListItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		NodeName                     string `json:"nodeName"`
		ServiceAccountName           string `json:"serviceAccountName"`
		AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
		Containers                   []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (d *Daemon) listAssignedPods(ctx context.Context) ([]Pod, error) {
	selector := targetLabel + "=" + d.config.ExecutionTargetID.String() + "," +
		allocationBackendLabel + "=sandbox-operator-cocoon"
	encoded, err := d.commandOutput(
		ctx, d.config.KubectlCommand,
		"-n", d.config.Namespace, "get", "pods",
		"--field-selector", "spec.nodeName="+d.config.VirtualNodeName,
		"-l", selector, "-o", "json",
	)
	if err != nil {
		return nil, fmt.Errorf("list assigned Cocoon Pods: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var response podList
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode assigned Cocoon Pods: %w", err)
	}
	result := make([]Pod, 0, len(response.Items))
	seen := make(map[uuid.UUID]struct{}, len(response.Items))
	for _, item := range response.Items {
		pod, eligible, err := d.validateAssignedPod(item)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		if _, duplicate := seen[pod.UID]; duplicate {
			return nil, fmt.Errorf("Kubernetes returned duplicate Pod UID %s", pod.UID)
		}
		seen[pod.UID] = struct{}{}
		result = append(result, pod)
	}
	return result, nil
}

func (d *Daemon) validateAssignedPod(item podListItem) (Pod, bool, error) {
	if item.Metadata.DeletionTimestamp != nil || item.Status.Phase != "Running" {
		return Pod{}, false, nil
	}
	ready := false
	for _, condition := range item.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			ready = true
			break
		}
	}
	if !ready {
		return Pod{}, false, nil
	}
	if item.Spec.NodeName != d.config.VirtualNodeName || item.Metadata.Namespace != d.config.Namespace ||
		item.Metadata.Labels[targetLabel] != d.config.ExecutionTargetID.String() ||
		item.Metadata.Labels[allocationBackendLabel] != "sandbox-operator-cocoon" ||
		item.Metadata.Labels[workerModeLabel] != "execution-pinned" ||
		!strings.EqualFold(strings.TrimSpace(item.Metadata.Annotations[sharedMemoryAnnotation]), "true") {
		return Pod{}, false, errors.New("assigned Cocoon Pod violates the immutable supervisor boundary")
	}
	podUID, podErr := uuid.Parse(strings.TrimSpace(item.Metadata.UID))
	executionID, executionErr := uuid.Parse(strings.TrimSpace(item.Metadata.Labels[executionLabel]))
	assignedExecutionID, assignedErr := uuid.Parse(strings.TrimSpace(item.Metadata.Labels[assignedExecutionLabel]))
	generation, generationErr := strconv.ParseInt(strings.TrimSpace(item.Metadata.Labels[generationLabel]), 10, 64)
	if podErr != nil || podUID == uuid.Nil || executionErr != nil || executionID == uuid.Nil ||
		assignedErr != nil || assignedExecutionID != executionID || generationErr != nil || generation < 1 {
		return Pod{}, false, errors.New("assigned Cocoon Pod identity or Generation is invalid")
	}
	vmID := strings.TrimSpace(item.Metadata.Annotations[vmIDAnnotation])
	serviceAccount := strings.TrimSpace(item.Spec.ServiceAccountName)
	if !safeVMID(vmID) || !validKubernetesName(item.Metadata.Name) || !validKubernetesName(serviceAccount) ||
		item.Spec.AutomountServiceAccountToken == nil || *item.Spec.AutomountServiceAccountToken || len(item.Spec.Containers) == 0 {
		return Pod{}, false, errors.New("assigned Cocoon Pod runtime identity is incomplete")
	}
	guestImage := strings.TrimSpace(item.Spec.Containers[0].Image)
	if !strings.Contains(guestImage, "@sha256:") || len(guestImage) > 512 {
		return Pod{}, false, errors.New("assigned Cocoon Pod guest image is not immutable")
	}
	return Pod{
		Name: item.Metadata.Name, Namespace: item.Metadata.Namespace, UID: podUID,
		VMID: vmID, ExecutionID: executionID, Generation: generation,
		ServiceAccountName: serviceAccount, GuestImage: guestImage,
	}, true, nil
}

func safeVMID(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "/\\\r\n\t\x00")
}

func (d *Daemon) requestBoundRegistrationToken(ctx context.Context, pod Pod) (string, error) {
	audience := "synara.execution-target." + d.config.ExecutionTargetID.String()
	encoded, err := d.commandOutput(
		ctx, d.config.KubectlCommand,
		"-n", pod.Namespace, "create", "token", pod.ServiceAccountName,
		"--audience", audience, "--duration", "10m",
		"--bound-object-kind", "Pod", "--bound-object-name", pod.Name,
		"--bound-object-uid", pod.UID.String(),
	)
	if err != nil {
		return "", fmt.Errorf("request Pod-bound registration token: %w", err)
	}
	token := strings.TrimSpace(string(encoded))
	if token == "" || len(token) > 64<<10 || strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("Pod-bound registration token response is invalid")
	}
	return token, nil
}

func (d *Daemon) publishNodeAttestation(ctx context.Context, ready bool) error {
	labels := []string{
		"synara.io/host-supervisor=" + HostSupervisorVersion,
		"synara.io/provider-transport=" + ProviderTransport,
		"synara.io/isolation-profile=" + IsolationProfile,
	}
	annotations := []string{
		"synara.io/host-supervisor-instance=" + d.instanceID.String(),
		"synara.io/host-supervisor-observed-at=" + d.now().UTC().Format(time.RFC3339Nano),
	}
	if !ready {
		labels = []string{
			"synara.io/host-supervisor-", "synara.io/provider-transport-", "synara.io/isolation-profile-",
		}
		annotations = []string{
			"synara.io/host-supervisor-instance-", "synara.io/host-supervisor-observed-at-",
		}
	}
	if _, err := d.commandOutput(ctx, d.config.KubectlCommand, append(
		[]string{"label", "node", d.config.VirtualNodeName, "--overwrite"}, labels...,
	)...); err != nil {
		return fmt.Errorf("publish Cocoon supervisor labels: %w", err)
	}
	if _, err := d.commandOutput(ctx, d.config.KubectlCommand, append(
		[]string{"annotate", "node", d.config.VirtualNodeName, "--overwrite"}, annotations...,
	)...); err != nil {
		return fmt.Errorf("publish Cocoon supervisor heartbeat: %w", err)
	}
	return nil
}

func (d *Daemon) commandOutput(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	var output bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, remaining: maximumCommandOutputBytes}
	command.Stderr = &limitedWriter{writer: &output, remaining: maximumCommandOutputBytes}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s exited: %w (%s)", filepathBase(name), err, strings.TrimSpace(output.String()))
	}
	return output.Bytes(), nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(payload []byte) (int, error) {
	original := len(payload)
	if len(payload) > w.remaining {
		payload = payload[:w.remaining]
	}
	if len(payload) > 0 {
		if _, err := w.writer.Write(payload); err != nil {
			return 0, err
		}
		w.remaining -= len(payload)
	}
	return original, nil
}

func filepathBase(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

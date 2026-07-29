package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/cocoonsupervisor"
)

const (
	cocoonSupervisorAttestationFileEnvironment = "SYNARA_AGENTD_COCOON_SUPERVISOR_ATTESTATION_FILE"
	maximumCocoonSupervisorAttestationBytes    = 4096
	cocoonSupervisorAttestationMaximumAge      = 45 * time.Second
	cocoonSupervisorAttestationFutureSkew      = 5 * time.Second
)

type CocoonSupervisorAttestation = cocoonsupervisor.WorkerAttestation

func loadCocoonSupervisorAttestation(
	path string,
	expectedPodUID string,
	runnerCommand []string,
	now time.Time,
) (*CocoonSupervisorAttestation, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return nil, errors.New("Cocoon supervisor attestation path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Cocoon supervisor attestation: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Cocoon supervisor attestation must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Cocoon supervisor attestation: %w", err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maximumCocoonSupervisorAttestationBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read Cocoon supervisor attestation: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Cocoon supervisor attestation: %w", closeErr)
	}
	if len(contents) > maximumCocoonSupervisorAttestationBytes {
		return nil, errors.New("Cocoon supervisor attestation is oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var attestation CocoonSupervisorAttestation
	if err := decoder.Decode(&attestation); err != nil {
		return nil, fmt.Errorf("decode Cocoon supervisor attestation: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("Cocoon supervisor attestation contains trailing JSON")
	}
	if err := validateCocoonSupervisorAttestation(attestation, expectedPodUID, runnerCommand, now); err != nil {
		return nil, err
	}
	return &attestation, nil
}

func validateCocoonSupervisorAttestation(
	attestation CocoonSupervisorAttestation,
	expectedPodUID string,
	runnerCommand []string,
	now time.Time,
) error {
	if attestation.Version != cocoonsupervisor.WorkerAttestationVersion ||
		attestation.HostSupervisor != cocoonsupervisor.HostSupervisorVersion ||
		attestation.ProviderTransport != cocoonsupervisor.ProviderTransport ||
		attestation.IsolationProfile != cocoonsupervisor.IsolationProfile {
		return errors.New("Cocoon supervisor attestation contract is unsupported")
	}
	expectedUID, expectedErr := uuid.Parse(strings.TrimSpace(expectedPodUID))
	attestedUID, attestedErr := uuid.Parse(strings.TrimSpace(attestation.PodUID))
	supervisorInstance, supervisorErr := uuid.Parse(strings.TrimSpace(attestation.SupervisorInstance))
	if expectedErr != nil || attestedErr != nil || expectedUID == uuid.Nil || attestedUID != expectedUID ||
		supervisorErr != nil || supervisorInstance == uuid.Nil {
		return errors.New("Cocoon supervisor attestation identity does not match the bound Pod")
	}
	attestation.VMID = strings.TrimSpace(attestation.VMID)
	if attestation.VMID == "" || len(attestation.VMID) > 128 || strings.ContainsAny(attestation.VMID, "\r\n\t\x00/\\") {
		return errors.New("Cocoon supervisor attestation VM identity is invalid")
	}
	now = now.UTC()
	observedAt := attestation.ObservedAt.UTC()
	if attestation.ObservedAt.IsZero() || observedAt.Before(now.Add(-cocoonSupervisorAttestationMaximumAge)) ||
		observedAt.After(now.Add(cocoonSupervisorAttestationFutureSkew)) {
		return errors.New("Cocoon supervisor attestation heartbeat is stale or in the future")
	}
	if !cocoonRunnerCommandBindsVM(runnerCommand, attestation.VMID) {
		return errors.New("Cocoon Provider transport command is not bound to the attested VM")
	}
	return nil
}

func cocoonRunnerCommandBindsVM(command []string, vmID string) bool {
	if len(command) < 6 || filepath.Base(command[0]) != "synara-cocoon-provider-transport" || command[1] != "host" {
		return false
	}
	vmBinding := 0
	separator := -1
	for index := 2; index < len(command); index++ {
		switch command[index] {
		case "--vm-id":
			if index+1 >= len(command) || command[index+1] != vmID {
				return false
			}
			vmBinding++
			index++
		case "--":
			separator = index
			index = len(command)
		}
	}
	return vmBinding == 1 && separator >= 0 && separator+1 < len(command) &&
		filepath.Base(command[separator+1]) == "provider-host"
}

func removeConsumedCocoonSupervisorAttestation(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncRegistrationTokenDirectory(filepath.Dir(path))
}

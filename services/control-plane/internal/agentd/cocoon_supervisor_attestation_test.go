package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/cocoonsupervisor"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestLoadConfigConsumesBoundCocoonSupervisorAttestation(t *testing.T) {
	root := t.TempDir()
	setAgentdConfigEnvironment(t, filepath.Join(root, "workspaces"), filepath.Join(root, "git-cache"))
	podUID := uuid.NewString()
	vmID := "YPBQMIFLFUYNGYRQX5YDOUOMRA"
	t.Setenv("SYNARA_EXECUTION_TARGET_KIND", string(platform.TargetKubernetes))
	t.Setenv(platform.KubernetesPIDsLimitEnvironment, "512")
	t.Setenv("SYNARA_AGENTD_INSTANCE_UID", podUID)
	t.Setenv("SYNARA_AGENTD_PRIVATE_TMP_ROOT", filepath.Join(root, "private-tmp"))
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN", "")
	t.Setenv("SYNARA_AGENTD_RUNNER_COMMAND_JSON", `[
		"/usr/local/bin/synara-cocoon-provider-transport","host","--vm-id","`+vmID+`","--","/usr/local/bin/provider-host"
	]`)
	registrationPath := filepath.Join(root, "registration-token")
	if err := os.WriteFile(registrationPath, []byte("pod-bound-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNARA_WORKER_REGISTRATION_TOKEN_FILE", registrationPath)
	attestationPath := filepath.Join(root, "cocoon-attestation.json")
	writeCocoonSupervisorAttestation(t, attestationPath, CocoonSupervisorAttestation{
		Version: cocoonsupervisor.WorkerAttestationVersion, PodUID: podUID, VMID: vmID,
		SupervisorInstance: uuid.NewString(), ObservedAt: time.Now().UTC(),
		HostSupervisor:    cocoonsupervisor.HostSupervisorVersion,
		ProviderTransport: cocoonsupervisor.ProviderTransport,
		IsolationProfile:  cocoonsupervisor.IsolationProfile,
	})
	t.Setenv(cocoonSupervisorAttestationFileEnvironment, attestationPath)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderOuterSandboxProfile != providerOuterSandboxMicroVM ||
		cfg.CocoonSupervisorAttestation == nil || cfg.CocoonSupervisorAttestation.VMID != vmID {
		t.Fatalf("Cocoon supervisor boundary was not loaded: %#v", cfg)
	}
	for _, path := range []string{registrationPath, attestationPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("one-shot supervisor input remains readable at %s: %v", path, err)
		}
	}
}

func TestLoadCocoonSupervisorAttestationFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	podUID := uuid.NewString()
	vmID := "YPBQMIFLFUYNGYRQX5YDOUOMRA"
	command := []string{
		"/usr/local/bin/synara-cocoon-provider-transport", "host", "--vm-id", vmID,
		"--", "/usr/local/bin/provider-host",
	}
	valid := CocoonSupervisorAttestation{
		Version: cocoonsupervisor.WorkerAttestationVersion, PodUID: podUID, VMID: vmID,
		SupervisorInstance: uuid.NewString(), ObservedAt: now,
		HostSupervisor:    cocoonsupervisor.HostSupervisorVersion,
		ProviderTransport: cocoonsupervisor.ProviderTransport,
		IsolationProfile:  cocoonsupervisor.IsolationProfile,
	}
	tests := []struct {
		name    string
		mutate  func(*CocoonSupervisorAttestation)
		command []string
		want    string
	}{
		{name: "wrong Pod", mutate: func(value *CocoonSupervisorAttestation) { value.PodUID = uuid.NewString() }, want: "bound Pod"},
		{name: "stale", mutate: func(value *CocoonSupervisorAttestation) { value.ObservedAt = now.Add(-46 * time.Second) }, want: "stale"},
		{name: "wrong transport", mutate: func(value *CocoonSupervisorAttestation) { value.ProviderTransport = "tcp" }, want: "unsupported"},
		{name: "wrong VM command", command: []string{"/usr/local/bin/synara-cocoon-provider-transport", "host", "--vm-id", "other", "--", "/usr/local/bin/provider-host"}, want: "attested VM"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			candidateCommand := command
			if test.command != nil {
				candidateCommand = test.command
			}
			if err := validateCocoonSupervisorAttestation(candidate, podUID, candidateCommand, now); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid attestation returned %v", err)
			}
		})
	}
}

func TestLoadCocoonSupervisorAttestationRejectsMutableOrUnknownInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCocoonSupervisorAttestation(path, uuid.NewString(), nil, time.Now()); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("mutable supervisor attestation returned %v", err)
	}
}

func writeCocoonSupervisorAttestation(t *testing.T, path string, value CocoonSupervisorAttestation) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsInvalidEnumAndScalarValues(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DEPLOYMENT_PROFILE", "unknown")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_DEPLOYMENT_PROFILE") {
		t.Fatalf("expected invalid profile error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOGIN_COOKIE_SECURE", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_LOGIN_COOKIE_SECURE") {
		t.Fatalf("expected invalid boolean error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CONTROL_PLANE_REPLICAS", "many")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_CONTROL_PLANE_REPLICAS") {
		t.Fatalf("expected invalid replica count error, got %v", err)
	}
}

func TestLoadRequiresExplicitPostgresConnection(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DEPLOYMENT_PROFILE", "single-node")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_DATABASE_URL is required") {
		t.Fatalf("expected missing database url error, got %v", err)
	}
}

func TestLoadParsesOptionalLocalAgentdConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", `["runner","--jsonl"]`)
	t.Setenv("SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT", "./test-workspaces")
	t.Setenv("SYNARA_LOCAL_AGENTD_RESTART_BACKOFF", "2s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalAgentdRunnerCommand) != 2 || cfg.LocalAgentdRunnerCommand[1] != "--jsonl" {
		t.Fatalf("unexpected local agentd runner command: %#v", cfg.LocalAgentdRunnerCommand)
	}
	if cfg.LocalAgentdRestartBackoff.String() != "2s" {
		t.Fatalf("unexpected local agentd restart backoff: %s", cfg.LocalAgentdRestartBackoff)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", `{}`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON") {
		t.Fatalf("expected invalid local agentd runner command error, got %v", err)
	}
}

func TestLoadValidatesCredentialKMSConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CREDENTIAL_MASTER_KEY", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CredentialKMSProvider != "local" || cfg.CredentialKMSKeyID != "local-v1" || len(cfg.CredentialKMSLocalKey) != 32 {
		t.Fatalf("unexpected local credential KMS config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CREDENTIAL_KMS_PROVIDER", "aws-kms")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_CREDENTIAL_KMS_KEY_ID") {
		t.Fatalf("expected missing AWS KMS key error, got %v", err)
	}
}

func TestLoadValidatesSSHProvisioningConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "https://synara.example.com/control-plane")
	t.Setenv("SYNARA_AGENTD_BINARY_PATH", "/tmp/synara-agentd")
	t.Setenv("SYNARA_SSH_PROVISION_TIMEOUT", "45s")
	t.Setenv("SYNARA_DOCKER_RECONCILE_INTERVAL", "7s")
	t.Setenv("SYNARA_KUBERNETES_RECONCILE_INTERVAL", "3s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicControlPlaneURL != "https://synara.example.com/control-plane" ||
		cfg.AgentdBinaryPath != "/tmp/synara-agentd" || cfg.SSHProvisionTimeout.String() != "45s" ||
		cfg.DockerReconcileInterval.String() != "7s" {
		t.Fatalf("unexpected SSH provisioning config: %#v", cfg)
	}
	if cfg.KubernetesReconcileInterval.String() != "3s" {
		t.Fatalf("unexpected Kubernetes reconcile interval: %s", cfg.KubernetesReconcileInterval)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "ssh://synara.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PUBLIC_CONTROL_PLANE_URL") {
		t.Fatalf("expected invalid public control-plane URL error, got %v", err)
	}
}

func TestLoadValidatesOutboxConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_OUTBOX_POLL_INTERVAL", "2s")
	t.Setenv("SYNARA_OUTBOX_CLAIM_TTL", "10s")
	t.Setenv("SYNARA_OUTBOX_BATCH_SIZE", "25")
	t.Setenv("SYNARA_OUTBOX_MAX_ATTEMPTS", "7")
	t.Setenv("SYNARA_OUTBOX_BASE_BACKOFF", "3s")
	t.Setenv("SYNARA_OUTBOX_MAX_BACKOFF", "2m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutboxPollInterval != 2*time.Second || cfg.OutboxClaimTTL != 10*time.Second ||
		cfg.OutboxBatchSize != 25 || cfg.OutboxMaxAttempts != 7 ||
		cfg.OutboxBaseBackoff != 3*time.Second || cfg.OutboxMaxBackoff != 2*time.Minute {
		t.Fatalf("unexpected outbox config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_OUTBOX_POLL_INTERVAL", "10s")
	t.Setenv("SYNARA_OUTBOX_CLAIM_TTL", "5s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_OUTBOX_CLAIM_TTL") {
		t.Fatalf("expected invalid outbox claim TTL error, got %v", err)
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SYNARA_DEPLOYMENT_PROFILE", "SYNARA_METADATA_STORE", "SYNARA_ARTIFACT_STORE",
		"SYNARA_QUEUE_DRIVER", "SYNARA_CONTROL_PLANE_REPLICAS", "SYNARA_WORKER_LEASES_ENABLED",
		"SYNARA_WORKER_FENCING_ENABLED", "SYNARA_DATABASE_URL", "SYNARA_LOGIN_COOKIE_SECURE",
		"SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP", "SYNARA_LOGIN_SESSION_TTL",
		"SYNARA_CONTROL_PLANE_SHUTDOWN_TIMEOUT", "SYNARA_WORKER_LEASE_TTL",
		"SYNARA_WORKER_HEARTBEAT_TIMEOUT", "SYNARA_WORKER_RECEIPT_TTL",
		"SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", "SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT",
		"SYNARA_LOCAL_AGENTD_RESTART_BACKOFF",
		"SYNARA_CREDENTIAL_KMS_PROVIDER", "SYNARA_CREDENTIAL_KMS_KEY_ID",
		"SYNARA_CREDENTIAL_MASTER_KEY", "SYNARA_CREDENTIAL_KMS_AWS_REGION",
		"SYNARA_PUBLIC_CONTROL_PLANE_URL", "SYNARA_AGENTD_BINARY_PATH",
		"SYNARA_SSH_PROVISION_TIMEOUT",
		"SYNARA_DOCKER_RECONCILE_INTERVAL",
		"SYNARA_KUBERNETES_RECONCILE_INTERVAL",
		"SYNARA_RETENTION_SWEEP_INTERVAL",
		"SYNARA_OUTBOX_POLL_INTERVAL", "SYNARA_OUTBOX_CLAIM_TTL",
		"SYNARA_OUTBOX_BATCH_SIZE", "SYNARA_OUTBOX_MAX_ATTEMPTS",
		"SYNARA_OUTBOX_BASE_BACKOFF", "SYNARA_OUTBOX_MAX_BACKOFF",
	} {
		t.Setenv(name, "")
	}
}

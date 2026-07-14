package config

import (
	"path/filepath"
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

func TestLoadRejectsEnterpriseDevBootstrapAndMissingPublicURL(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DEPLOYMENT_PROFILE", "enterprise")
	t.Setenv("SYNARA_DATABASE_URL", "postgres://synara:test@db/synara")
	t.Setenv("SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DEV_BOOTSTRAP must be false") {
		t.Fatalf("expected enterprise dev bootstrap rejection, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DEPLOYMENT_PROFILE", "enterprise")
	t.Setenv("SYNARA_DATABASE_URL", "postgres://synara:test@db/synara")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PUBLIC_CONTROL_PLANE_URL is required") {
		t.Fatalf("expected enterprise public URL requirement, got %v", err)
	}
}

func TestLoadValidatesCookieProxyAndIdleSessionConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOGIN_COOKIE_SECURE", "true")
	t.Setenv("SYNARA_LOGIN_COOKIE_DOMAIN", ".example.com")
	t.Setenv("SYNARA_LOGIN_COOKIE_PATH", "/control-plane")
	t.Setenv("SYNARA_LOGIN_COOKIE_SAME_SITE", "strict")
	t.Setenv("SYNARA_LOGIN_SESSION_IDLE_TTL", "12h")
	t.Setenv("SYNARA_TRUSTED_PROXY_CIDRS", "10.0.0.0/8,2001:db8::/32")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CookieDomain != ".example.com" || cfg.CookiePath != "/control-plane" || cfg.CookieSameSite != "strict" ||
		cfg.SessionIdleTTL != 12*time.Hour || len(cfg.TrustedProxyCIDRs) != 2 {
		t.Fatalf("unexpected cookie/proxy/session config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOGIN_COOKIE_SAME_SITE", "none")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COOKIE_SECURE") {
		t.Fatalf("expected SameSite=None secure cookie rejection, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOGIN_SESSION_TTL", "1h")
	t.Setenv("SYNARA_LOGIN_SESSION_IDLE_TTL", "2h")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SESSION_IDLE_TTL") {
		t.Fatalf("expected idle TTL rejection, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("expected trusted proxy CIDR rejection, got %v", err)
	}
}

func TestLoadRequiresHTTPSAndSecureCookiesOutsideLoopback(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "http://synara.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("expected non-loopback HTTPS requirement, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "https://synara.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COOKIE_SECURE") {
		t.Fatalf("expected secure cookie requirement, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "http://127.0.0.1:3780")
	if _, err := Load(); err != nil {
		t.Fatalf("loopback HTTP should be allowed: %v", err)
	}
}

func TestLoadValidatesDatabasePoolAndMigrationConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DATABASE_MAX_OPEN_CONNECTIONS", "40")
	t.Setenv("SYNARA_DATABASE_MAX_IDLE_CONNECTIONS", "10")
	t.Setenv("SYNARA_DATABASE_CONNECTION_MAX_LIFETIME", "45m")
	t.Setenv("SYNARA_DATABASE_CONNECTION_MAX_IDLE_TIME", "5m")
	t.Setenv("SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT", "20s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseMaxOpenConnections != 40 || cfg.DatabaseMaxIdleConnections != 10 ||
		cfg.DatabaseConnectionMaxLifetime != 45*time.Minute || cfg.DatabaseConnectionMaxIdleTime != 5*time.Minute ||
		cfg.DatabaseMigrationLockTimeout != 20*time.Second {
		t.Fatalf("unexpected database configuration: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DATABASE_MAX_OPEN_CONNECTIONS", "4")
	t.Setenv("SYNARA_DATABASE_MAX_IDLE_CONNECTIONS", "5")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_DATABASE_MAX_IDLE_CONNECTIONS") {
		t.Fatalf("expected invalid idle connection limit, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT", "0s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT") {
		t.Fatalf("expected invalid migration lock timeout, got %v", err)
	}
}

func TestLoadValidatesProviderCursorMaximumAge(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCursorMaximumAge != 30*24*time.Hour {
		t.Fatalf("default Provider Cursor maximum age = %s", cfg.ProviderCursorMaximumAge)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CURSOR_MAX_AGE", "48h")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCursorMaximumAge != 48*time.Hour {
		t.Fatalf("configured Provider Cursor maximum age = %s", cfg.ProviderCursorMaximumAge)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CURSOR_MAX_AGE", "0s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PROVIDER_CURSOR_MAX_AGE") {
		t.Fatalf("expected invalid Provider Cursor maximum age, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CURSOR_MAX_AGE", "8761h")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PROVIDER_CURSOR_MAX_AGE") {
		t.Fatalf("expected excessive Provider Cursor maximum age rejection, got %v", err)
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
	expectedGitCacheRoot := filepath.Join(filepath.Dir("./test-workspaces"), "git-cache")
	if cfg.LocalAgentdWorkspaceRoot != "./test-workspaces" || cfg.LocalAgentdGitCacheRoot != expectedGitCacheRoot {
		t.Fatalf("unexpected local agentd storage roots: workspace=%q gitCache=%q", cfg.LocalAgentdWorkspaceRoot, cfg.LocalAgentdGitCacheRoot)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", `["runner"]`)
	t.Setenv("SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT", "./test-workspaces")
	t.Setenv("SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT", "./test-git-cache")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalAgentdGitCacheRoot != "./test-git-cache" {
		t.Fatalf("unexpected explicit local agentd Git cache root: %q", cfg.LocalAgentdGitCacheRoot)
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
	t.Setenv("SYNARA_LOGIN_COOKIE_SECURE", "true")
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

func TestLoadValidatesSSEConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_SSE_POLL_INTERVAL", "3s")
	t.Setenv("SYNARA_SSE_HEARTBEAT_INTERVAL", "20s")
	t.Setenv("SYNARA_SSE_WRITE_TIMEOUT", "8s")
	t.Setenv("SYNARA_SSE_LEASE_TTL", "1m")
	t.Setenv("SYNARA_SSE_MAX_CONNECTIONS_PER_USER", "6")
	t.Setenv("SYNARA_SSE_MAX_CONNECTIONS_PER_TENANT", "300")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSEPollInterval != 3*time.Second || cfg.SSEHeartbeatInterval != 20*time.Second ||
		cfg.SSEWriteTimeout != 8*time.Second || cfg.SSELeaseTTL != time.Minute ||
		cfg.SSEMaxConnectionsPerUser != 6 || cfg.SSEMaxConnectionsPerTenant != 300 {
		t.Fatalf("unexpected SSE config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_SSE_HEARTBEAT_INTERVAL", "20s")
	t.Setenv("SYNARA_SSE_LEASE_TTL", "40s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_SSE_LEASE_TTL") {
		t.Fatalf("expected invalid SSE lease TTL error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_SSE_MAX_CONNECTIONS_PER_USER", "20")
	t.Setenv("SYNARA_SSE_MAX_CONNECTIONS_PER_TENANT", "10")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_SSE connection limits") {
		t.Fatalf("expected invalid SSE connection limits, got %v", err)
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SYNARA_DEPLOYMENT_PROFILE", "SYNARA_METADATA_STORE", "SYNARA_ARTIFACT_STORE",
		"SYNARA_QUEUE_DRIVER", "SYNARA_CONTROL_PLANE_REPLICAS", "SYNARA_WORKER_LEASES_ENABLED",
		"SYNARA_WORKER_FENCING_ENABLED", "SYNARA_DATABASE_URL", "SYNARA_LOGIN_COOKIE_SECURE",
		"SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP", "SYNARA_LOGIN_SESSION_TTL",
		"SYNARA_LOGIN_SESSION_IDLE_TTL", "SYNARA_LOGIN_COOKIE_NAME", "SYNARA_LOGIN_COOKIE_DOMAIN",
		"SYNARA_LOGIN_COOKIE_PATH", "SYNARA_LOGIN_COOKIE_SAME_SITE", "SYNARA_TRUSTED_PROXY_CIDRS",
		"SYNARA_CONTROL_PLANE_SHUTDOWN_TIMEOUT", "SYNARA_WORKER_LEASE_TTL",
		"SYNARA_DATABASE_MAX_OPEN_CONNECTIONS", "SYNARA_DATABASE_MAX_IDLE_CONNECTIONS",
		"SYNARA_DATABASE_CONNECTION_MAX_LIFETIME", "SYNARA_DATABASE_CONNECTION_MAX_IDLE_TIME",
		"SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT",
		"SYNARA_WORKER_HEARTBEAT_TIMEOUT", "SYNARA_WORKER_RECEIPT_TTL",
		"SYNARA_PROVIDER_CURSOR_MAX_AGE",
		"SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", "SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT",
		"SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT", "SYNARA_LOCAL_AGENTD_RESTART_BACKOFF",
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
		"SYNARA_SSE_POLL_INTERVAL", "SYNARA_SSE_HEARTBEAT_INTERVAL",
		"SYNARA_SSE_WRITE_TIMEOUT", "SYNARA_SSE_LEASE_TTL",
		"SYNARA_SSE_MAX_CONNECTIONS_PER_USER", "SYNARA_SSE_MAX_CONNECTIONS_PER_TENANT",
	} {
		t.Setenv(name, "")
	}
}

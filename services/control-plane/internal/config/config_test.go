package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/billing"
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

func TestLoadValidatesProviderCredentialAccessTTL(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCredentialAccessTTL != 5*time.Minute {
		t.Fatalf("default Provider Credential access TTL = %s, want 5m", cfg.ProviderCredentialAccessTTL)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_WORKER_LEASE_TTL", "30s")
	t.Setenv("SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL", "60s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL") {
		t.Fatalf("expected access TTL/Worker Lease safety error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL", "2h")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL") {
		t.Fatalf("expected bounded access TTL error, got %v", err)
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
	t.Setenv("SYNARA_KUBERNETES_POD_PENDING_FAILURE_THRESHOLD", "12s")
	t.Setenv("SYNARA_RESOURCE_LIFECYCLE_SWEEP_INTERVAL", "4s")
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
	if cfg.KubernetesPodPendingFailureThreshold.String() != "12s" {
		t.Fatalf("unexpected Kubernetes Pod Pending failure threshold: %s", cfg.KubernetesPodPendingFailureThreshold)
	}
	if cfg.ResourceLifecycleSweepInterval.String() != "4s" {
		t.Fatalf("unexpected Resource Lifecycle sweep interval: %s", cfg.ResourceLifecycleSweepInterval)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_KUBERNETES_RECONCILE_INTERVAL", "30s")
	t.Setenv("SYNARA_KUBERNETES_POD_PENDING_FAILURE_THRESHOLD", "20s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_KUBERNETES_POD_PENDING_FAILURE_THRESHOLD") {
		t.Fatalf("expected Pending threshold/reconcile interval safety error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "ssh://synara.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PUBLIC_CONTROL_PLANE_URL") {
		t.Fatalf("expected invalid public control-plane URL error, got %v", err)
	}
}

func TestLoadValidatesWorkerAutoRollbackConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkerAutoRollbackEnabled || cfg.WorkerAutoRollbackInterval != 10*time.Second {
		t.Fatalf("unexpected default Worker auto-rollback config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_WORKER_AUTO_ROLLBACK_ENABLED", "false")
	t.Setenv("SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL", "3s")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerAutoRollbackEnabled || cfg.WorkerAutoRollbackInterval != 3*time.Second {
		t.Fatalf("unexpected configured Worker auto-rollback config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_RESOURCE_LIFECYCLE_SWEEP_INTERVAL", "0s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_RESOURCE_LIFECYCLE_SWEEP_INTERVAL") {
		t.Fatalf("expected invalid Resource Lifecycle sweep interval error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL", "0s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL") {
		t.Fatalf("expected invalid Worker auto-rollback interval error, got %v", err)
	}
}

func TestLoadValidatesMetricRollupConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricRollupInterval != time.Minute || cfg.MetricRollupBatchSize != 500 {
		t.Fatalf("unexpected default metric rollup config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_METRIC_ROLLUP_INTERVAL", "15s")
	t.Setenv("SYNARA_METRIC_ROLLUP_BATCH_SIZE", "750")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricRollupInterval != 15*time.Second || cfg.MetricRollupBatchSize != 750 {
		t.Fatalf("unexpected metric rollup config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_METRIC_ROLLUP_BATCH_SIZE", "10001")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_METRIC_ROLLUP_BATCH_SIZE") {
		t.Fatalf("expected metric rollup batch bound error, got %v", err)
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

func TestLoadValidatesResourceLifecycleDefaultsAndOperatorBounds(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_RESOURCE_WAITING_KEEP_ALIVE", "20m")
	t.Setenv("SYNARA_RESOURCE_WAITING_KEEP_ALIVE_MIN_SECONDS", "300")
	t.Setenv("SYNARA_RESOURCE_WAITING_KEEP_ALIVE_MAX_SECONDS", "7200")
	t.Setenv("SYNARA_RESOURCE_SUSPEND_AFTER_IDLE", "45m")
	t.Setenv("SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME", "24h")
	t.Setenv("SYNARA_RESOURCE_WORKSPACE_RETENTION_DAYS", "14")
	t.Setenv("SYNARA_RESOURCE_WARM_POOL_MODE", "low-latency")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.ResourceLifecycle
	if policy.Defaults.WaitingKeepAliveSeconds != 1200 || policy.Defaults.SuspendAfterIdleSeconds != 2700 ||
		policy.Defaults.AbsoluteSessionLifetimeSeconds == nil || *policy.Defaults.AbsoluteSessionLifetimeSeconds != 86400 ||
		policy.Defaults.WorkspaceRetentionDays != 14 || policy.Defaults.WarmPoolMode != "low-latency" ||
		policy.Bounds.WaitingKeepAliveSeconds.Minimum != 300 || policy.Bounds.WaitingKeepAliveSeconds.Maximum != 7200 {
		t.Fatalf("unexpected Resource Lifecycle config: %#v", policy)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_RESOURCE_WAITING_KEEP_ALIVE", "30s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "waitingKeepAliveSeconds") {
		t.Fatalf("expected out-of-bounds Resource Lifecycle default, got %v", err)
	}
}

func TestLoadParsesBillingRuntimeConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	tenantID := uuid.New()
	targetID := uuid.New()
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "s3")
	t.Setenv("SYNARA_BILLING_S3_BUCKET", "billing-bucket")
	t.Setenv("SYNARA_BILLING_S3_REGION", "us-east-1")
	t.Setenv("SYNARA_BILLING_S3_ENDPOINT", "https://billing.example.com/")
	t.Setenv("SYNARA_BILLING_S3_ALLOW_CUSTOM_ENDPOINT", "true")
	t.Setenv("SYNARA_BILLING_BLOB_PREFIX", " exports/aws ")
	t.Setenv("SYNARA_BILLING_MAX_OBJECT_BYTES", "4096")
	t.Setenv("SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID", tenantID.String())
	t.Setenv("SYNARA_BILLING_S3_USE_PATH_STYLE", "true")
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":" AWS ",
		"externalImportId":" july-2026 ",
		"format":"aws-cur-csv",
		"objectKey":" reports/cur.csv ",
		"objectVersion":"version-1",
		"executionTargetIds":["`+targetID.String()+`"],
		"scheduleInterval":"30m",
		"reconcile":true,
		"estimateAfterImport":true
	}]`)
	t.Setenv("SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON", `{"allocations":[{
		"executionTargetId":"`+targetID.String()+`",
		"provider":" AWS ",
		"currencyCode":" usd ",
		"billingPeriodStartAt":"2026-07-01T00:00:00Z",
		"billingPeriodEndAt":"2026-08-01T00:00:00Z",
		"settlementDelay":"24h",
		"scheduleInterval":"6h"
	}]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Billing.MaxObjectBytes != 4096 || cfg.Billing.Source.Kind != billing.SourceKindS3 ||
		cfg.Billing.TariffOperatorTenantID != tenantID ||
		cfg.Billing.Source.S3Bucket != "billing-bucket" || cfg.Billing.Source.S3Region != "us-east-1" ||
		cfg.Billing.Source.S3Endpoint != "https://billing.example.com" ||
		cfg.Billing.Source.Prefix != "exports/aws" || !cfg.Billing.Source.S3UsePathStyle || !cfg.Billing.Source.S3AllowCustomEndpoint {
		t.Fatalf("unexpected billing source config: %#v", cfg.Billing.Source)
	}
	if len(cfg.Billing.Imports) != 1 {
		t.Fatalf("billing imports = %#v", cfg.Billing.Imports)
	}
	mapping := cfg.Billing.Imports[0]
	if mapping.TenantID != tenantID || mapping.Provider != "aws" || mapping.ExternalImportID != "july-2026" ||
		mapping.Format != billing.ExportObjectFormatAWSCURCSV || mapping.ObjectKey != "reports/cur.csv" ||
		mapping.ObjectVersion != "version-1" || mapping.ScheduleInterval != 30*time.Minute ||
		len(mapping.ExecutionTargetIDs) != 1 || mapping.ExecutionTargetIDs[0] != targetID ||
		!mapping.Reconcile || !mapping.EstimateAfterImport {
		t.Fatalf("unexpected billing import mapping: %#v", mapping)
	}
	if len(cfg.Billing.SharedAllocations) != 1 {
		t.Fatalf("billing shared allocations = %#v", cfg.Billing.SharedAllocations)
	}
	shared := cfg.Billing.SharedAllocations[0]
	if shared.ExecutionTargetID != targetID || shared.Provider != "aws" || shared.CurrencyCode != "USD" ||
		!shared.BillingPeriodStartAt.Equal(time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)) ||
		!shared.BillingPeriodEndAt.Equal(time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)) ||
		shared.SettlementDelay != 24*time.Hour || shared.ScheduleInterval != 6*time.Hour {
		t.Fatalf("unexpected billing shared allocation mapping: %#v", shared)
	}
}

func TestParseBillingSharedAllocationMappingsRejectsUnknownAndInvalidFields(t *testing.T) {
	targetID := uuid.New()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "unknown field",
			raw: `[{"executionTargetId":"` + targetID.String() + `","provider":"aws","currencyCode":"USD",` +
				`"billingPeriodStartAt":"2026-07-01T00:00:00Z","billingPeriodEndAt":"2026-08-01T00:00:00Z",` +
				`"settlementDelay":"24h","scheduleInterval":"6h","calendarz":"monthly-utc"}]`,
			want: "unknown field \"calendarz\"",
		},
		{
			name: "invalid start",
			raw: `[{"executionTargetId":"` + targetID.String() + `","provider":"aws","currencyCode":"USD",` +
				`"billingPeriodStartAt":"July","billingPeriodEndAt":"2026-08-01T00:00:00Z",` +
				`"settlementDelay":"24h","scheduleInterval":"6h"}]`,
			want: "billingPeriodStartAt must use RFC3339",
		},
		{
			name: "missing settlement delay",
			raw: `[{"executionTargetId":"` + targetID.String() + `","provider":"aws","currencyCode":"USD",` +
				`"billingPeriodStartAt":"2026-07-01T00:00:00Z","billingPeriodEndAt":"2026-08-01T00:00:00Z",` +
				`"scheduleInterval":"6h"}]`,
			want: "settlementDelay must be a valid duration",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseBillingSharedAllocationMappings(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestLoadParsesMonthlyUTCSharedAllocationSchedule(t *testing.T) {
	clearConfigEnvironment(t)
	targetID := uuid.New()
	t.Setenv("SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON", `{"allocations":[{
		"executionTargetId":"`+targetID.String()+`",
		"provider":"aws",
		"currencyCode":"USD",
		"calendar":"monthly-utc",
		"firstPeriodStartAt":"2026-01-01T00:00:00Z",
		"lastPeriodEndAt":"2027-01-01T00:00:00Z",
		"settlementDelay":"24h",
		"scheduleInterval":"6h"
	}]}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Billing.SharedAllocations) != 1 {
		t.Fatalf("monthly shared allocations = %#v", cfg.Billing.SharedAllocations)
	}
	shared := cfg.Billing.SharedAllocations[0]
	if shared.ExecutionTargetID != targetID || shared.Calendar != billing.SharedAllocationCalendarMonthlyUTC ||
		!shared.FirstPeriodStartAt.Equal(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)) ||
		shared.LastPeriodEndAt == nil ||
		!shared.LastPeriodEndAt.Equal(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)) ||
		shared.SettlementDelay != 24*time.Hour || shared.ScheduleInterval != 6*time.Hour {
		t.Fatalf("monthly shared allocation config = %#v", shared)
	}
}

func TestLoadRejectsMixedStaticAndCalendarSharedAllocationFields(t *testing.T) {
	clearConfigEnvironment(t)
	targetID := uuid.New()
	t.Setenv("SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON", `[{
		"executionTargetId":"`+targetID.String()+`",
		"provider":"aws",
		"currencyCode":"USD",
		"calendar":"monthly-utc",
		"firstPeriodStartAt":"2026-01-01T00:00:00Z",
		"billingPeriodStartAt":"2026-01-01T00:00:00Z",
		"billingPeriodEndAt":"2026-02-01T00:00:00Z",
		"settlementDelay":"24h",
		"scheduleInterval":"6h"
	}]`)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "cannot include a static billing period") {
		t.Fatalf("mixed static/calendar shared allocation error = %v", err)
	}
}

func TestParseBillingImportMappingsRejectsUnknownShapesAndFields(t *testing.T) {
	tenantID := uuid.New()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "unknown envelope field",
			raw:  `{"importz":[]}`,
			want: "unknown field \"importz\"",
		},
		{
			name: "unknown item field",
			raw: `[{
				"tenantId":"` + tenantID.String() + `",
				"provider":"aws",
				"externalImportId":"july-2026",
				"format":"aws-cur-csv",
				"objectKey":"cur.csv",
				"objectVersion":"v1",
				"unexpected":true
			}]`,
			want: "unknown field \"unexpected\"",
		},
		{
			name: "trailing content",
			raw:  `[] true`,
			want: "unexpected trailing JSON content",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseBillingImportMappings(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestLoadRejectsInvalidBillingConfiguration(t *testing.T) {
	tenantID := uuid.New()

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID", "not-a-uuid")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID must be a UUID") {
		t.Fatalf("expected invalid tariff operator tenant error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":"aws",
		"externalImportId":"july-2026",
		"format":"aws-cur-csv",
		"objectKey":"cur.csv"
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "require a configured billing blob source") {
		t.Fatalf("expected missing billing source error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "s3")
	t.Setenv("SYNARA_BILLING_S3_BUCKET", "billing-bucket")
	t.Setenv("SYNARA_BILLING_S3_REGION", "us-east-1")
	t.Setenv("SYNARA_BILLING_S3_ENDPOINT", "https://billing.example.com")
	t.Setenv("SYNARA_BILLING_S3_ALLOW_CUSTOM_ENDPOINT", "true")
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":"aws",
		"externalImportId":"july-2026",
		"format":"aws-cur-csv",
		"objectKey":"cur.csv"
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires objectVersion") {
		t.Fatalf("expected missing object version error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "s3")
	t.Setenv("SYNARA_BILLING_S3_BUCKET", "billing-bucket")
	t.Setenv("SYNARA_BILLING_S3_REGION", "us-east-1")
	t.Setenv("SYNARA_BILLING_S3_ENDPOINT", "https://billing.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "custom endpoint requires explicit enablement") {
		t.Fatalf("expected explicit custom endpoint allow error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "azure")
	t.Setenv("SYNARA_BILLING_AZURE_CONTAINER_URL", "http://127.0.0.1:10000/devstoreaccount1/invoices")
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":"azure",
		"externalImportId":"july-2026",
		"format":"azure-cost-normalized-json",
		"objectKey":"invoice.json",
		"objectVersion":"etag-1"
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must use HTTPS unless HTTP is explicitly allowed") {
		t.Fatalf("expected azure HTTPS requirement error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "local")
	t.Setenv("SYNARA_BILLING_LOCAL_BASE_DIR", t.TempDir())
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":"aws",
		"externalImportId":"duplicate",
		"format":"aws-cur-csv",
		"objectKey":"first.csv"
	},{
		"tenantId":"`+tenantID.String()+`",
		"provider":"aws",
		"externalImportId":"duplicate",
		"format":"aws-cur-csv",
		"objectKey":"second.csv"
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "is duplicated") {
		t.Fatalf("expected duplicate billing mapping error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "local")
	t.Setenv("SYNARA_BILLING_LOCAL_BASE_DIR", t.TempDir())
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":"aws",
		"externalImportId":"negative",
		"format":"aws-cur-csv",
		"objectKey":"negative.csv",
		"scheduleInterval":"-1h"
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "negative schedule interval") {
		t.Fatalf("expected negative schedule interval error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "local")
	t.Setenv("SYNARA_BILLING_LOCAL_BASE_DIR", t.TempDir())
	t.Setenv("SYNARA_BILLING_IMPORT_MAPPINGS_JSON", `[{
		"tenantId":"`+tenantID.String()+`",
		"provider":"gcp",
		"externalImportId":"provider-format-mismatch",
		"format":"aws-cur-csv",
		"objectKey":"cur.csv"
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires provider aws, not gcp") {
		t.Fatalf("expected billing provider/format mismatch error, got %v", err)
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
		"SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL",
		"SYNARA_PROVIDER_CURSOR_MAX_AGE",
		"SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", "SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT",
		"SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT", "SYNARA_LOCAL_AGENTD_RESTART_BACKOFF",
		"SYNARA_CREDENTIAL_KMS_PROVIDER", "SYNARA_CREDENTIAL_KMS_KEY_ID",
		"SYNARA_CREDENTIAL_MASTER_KEY", "SYNARA_CREDENTIAL_KMS_AWS_REGION",
		"SYNARA_PUBLIC_CONTROL_PLANE_URL", "SYNARA_AGENTD_BINARY_PATH",
		"SYNARA_SSH_PROVISION_TIMEOUT",
		"SYNARA_DOCKER_RECONCILE_INTERVAL",
		"SYNARA_KUBERNETES_RECONCILE_INTERVAL",
		"SYNARA_KUBERNETES_POD_PENDING_FAILURE_THRESHOLD",
		"SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON",
		"SYNARA_RESOURCE_LIFECYCLE_SWEEP_INTERVAL",
		"SYNARA_WORKER_AUTO_ROLLBACK_ENABLED", "SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL",
		"SYNARA_RETENTION_SWEEP_INTERVAL",
		"SYNARA_METRIC_ROLLUP_INTERVAL", "SYNARA_METRIC_ROLLUP_BATCH_SIZE",
		"SYNARA_OUTBOX_POLL_INTERVAL", "SYNARA_OUTBOX_CLAIM_TTL",
		"SYNARA_OUTBOX_BATCH_SIZE", "SYNARA_OUTBOX_MAX_ATTEMPTS",
		"SYNARA_OUTBOX_BASE_BACKOFF", "SYNARA_OUTBOX_MAX_BACKOFF",
		"SYNARA_SSE_POLL_INTERVAL", "SYNARA_SSE_HEARTBEAT_INTERVAL",
		"SYNARA_SSE_WRITE_TIMEOUT", "SYNARA_SSE_LEASE_TTL",
		"SYNARA_SSE_MAX_CONNECTIONS_PER_USER", "SYNARA_SSE_MAX_CONNECTIONS_PER_TENANT",
		"SYNARA_RESOURCE_WAITING_KEEP_ALIVE", "SYNARA_RESOURCE_SUSPEND_AFTER_IDLE",
		"SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME", "SYNARA_RESOURCE_WORKSPACE_RETENTION_DAYS",
		"SYNARA_RESOURCE_WARM_POOL_MODE",
		"SYNARA_RESOURCE_WAITING_KEEP_ALIVE_MIN_SECONDS", "SYNARA_RESOURCE_WAITING_KEEP_ALIVE_MAX_SECONDS",
		"SYNARA_RESOURCE_SUSPEND_AFTER_IDLE_MIN_SECONDS", "SYNARA_RESOURCE_SUSPEND_AFTER_IDLE_MAX_SECONDS",
		"SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME_MIN_SECONDS", "SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME_MAX_SECONDS",
		"SYNARA_RESOURCE_WORKSPACE_RETENTION_MIN_DAYS", "SYNARA_RESOURCE_WORKSPACE_RETENTION_MAX_DAYS",
		"SYNARA_BILLING_BLOB_SOURCE", "SYNARA_BILLING_LOCAL_BASE_DIR",
		"SYNARA_BILLING_S3_BUCKET", "SYNARA_BILLING_S3_REGION", "SYNARA_BILLING_S3_ENDPOINT",
		"SYNARA_BILLING_S3_USE_PATH_STYLE", "SYNARA_BILLING_S3_ALLOW_CUSTOM_ENDPOINT",
		"SYNARA_BILLING_S3_ALLOW_HTTP", "SYNARA_BILLING_GCS_BUCKET",
		"SYNARA_BILLING_AZURE_CONTAINER_URL", "SYNARA_BILLING_AZURE_ALLOW_HTTP",
		"SYNARA_BILLING_BLOB_PREFIX", "SYNARA_BILLING_MAX_OBJECT_BYTES",
		"SYNARA_BILLING_IMPORT_MAPPINGS_JSON", "SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON",
		"SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID",
	} {
		t.Setenv(name, "")
	}
}

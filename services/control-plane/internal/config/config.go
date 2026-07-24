package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Config struct {
	Platform                      platform.Config
	ListenAddress                 string
	DatabaseURL                   string
	DatabaseMaxOpenConnections    int
	DatabaseMaxIdleConnections    int
	DatabaseConnectionMaxLifetime time.Duration
	DatabaseConnectionMaxIdleTime time.Duration
	DatabaseMigrationLockTimeout  time.Duration
	SQLitePath                    string
	ArtifactLocalPath             string
	ArtifactBucket                string
	ArtifactRegion                string
	ArtifactEndpoint              string
	ArtifactPublicEndpoint        string
	ArtifactAccessKeyID           string
	ArtifactSecretAccessKey       string
	ArtifactSessionToken          string
	ArtifactUsePathStyle          bool
	ArtifactPresignTTL            time.Duration
	ArtifactMaxUploadBytes        int64
	InstallationID                string
	CookieName                    string
	CookieDomain                  string
	CookiePath                    string
	CookieSameSite                string
	CookieSecure                  bool
	DevBootstrapEnabled           bool
	SessionTTL                    time.Duration
	SessionIdleTTL                time.Duration
	TrustedProxyCIDRs             []netip.Prefix
	ShutdownTimeout               time.Duration
	WorkerRegistrationToken       string
	WorkerLeaseTTL                time.Duration
	WorkerHeartbeatTimeout        time.Duration
	WorkerReceiptTTL              time.Duration
	ProviderCursorKey             []byte
	ProviderCursorMaximumAge      time.Duration
	LocalAgentdRunnerCommand      []string
	LocalAgentdWorkspaceRoot      string
	LocalAgentdGitCacheRoot       string
	LocalAgentdRestartBackoff     time.Duration
	CredentialKMSProvider         string
	CredentialKMSKeyID            string
	CredentialKMSLocalKey         []byte
	CredentialKMSAWSRegion        string
	PublicControlPlaneURL         string
	AgentdBinaryPath              string
	SSHProvisionTimeout           time.Duration
	DockerReconcileInterval       time.Duration
	KubernetesReconcileInterval   time.Duration
	WorkerAutoRollbackEnabled     bool
	WorkerAutoRollbackInterval    time.Duration
	RetentionSweepInterval        time.Duration
	OutboxPollInterval            time.Duration
	OutboxClaimTTL                time.Duration
	OutboxBatchSize               int
	OutboxMaxAttempts             int
	OutboxBaseBackoff             time.Duration
	OutboxMaxBackoff              time.Duration
	SSEPollInterval               time.Duration
	SSEHeartbeatInterval          time.Duration
	SSEWriteTimeout               time.Duration
	SSELeaseTTL                   time.Duration
	SSEMaxConnectionsPerUser      int
	SSEMaxConnectionsPerTenant    int
}

func Load() (Config, error) {
	profile, err := platform.ParseDeploymentProfile(envOrDefault("SYNARA_DEPLOYMENT_PROFILE", string(platform.ProfilePersonal)))
	if err != nil {
		return Config{}, fmt.Errorf("SYNARA_DEPLOYMENT_PROFILE: %w", err)
	}
	platformConfig, err := platform.Defaults(profile)
	if err != nil {
		return Config{}, err
	}
	if value, ok := nonEmptyEnv("SYNARA_METADATA_STORE"); ok {
		platformConfig.MetadataStore, err = platform.ParseMetadataStore(value)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_METADATA_STORE: %w", err)
		}
	}
	if value, ok := nonEmptyEnv("SYNARA_ARTIFACT_STORE"); ok {
		platformConfig.ArtifactStore, err = platform.ParseArtifactStore(value)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_ARTIFACT_STORE: %w", err)
		}
	}
	if value, ok := nonEmptyEnv("SYNARA_QUEUE_DRIVER"); ok {
		platformConfig.QueueDriver, err = platform.ParseQueueDriver(value)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_QUEUE_DRIVER: %w", err)
		}
	}
	if platformConfig.ControlPlaneReplicas, err = envInt("SYNARA_CONTROL_PLANE_REPLICAS", platformConfig.ControlPlaneReplicas); err != nil {
		return Config{}, err
	}
	if platformConfig.LeaseEnabled, err = envBoolStrict("SYNARA_WORKER_LEASES_ENABLED", true); err != nil {
		return Config{}, err
	}
	if platformConfig.FencingEnabled, err = envBoolStrict("SYNARA_WORKER_FENCING_ENABLED", true); err != nil {
		return Config{}, err
	}
	if err := platformConfig.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid deployment profile configuration: %w", err)
	}

	defaultDataDir := envOrDefault("SYNARA_CONTROL_PLANE_DATA_DIR", "./data")
	cfg := Config{
		Platform:                platformConfig,
		ListenAddress:           envOrDefault("SYNARA_CONTROL_PLANE_LISTEN", ":3780"),
		DatabaseURL:             strings.TrimSpace(os.Getenv("SYNARA_DATABASE_URL")),
		SQLitePath:              envOrDefault("SYNARA_SQLITE_PATH", filepath.Join(defaultDataDir, "metadata.sqlite")),
		ArtifactLocalPath:       envOrDefault("SYNARA_ARTIFACT_LOCAL_PATH", filepath.Join(defaultDataDir, "artifacts")),
		ArtifactBucket:          envOrDefault("SYNARA_ARTIFACT_BUCKET", "synara-artifacts"),
		ArtifactRegion:          envOrDefault("SYNARA_ARTIFACT_REGION", "us-east-1"),
		ArtifactEndpoint:        strings.TrimSpace(os.Getenv("SYNARA_ARTIFACT_ENDPOINT")),
		ArtifactPublicEndpoint:  strings.TrimSpace(os.Getenv("SYNARA_ARTIFACT_PUBLIC_ENDPOINT")),
		ArtifactAccessKeyID:     strings.TrimSpace(os.Getenv("SYNARA_ARTIFACT_ACCESS_KEY_ID")),
		ArtifactSecretAccessKey: strings.TrimSpace(os.Getenv("SYNARA_ARTIFACT_SECRET_ACCESS_KEY")),
		ArtifactSessionToken:    strings.TrimSpace(os.Getenv("SYNARA_ARTIFACT_SESSION_TOKEN")),
		InstallationID:          strings.TrimSpace(os.Getenv("SYNARA_INSTALLATION_ID")),
		CookieName:              envOrDefault("SYNARA_LOGIN_COOKIE_NAME", "synara_login_session"),
		CookieDomain:            strings.TrimSpace(os.Getenv("SYNARA_LOGIN_COOKIE_DOMAIN")),
		CookiePath:              envOrDefault("SYNARA_LOGIN_COOKIE_PATH", "/"),
		CookieSameSite:          strings.ToLower(envOrDefault("SYNARA_LOGIN_COOKIE_SAME_SITE", "lax")),
		WorkerRegistrationToken: strings.TrimSpace(os.Getenv("SYNARA_WORKER_REGISTRATION_TOKEN")),
	}
	if cfg.CookieSecure, err = envBoolStrict("SYNARA_LOGIN_COOKIE_SECURE", false); err != nil {
		return Config{}, err
	}
	if cfg.DevBootstrapEnabled, err = envBoolStrict("SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP", false); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = envDurationStrict("SYNARA_LOGIN_SESSION_TTL", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.SessionIdleTTL, err = envDurationStrict("SYNARA_LOGIN_SESSION_IDLE_TTL", 7*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.TrustedProxyCIDRs, err = parseCIDRs(os.Getenv("SYNARA_TRUSTED_PROXY_CIDRS")); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDurationStrict("SYNARA_CONTROL_PLANE_SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMaxOpenConnections, err = envInt("SYNARA_DATABASE_MAX_OPEN_CONNECTIONS", 20); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMaxIdleConnections, err = envInt("SYNARA_DATABASE_MAX_IDLE_CONNECTIONS", 5); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseConnectionMaxLifetime, err = envDurationStrict("SYNARA_DATABASE_CONNECTION_MAX_LIFETIME", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseConnectionMaxIdleTime, err = envDurationStrict("SYNARA_DATABASE_CONNECTION_MAX_IDLE_TIME", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMigrationLockTimeout, err = envDurationStrict("SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerLeaseTTL, err = envDurationStrict("SYNARA_WORKER_LEASE_TTL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerHeartbeatTimeout, err = envDurationStrict("SYNARA_WORKER_HEARTBEAT_TIMEOUT", 90*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerReceiptTTL, err = envDurationStrict("SYNARA_WORKER_RECEIPT_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ProviderCursorMaximumAge, err = envDurationStrict("SYNARA_PROVIDER_CURSOR_MAX_AGE", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ArtifactUsePathStyle, err = envBoolStrict("SYNARA_ARTIFACT_USE_PATH_STYLE", cfg.Platform.ArtifactStore == platform.ArtifactMinIO); err != nil {
		return Config{}, err
	}
	if cfg.ArtifactPresignTTL, err = envDurationStrict("SYNARA_ARTIFACT_PRESIGN_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.ArtifactMaxUploadBytes, err = envInt64("SYNARA_ARTIFACT_MAX_UPLOAD_BYTES", 2<<30); err != nil {
		return Config{}, err
	}
	if raw := strings.TrimSpace(os.Getenv("SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON")); raw != "" {
		cfg.LocalAgentdRunnerCommand, err = validation.CommandJSON(raw)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON: %w", err)
		}
	}
	cfg.LocalAgentdWorkspaceRoot = envOrDefault("SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT", filepath.Join(defaultDataDir, "workspaces"))
	cfg.LocalAgentdGitCacheRoot = envOrDefault(
		"SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT", filepath.Join(filepath.Dir(cfg.LocalAgentdWorkspaceRoot), "git-cache"),
	)
	if cfg.LocalAgentdRestartBackoff, err = envDurationStrict("SYNARA_LOCAL_AGENTD_RESTART_BACKOFF", time.Second); err != nil {
		return Config{}, err
	}
	if encodedKey := strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_CURSOR_KEY")); encodedKey != "" {
		key, err := decodeKey(encodedKey, "SYNARA_PROVIDER_CURSOR_KEY")
		if err != nil {
			return Config{}, err
		}
		cfg.ProviderCursorKey = key
	}
	cfg.CredentialKMSProvider = strings.ToLower(strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_PROVIDER")))
	cfg.CredentialKMSKeyID = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_KEY_ID"))
	cfg.CredentialKMSAWSRegion = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_AWS_REGION"))
	cfg.PublicControlPlaneURL = strings.TrimRight(strings.TrimSpace(os.Getenv("SYNARA_PUBLIC_CONTROL_PLANE_URL")), "/")
	cfg.AgentdBinaryPath = envOrDefault("SYNARA_AGENTD_BINARY_PATH", "/usr/local/bin/synara-agentd")
	if cfg.SSHProvisionTimeout, err = envDurationStrict("SYNARA_SSH_PROVISION_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DockerReconcileInterval, err = envDurationStrict("SYNARA_DOCKER_RECONCILE_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.KubernetesReconcileInterval, err = envDurationStrict("SYNARA_KUBERNETES_RECONCILE_INTERVAL", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerAutoRollbackEnabled, err = envBoolStrict("SYNARA_WORKER_AUTO_ROLLBACK_ENABLED", true); err != nil {
		return Config{}, err
	}
	if cfg.WorkerAutoRollbackInterval, err = envDurationStrict("SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RetentionSweepInterval, err = envDurationStrict("SYNARA_RETENTION_SWEEP_INTERVAL", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.OutboxPollInterval, err = envDurationStrict("SYNARA_OUTBOX_POLL_INTERVAL", 500*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.OutboxClaimTTL, err = envDurationStrict("SYNARA_OUTBOX_CLAIM_TTL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.OutboxBatchSize, err = envInt("SYNARA_OUTBOX_BATCH_SIZE", 50); err != nil {
		return Config{}, err
	}
	if cfg.OutboxMaxAttempts, err = envInt("SYNARA_OUTBOX_MAX_ATTEMPTS", 12); err != nil {
		return Config{}, err
	}
	if cfg.OutboxBaseBackoff, err = envDurationStrict("SYNARA_OUTBOX_BASE_BACKOFF", time.Second); err != nil {
		return Config{}, err
	}
	if cfg.OutboxMaxBackoff, err = envDurationStrict("SYNARA_OUTBOX_MAX_BACKOFF", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.SSEPollInterval, err = envDurationStrict("SYNARA_SSE_POLL_INTERVAL", 2*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SSEHeartbeatInterval, err = envDurationStrict("SYNARA_SSE_HEARTBEAT_INTERVAL", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SSEWriteTimeout, err = envDurationStrict("SYNARA_SSE_WRITE_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SSELeaseTTL, err = envDurationStrict("SYNARA_SSE_LEASE_TTL", 45*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SSEMaxConnectionsPerUser, err = envInt("SYNARA_SSE_MAX_CONNECTIONS_PER_USER", 4); err != nil {
		return Config{}, err
	}
	if cfg.SSEMaxConnectionsPerTenant, err = envInt("SYNARA_SSE_MAX_CONNECTIONS_PER_TENANT", 200); err != nil {
		return Config{}, err
	}
	if encodedKey := strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_MASTER_KEY")); encodedKey != "" {
		cfg.CredentialKMSLocalKey, err = decodeKey(encodedKey, "SYNARA_CREDENTIAL_MASTER_KEY")
		if err != nil {
			return Config{}, err
		}
		if cfg.CredentialKMSProvider == "" {
			cfg.CredentialKMSProvider = "local"
		}
	}
	if cfg.CredentialKMSProvider == "local" && cfg.CredentialKMSKeyID == "" {
		cfg.CredentialKMSKeyID = "local-v1"
	}

	if cfg.Platform.MetadataStore == platform.MetadataPostgres && cfg.DatabaseURL == "" {
		return Config{}, errors.New("SYNARA_DATABASE_URL is required")
	}
	if cfg.Platform.MetadataStore == platform.MetadataSQLite && cfg.DatabaseURL != "" {
		return Config{}, errors.New("SYNARA_DATABASE_URL must not be set when SYNARA_METADATA_STORE=sqlite")
	}
	if strings.TrimSpace(cfg.SQLitePath) == "" {
		return Config{}, errors.New("SYNARA_SQLITE_PATH must not be empty")
	}
	if len(cfg.InstallationID) > 160 || strings.ContainsAny(cfg.InstallationID, "\r\n\t") {
		return Config{}, errors.New("SYNARA_INSTALLATION_ID must not exceed 160 characters or contain control whitespace")
	}
	if strings.TrimSpace(cfg.CookieName) == "" {
		return Config{}, errors.New("SYNARA_LOGIN_COOKIE_NAME must not be empty")
	}
	if !strings.HasPrefix(cfg.CookiePath, "/") || strings.ContainsAny(cfg.CookiePath, "\r\n\t") {
		return Config{}, errors.New("SYNARA_LOGIN_COOKIE_PATH must be an absolute cookie path")
	}
	if strings.ContainsAny(cfg.CookieDomain, "\r\n\t/:@") {
		return Config{}, errors.New("SYNARA_LOGIN_COOKIE_DOMAIN is invalid")
	}
	switch cfg.CookieSameSite {
	case "lax", "strict", "none":
	default:
		return Config{}, errors.New("SYNARA_LOGIN_COOKIE_SAME_SITE must be lax, strict, or none")
	}
	if cfg.CookieSameSite == "none" && !cfg.CookieSecure {
		return Config{}, errors.New("SYNARA_LOGIN_COOKIE_SECURE must be true when SYNARA_LOGIN_COOKIE_SAME_SITE=none")
	}
	if cfg.SessionTTL <= 0 {
		return Config{}, errors.New("SYNARA_LOGIN_SESSION_TTL must be positive")
	}
	if cfg.SessionIdleTTL <= 0 || cfg.SessionIdleTTL > cfg.SessionTTL {
		return Config{}, errors.New("SYNARA_LOGIN_SESSION_IDLE_TTL must be positive and no greater than SYNARA_LOGIN_SESSION_TTL")
	}
	if cfg.Platform.Profile == platform.ProfileEnterprise && cfg.DevBootstrapEnabled {
		return Config{}, errors.New("SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP must be false for enterprise deployments")
	}
	if cfg.DatabaseMaxOpenConnections <= 0 || cfg.DatabaseMaxOpenConnections > 1000 {
		return Config{}, errors.New("SYNARA_DATABASE_MAX_OPEN_CONNECTIONS must be between 1 and 1000")
	}
	if cfg.DatabaseMaxIdleConnections < 0 || cfg.DatabaseMaxIdleConnections > cfg.DatabaseMaxOpenConnections {
		return Config{}, errors.New("SYNARA_DATABASE_MAX_IDLE_CONNECTIONS must be between 0 and SYNARA_DATABASE_MAX_OPEN_CONNECTIONS")
	}
	if cfg.DatabaseConnectionMaxLifetime < 0 {
		return Config{}, errors.New("SYNARA_DATABASE_CONNECTION_MAX_LIFETIME must not be negative")
	}
	if cfg.DatabaseConnectionMaxIdleTime < 0 {
		return Config{}, errors.New("SYNARA_DATABASE_CONNECTION_MAX_IDLE_TIME must not be negative")
	}
	if cfg.DatabaseMigrationLockTimeout <= 0 {
		return Config{}, errors.New("SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT must be positive")
	}
	if cfg.WorkerLeaseTTL <= 0 {
		return Config{}, errors.New("SYNARA_WORKER_LEASE_TTL must be positive")
	}
	if cfg.WorkerHeartbeatTimeout <= cfg.WorkerLeaseTTL {
		return Config{}, errors.New("SYNARA_WORKER_HEARTBEAT_TIMEOUT must be greater than SYNARA_WORKER_LEASE_TTL")
	}
	if cfg.WorkerReceiptTTL <= 0 {
		return Config{}, errors.New("SYNARA_WORKER_RECEIPT_TTL must be positive")
	}
	if cfg.ProviderCursorMaximumAge <= 0 || cfg.ProviderCursorMaximumAge > 365*24*time.Hour {
		return Config{}, errors.New("SYNARA_PROVIDER_CURSOR_MAX_AGE must be positive and at most 8760h")
	}
	if cfg.ArtifactPresignTTL <= 0 || cfg.ArtifactPresignTTL > 24*time.Hour {
		return Config{}, errors.New("SYNARA_ARTIFACT_PRESIGN_TTL must be positive and at most 24h")
	}
	if cfg.ArtifactMaxUploadBytes <= 0 {
		return Config{}, errors.New("SYNARA_ARTIFACT_MAX_UPLOAD_BYTES must be positive")
	}
	if cfg.LocalAgentdRestartBackoff <= 0 {
		return Config{}, errors.New("SYNARA_LOCAL_AGENTD_RESTART_BACKOFF must be positive")
	}
	if len(cfg.LocalAgentdRunnerCommand) > 0 &&
		(strings.TrimSpace(cfg.LocalAgentdWorkspaceRoot) == "" || strings.TrimSpace(cfg.LocalAgentdGitCacheRoot) == "") {
		return Config{}, errors.New("SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT and SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT must not be empty")
	}
	if cfg.Platform.Profile == platform.ProfileEnterprise && cfg.PublicControlPlaneURL == "" {
		return Config{}, errors.New("SYNARA_PUBLIC_CONTROL_PLANE_URL is required for enterprise deployments")
	}
	if cfg.PublicControlPlaneURL != "" {
		parsed, parseErr := url.Parse(cfg.PublicControlPlaneURL)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return Config{}, errors.New("SYNARA_PUBLIC_CONTROL_PLANE_URL must be an HTTP(S) origin")
		}
		if !isLoopbackHostname(parsed.Hostname()) {
			if parsed.Scheme != "https" {
				return Config{}, errors.New("SYNARA_PUBLIC_CONTROL_PLANE_URL must use HTTPS outside loopback")
			}
			if !cfg.CookieSecure {
				return Config{}, errors.New("SYNARA_LOGIN_COOKIE_SECURE must be true outside loopback")
			}
		}
	}
	if strings.TrimSpace(cfg.AgentdBinaryPath) == "" {
		return Config{}, errors.New("SYNARA_AGENTD_BINARY_PATH must not be empty")
	}
	if cfg.SSHProvisionTimeout <= 0 {
		return Config{}, errors.New("SYNARA_SSH_PROVISION_TIMEOUT must be positive")
	}
	if cfg.DockerReconcileInterval <= 0 {
		return Config{}, errors.New("SYNARA_DOCKER_RECONCILE_INTERVAL must be positive")
	}
	if cfg.KubernetesReconcileInterval <= 0 {
		return Config{}, errors.New("SYNARA_KUBERNETES_RECONCILE_INTERVAL must be positive")
	}
	if cfg.WorkerAutoRollbackInterval <= 0 {
		return Config{}, errors.New("SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL must be positive")
	}
	if cfg.RetentionSweepInterval <= 0 {
		return Config{}, errors.New("SYNARA_RETENTION_SWEEP_INTERVAL must be positive")
	}
	if cfg.OutboxPollInterval <= 0 {
		return Config{}, errors.New("SYNARA_OUTBOX_POLL_INTERVAL must be positive")
	}
	if cfg.OutboxClaimTTL <= cfg.OutboxPollInterval {
		return Config{}, errors.New("SYNARA_OUTBOX_CLAIM_TTL must be greater than SYNARA_OUTBOX_POLL_INTERVAL")
	}
	if cfg.OutboxBatchSize <= 0 || cfg.OutboxBatchSize > 1000 {
		return Config{}, errors.New("SYNARA_OUTBOX_BATCH_SIZE must be between 1 and 1000")
	}
	if cfg.OutboxMaxAttempts <= 0 {
		return Config{}, errors.New("SYNARA_OUTBOX_MAX_ATTEMPTS must be positive")
	}
	if cfg.OutboxBaseBackoff <= 0 || cfg.OutboxMaxBackoff < cfg.OutboxBaseBackoff {
		return Config{}, errors.New("SYNARA_OUTBOX_MAX_BACKOFF must be greater than or equal to SYNARA_OUTBOX_BASE_BACKOFF")
	}
	if cfg.SSEPollInterval <= 0 {
		return Config{}, errors.New("SYNARA_SSE_POLL_INTERVAL must be positive")
	}
	if cfg.SSEHeartbeatInterval <= 0 {
		return Config{}, errors.New("SYNARA_SSE_HEARTBEAT_INTERVAL must be positive")
	}
	if cfg.SSEWriteTimeout <= 0 || cfg.SSEWriteTimeout >= cfg.SSEHeartbeatInterval {
		return Config{}, errors.New("SYNARA_SSE_WRITE_TIMEOUT must be positive and less than SYNARA_SSE_HEARTBEAT_INTERVAL")
	}
	if cfg.SSELeaseTTL <= 2*cfg.SSEHeartbeatInterval {
		return Config{}, errors.New("SYNARA_SSE_LEASE_TTL must be greater than twice SYNARA_SSE_HEARTBEAT_INTERVAL")
	}
	if cfg.SSEMaxConnectionsPerUser <= 0 || cfg.SSEMaxConnectionsPerTenant <= 0 ||
		cfg.SSEMaxConnectionsPerUser > cfg.SSEMaxConnectionsPerTenant {
		return Config{}, errors.New("SYNARA_SSE connection limits must be positive and the per-user limit must not exceed the per-tenant limit")
	}
	switch cfg.CredentialKMSProvider {
	case "":
		if cfg.CredentialKMSKeyID != "" || cfg.CredentialKMSAWSRegion != "" {
			return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_PROVIDER is required when credential KMS options are configured")
		}
	case "local":
		if len(cfg.CredentialKMSLocalKey) != 32 {
			return Config{}, errors.New("SYNARA_CREDENTIAL_MASTER_KEY is required for local credential KMS")
		}
	case "aws-kms":
		if cfg.CredentialKMSKeyID == "" {
			return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_KEY_ID is required for AWS KMS")
		}
		if len(cfg.CredentialKMSLocalKey) != 0 {
			return Config{}, errors.New("SYNARA_CREDENTIAL_MASTER_KEY must not be set with AWS KMS")
		}
	default:
		return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_PROVIDER must be local or aws-kms")
	}
	if strings.TrimSpace(cfg.ArtifactBucket) == "" {
		return Config{}, errors.New("SYNARA_ARTIFACT_BUCKET must not be empty")
	}
	if cfg.Platform.ArtifactStore == platform.ArtifactLocal && strings.TrimSpace(cfg.ArtifactLocalPath) == "" {
		return Config{}, errors.New("SYNARA_ARTIFACT_LOCAL_PATH must not be empty")
	}
	if cfg.Platform.ArtifactStore == platform.ArtifactMinIO {
		if cfg.ArtifactEndpoint == "" {
			return Config{}, errors.New("SYNARA_ARTIFACT_ENDPOINT is required for MinIO")
		}
		if cfg.ArtifactAccessKeyID == "" || cfg.ArtifactSecretAccessKey == "" {
			return Config{}, errors.New("SYNARA_ARTIFACT_ACCESS_KEY_ID and SYNARA_ARTIFACT_SECRET_ACCESS_KEY are required for MinIO")
		}
	}
	return cfg, nil
}

func parseCIDRs(value string) ([]netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	result := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("SYNARA_TRUSTED_PROXY_CIDRS must contain valid CIDR prefixes")
		}
		result = append(result, prefix.Masked())
	}
	return result, nil
}

func isLoopbackHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func decodeKey(value, name string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			if len(decoded) != 32 {
				return nil, fmt.Errorf("%s must decode to exactly 32 bytes", name)
			}
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%s must be base64 encoded", name)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func nonEmptyEnv(name string) (string, bool) {
	value := strings.TrimSpace(os.Getenv(name))
	return value, value != ""
}

func envBoolStrict(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

func envDurationStrict(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration", name)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

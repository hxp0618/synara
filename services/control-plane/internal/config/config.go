package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/billing"
	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Config struct {
	Platform                             platform.Config
	ResourceLifecycle                    lifecyclepolicy.Config
	ListenAddress                        string
	DatabaseURL                          string
	DatabaseMaxOpenConnections           int
	DatabaseMaxIdleConnections           int
	DatabaseConnectionMaxLifetime        time.Duration
	DatabaseConnectionMaxIdleTime        time.Duration
	DatabaseMigrationLockTimeout         time.Duration
	SQLitePath                           string
	ArtifactLocalPath                    string
	ArtifactBucket                       string
	ArtifactRegion                       string
	ArtifactEndpoint                     string
	ArtifactPublicEndpoint               string
	ArtifactAccessKeyID                  string
	ArtifactSecretAccessKey              string
	ArtifactSessionToken                 string
	ArtifactUsePathStyle                 bool
	ArtifactPresignTTL                   time.Duration
	ArtifactMaxUploadBytes               int64
	InstallationID                       string
	PlatformOperatorTenantID             uuid.UUID
	CookieName                           string
	CookieDomain                         string
	CookiePath                           string
	CookieSameSite                       string
	CookieSecure                         bool
	DevBootstrapEnabled                  bool
	SessionTTL                           time.Duration
	SessionIdleTTL                       time.Duration
	DesktopEnrollmentTTL                 time.Duration
	TrustedProxyCIDRs                    []netip.Prefix
	ShutdownTimeout                      time.Duration
	WorkerRegistrationToken              string
	WorkerLeaseTTL                       time.Duration
	WorkerHeartbeatTimeout               time.Duration
	WorkerReceiptTTL                     time.Duration
	ProviderCredentialAccessTTL          time.Duration
	ProviderCursorKey                    []byte
	ProviderCursorKeyID                  string
	ProviderCursorDecryptKeys            []ProviderCursorDecryptKeyConfig
	ProviderCursorMaximumAge             time.Duration
	LocalAgentdRunnerCommand             []string
	LocalAgentdWorkspaceRoot             string
	LocalAgentdGitCacheRoot              string
	LocalAgentdRestartBackoff            time.Duration
	CredentialKMSProvider                string
	CredentialKMSKeyID                   string
	CredentialKMSLocalKey                []byte
	CredentialKMSAWSRegion               string
	CredentialKMSEndpoint                string
	CredentialKMSCAFile                  string
	CredentialKMSClientCertFile          string
	CredentialKMSClientKeyFile           string
	CredentialKMSTimeout                 time.Duration
	CredentialKMSDecryptKeys             []CredentialKMSDecryptKeyConfig
	PublicControlPlaneURL                string
	PublicAdminURL                       string
	InternalStatusBoardURL               string
	InternalIncidentPublisherURL         string
	InternalIncidentPublisherHMACKeyID   string
	InternalIncidentPublisherHMACKey     []byte
	InternalIncidentPublisherTimeout     time.Duration
	CommercializationMode                string
	AgentdBinaryPath                     string
	DockerWorkerObservabilityRoot        string
	SSHWorkerObservabilityRoot           string
	SSHProvisionTimeout                  time.Duration
	DockerReconcileInterval              time.Duration
	KubernetesReconcileInterval          time.Duration
	KubernetesPodPendingFailureThreshold time.Duration
	WorkerPoolAutoscalingInterval        time.Duration
	WorkerPoolAutoscalingBatchSize       int
	PlatformRoutingPublishers            []routing.PlatformAuthorityPublisherConfig
	ResourceLifecycleSweepInterval       time.Duration
	WorkerAutoRollbackEnabled            bool
	WorkerAutoRollbackInterval           time.Duration
	RetentionSweepInterval               time.Duration
	MetricRollupInterval                 time.Duration
	MetricRollupBatchSize                int
	OutboxPollInterval                   time.Duration
	OutboxClaimTTL                       time.Duration
	OutboxBatchSize                      int
	OutboxMaxBatchSize                   int
	OutboxMaxConcurrency                 int
	OutboxScaleUpDepth                   int
	OutboxTargetDelay                    time.Duration
	OutboxThrottleDepth                  int
	OutboxMaxAttempts                    int
	OutboxBaseBackoff                    time.Duration
	OutboxMaxBackoff                     time.Duration
	SSEPollInterval                      time.Duration
	SSEHeartbeatInterval                 time.Duration
	SSEWriteTimeout                      time.Duration
	SSELeaseTTL                          time.Duration
	SSEMaxConnectionsPerUser             int
	SSEMaxConnectionsPerTenant           int
	Billing                              billing.RuntimeConfig
}

const CommercializationModeInternalSelfHosted = "internal-self-hosted"

type CredentialKMSDecryptKeyConfig struct {
	Provider       string
	KeyID          string
	LocalKey       []byte
	Region         string
	Endpoint       string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	Timeout        time.Duration
}

type ProviderCursorDecryptKeyConfig struct {
	KeyID string
	Key   []byte
}

type providerCursorDecryptKeyInput struct {
	KeyID          string `json:"keyId"`
	KeyEnvironment string `json:"keyEnvironment"`
}

type credentialKMSDecryptKeyInput struct {
	Provider            string `json:"provider"`
	KeyID               string `json:"keyId"`
	LocalKeyEnvironment string `json:"localKeyEnvironment,omitempty"`
	Region              string `json:"region,omitempty"`
	Endpoint            string `json:"endpoint,omitempty"`
	CAFile              string `json:"caFile,omitempty"`
	ClientCertFile      string `json:"clientCertFile,omitempty"`
	ClientKeyFile       string `json:"clientKeyFile,omitempty"`
	Timeout             string `json:"timeout,omitempty"`
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
	resourceLifecycle, err := loadResourceLifecycleConfig(profile)
	if err != nil {
		return Config{}, err
	}

	defaultDataDir := envOrDefault("SYNARA_CONTROL_PLANE_DATA_DIR", "./data")
	cfg := Config{
		Platform:                platformConfig,
		ResourceLifecycle:       resourceLifecycle,
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
	if rawTenantID, ok := nonEmptyEnv("SYNARA_PLATFORM_OPERATOR_TENANT_ID"); ok {
		cfg.PlatformOperatorTenantID, err = uuid.Parse(rawTenantID)
		if err != nil {
			return Config{}, errors.New("SYNARA_PLATFORM_OPERATOR_TENANT_ID must be a UUID")
		}
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
	if cfg.DesktopEnrollmentTTL, err = envDurationStrict("SYNARA_DESKTOP_ENROLLMENT_TTL", 3*time.Minute); err != nil {
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
	defaultProviderCredentialAccessTTL := max(5*time.Minute, 4*cfg.WorkerLeaseTTL)
	if cfg.ProviderCredentialAccessTTL, err = envDurationStrict(
		"SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL",
		defaultProviderCredentialAccessTTL,
	); err != nil {
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
	cfg.ProviderCursorKeyID = strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_CURSOR_KEY_ID"))
	if raw := strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON")); raw != "" {
		cfg.ProviderCursorDecryptKeys, err = parseProviderCursorDecryptKeys(
			raw, cfg.ProviderCursorKeyID, cfg.ProviderCursorKey,
		)
		if err != nil {
			return Config{}, err
		}
	}
	cfg.CredentialKMSProvider = strings.ToLower(strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_PROVIDER")))
	cfg.CredentialKMSKeyID = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_KEY_ID"))
	cfg.CredentialKMSAWSRegion = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_AWS_REGION"))
	cfg.CredentialKMSEndpoint = strings.TrimRight(strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_ENDPOINT")), "/")
	cfg.CredentialKMSCAFile = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_CA_FILE"))
	cfg.CredentialKMSClientCertFile = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_CLIENT_CERT_FILE"))
	cfg.CredentialKMSClientKeyFile = strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_CLIENT_KEY_FILE"))
	if cfg.CredentialKMSTimeout, err = envDurationStrict("SYNARA_CREDENTIAL_KMS_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	cfg.PublicControlPlaneURL = strings.TrimRight(strings.TrimSpace(os.Getenv("SYNARA_PUBLIC_CONTROL_PLANE_URL")), "/")
	cfg.PublicAdminURL = strings.TrimRight(strings.TrimSpace(os.Getenv("SYNARA_PUBLIC_ADMIN_URL")), "/")
	if strings.TrimSpace(os.Getenv("SYNARA_PUBLIC_STATUS_PAGE_URL")) != "" {
		return Config{}, errors.New("SYNARA_PUBLIC_STATUS_PAGE_URL is retired; use SYNARA_INTERNAL_STATUS_BOARD_URL for internal incident communications")
	}
	cfg.InternalStatusBoardURL = strings.TrimRight(strings.TrimSpace(os.Getenv("SYNARA_INTERNAL_STATUS_BOARD_URL")), "/")
	cfg.InternalIncidentPublisherURL = strings.TrimSpace(os.Getenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL"))
	cfg.InternalIncidentPublisherHMACKeyID = strings.TrimSpace(os.Getenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY_ID"))
	if encodedKey := strings.TrimSpace(os.Getenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY")); encodedKey != "" {
		cfg.InternalIncidentPublisherHMACKey, err = decodeKey(encodedKey, "SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY")
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.InternalIncidentPublisherTimeout, err = envDurationStrict("SYNARA_INTERNAL_INCIDENT_PUBLISHER_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	cfg.CommercializationMode = strings.ToLower(envOrDefault(
		"SYNARA_COMMERCIALIZATION_MODE",
		CommercializationModeInternalSelfHosted,
	))
	if cfg.CommercializationMode != CommercializationModeInternalSelfHosted {
		return Config{}, errors.New("SYNARA_COMMERCIALIZATION_MODE must be internal-self-hosted")
	}
	for _, name := range []string{
		"SYNARA_COMMERCIAL_BILLING_PROVIDER", "SYNARA_COMMERCIAL_BILLING_RETURN_URL",
		"SYNARA_STRIPE_SECRET_KEY", "SYNARA_STRIPE_WEBHOOK_SECRET", "SYNARA_STRIPE_PRICE_MAP_JSON",
		"SYNARA_STRIPE_AUTOMATIC_TAX_ENABLED", "SYNARA_STRIPE_CHECKOUT_TTL",
		"SYNARA_STRIPE_PORTAL_CONFIGURATION_ID",
	} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return Config{}, fmt.Errorf("%s is unsupported by the internal-self-hosted product", name)
		}
	}
	for _, legacy := range []struct{ old, current string }{
		{"SYNARA_BILLING_BLOB_SOURCE", "SYNARA_COST_ACCOUNTING_BLOB_SOURCE"},
		{"SYNARA_BILLING_LOCAL_BASE_DIR", "SYNARA_COST_ACCOUNTING_LOCAL_BASE_DIR"},
		{"SYNARA_BILLING_S3_BUCKET", "SYNARA_COST_ACCOUNTING_S3_BUCKET"},
		{"SYNARA_BILLING_S3_REGION", "SYNARA_COST_ACCOUNTING_S3_REGION"},
		{"SYNARA_BILLING_S3_ENDPOINT", "SYNARA_COST_ACCOUNTING_S3_ENDPOINT"},
		{"SYNARA_BILLING_S3_USE_PATH_STYLE", "SYNARA_COST_ACCOUNTING_S3_USE_PATH_STYLE"},
		{"SYNARA_BILLING_S3_ALLOW_CUSTOM_ENDPOINT", "SYNARA_COST_ACCOUNTING_S3_ALLOW_CUSTOM_ENDPOINT"},
		{"SYNARA_BILLING_S3_ALLOW_HTTP", "SYNARA_COST_ACCOUNTING_S3_ALLOW_HTTP"},
		{"SYNARA_BILLING_GCS_BUCKET", "SYNARA_COST_ACCOUNTING_GCS_BUCKET"},
		{"SYNARA_BILLING_AZURE_CONTAINER_URL", "SYNARA_COST_ACCOUNTING_AZURE_CONTAINER_URL"},
		{"SYNARA_BILLING_AZURE_ALLOW_HTTP", "SYNARA_COST_ACCOUNTING_AZURE_ALLOW_HTTP"},
		{"SYNARA_BILLING_BLOB_PREFIX", "SYNARA_COST_ACCOUNTING_BLOB_PREFIX"},
		{"SYNARA_BILLING_MAX_OBJECT_BYTES", "SYNARA_COST_ACCOUNTING_MAX_OBJECT_BYTES"},
		{"SYNARA_BILLING_IMPORT_MAPPINGS_JSON", "SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON"},
		{"SYNARA_BILLING_SHARED_ALLOCATION_MAPPINGS_JSON", "SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON"},
		{"SYNARA_BILLING_TARIFF_OPERATOR_TENANT_ID", "SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID"},
	} {
		if strings.TrimSpace(os.Getenv(legacy.old)) != "" {
			return Config{}, fmt.Errorf("%s is retired; use %s for internal cost accounting", legacy.old, legacy.current)
		}
	}
	cfg.AgentdBinaryPath = envOrDefault("SYNARA_AGENTD_BINARY_PATH", "/usr/local/bin/synara-agentd")
	cfg.DockerWorkerObservabilityRoot = strings.TrimSpace(os.Getenv("SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT"))
	cfg.SSHWorkerObservabilityRoot = strings.TrimSpace(os.Getenv("SYNARA_SSH_WORKER_OBSERVABILITY_ROOT"))
	if cfg.SSHProvisionTimeout, err = envDurationStrict("SYNARA_SSH_PROVISION_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DockerReconcileInterval, err = envDurationStrict("SYNARA_DOCKER_RECONCILE_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.KubernetesReconcileInterval, err = envDurationStrict("SYNARA_KUBERNETES_RECONCILE_INTERVAL", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.KubernetesPodPendingFailureThreshold, err = envDurationStrict("SYNARA_KUBERNETES_POD_PENDING_FAILURE_THRESHOLD", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.WorkerPoolAutoscalingInterval, err = envDurationStrict("SYNARA_WORKER_POOL_AUTOSCALING_INTERVAL", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerPoolAutoscalingBatchSize, err = envInt("SYNARA_WORKER_POOL_AUTOSCALING_BATCH_SIZE", 200); err != nil {
		return Config{}, err
	}
	if rawPublishers, ok := nonEmptyEnv("SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON"); ok {
		cfg.PlatformRoutingPublishers, err = parsePlatformRoutingPublishers(rawPublishers)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON: %w", err)
		}
	}
	if cfg.ResourceLifecycleSweepInterval, err = envDurationStrict("SYNARA_RESOURCE_LIFECYCLE_SWEEP_INTERVAL", 10*time.Second); err != nil {
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
	if cfg.MetricRollupInterval, err = envDurationStrict("SYNARA_METRIC_ROLLUP_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.MetricRollupBatchSize, err = envInt("SYNARA_METRIC_ROLLUP_BATCH_SIZE", 500); err != nil {
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
	if cfg.OutboxMaxBatchSize, err = envInt("SYNARA_OUTBOX_MAX_BATCH_SIZE", 500); err != nil {
		return Config{}, err
	}
	if cfg.OutboxMaxConcurrency, err = envInt("SYNARA_OUTBOX_MAX_CONCURRENCY", 8); err != nil {
		return Config{}, err
	}
	if cfg.OutboxScaleUpDepth, err = envInt("SYNARA_OUTBOX_SCALE_UP_DEPTH", 100); err != nil {
		return Config{}, err
	}
	if cfg.OutboxTargetDelay, err = envDurationStrict("SYNARA_OUTBOX_TARGET_DELAY", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.OutboxThrottleDepth, err = envInt("SYNARA_OUTBOX_THROTTLE_DEPTH", 100000); err != nil {
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
	cfg.Billing = billing.RuntimeConfig{
		MaxObjectBytes: 16 << 20,
		Source: billing.SourceConfig{
			Kind:              strings.ToLower(strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE"))),
			LocalBaseDir:      strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_LOCAL_BASE_DIR")),
			S3Bucket:          strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_S3_BUCKET")),
			S3Region:          strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_S3_REGION")),
			S3Endpoint:        strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_S3_ENDPOINT")),
			GCSBucket:         strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_GCS_BUCKET")),
			AzureContainerURL: strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_AZURE_CONTAINER_URL")),
			Prefix:            strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_BLOB_PREFIX")),
		},
	}
	if rawTenantID, ok := nonEmptyEnv("SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID"); ok {
		cfg.Billing.TariffOperatorTenantID, err = uuid.Parse(rawTenantID)
		if err != nil {
			return Config{}, errors.New("SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID must be a UUID")
		}
	}
	if cfg.Billing.MaxObjectBytes, err = envInt64("SYNARA_COST_ACCOUNTING_MAX_OBJECT_BYTES", cfg.Billing.MaxObjectBytes); err != nil {
		return Config{}, err
	}
	if cfg.Billing.Source.S3UsePathStyle, err = envBoolStrict("SYNARA_COST_ACCOUNTING_S3_USE_PATH_STYLE", false); err != nil {
		return Config{}, err
	}
	if cfg.Billing.Source.S3AllowCustomEndpoint, err = envBoolStrict("SYNARA_COST_ACCOUNTING_S3_ALLOW_CUSTOM_ENDPOINT", false); err != nil {
		return Config{}, err
	}
	if cfg.Billing.Source.S3AllowHTTP, err = envBoolStrict("SYNARA_COST_ACCOUNTING_S3_ALLOW_HTTP", false); err != nil {
		return Config{}, err
	}
	if cfg.Billing.Source.AzureAllowHTTP, err = envBoolStrict("SYNARA_COST_ACCOUNTING_AZURE_ALLOW_HTTP", false); err != nil {
		return Config{}, err
	}
	if rawMappings, ok := nonEmptyEnv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON"); ok {
		cfg.Billing.Imports, err = parseBillingImportMappings(rawMappings)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON: %w", err)
		}
	}
	if rawMappings, ok := nonEmptyEnv("SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON"); ok {
		cfg.Billing.SharedAllocations, err = parseBillingSharedAllocationMappings(rawMappings)
		if err != nil {
			return Config{}, fmt.Errorf("SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON: %w", err)
		}
	}
	if cfg.Billing, err = cfg.Billing.Normalize(); err != nil {
		return Config{}, fmt.Errorf("invalid cost accounting configuration: %w", err)
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
	if raw := strings.TrimSpace(os.Getenv("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON")); raw != "" {
		cfg.CredentialKMSDecryptKeys, err = parseCredentialKMSDecryptKeys(
			raw,
			cfg.CredentialKMSProvider,
			cfg.CredentialKMSKeyID,
			cfg.CredentialKMSLocalKey,
		)
		if err != nil {
			return Config{}, err
		}
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
	if cfg.DesktopEnrollmentTTL < time.Minute || cfg.DesktopEnrollmentTTL > 5*time.Minute {
		return Config{}, errors.New("SYNARA_DESKTOP_ENROLLMENT_TTL must be between 1m and 5m")
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
	if cfg.ProviderCredentialAccessTTL <= 2*cfg.WorkerLeaseTTL || cfg.ProviderCredentialAccessTTL > time.Hour {
		return Config{}, errors.New("SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL must be greater than twice SYNARA_WORKER_LEASE_TTL and at most 1h")
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
	if err := validatePublicHTTPURL("SYNARA_PUBLIC_CONTROL_PLANE_URL", cfg.PublicControlPlaneURL, cfg.CookieSecure); err != nil {
		return Config{}, err
	}
	if err := validatePublicHTTPURL("SYNARA_PUBLIC_ADMIN_URL", cfg.PublicAdminURL, cfg.CookieSecure); err != nil {
		return Config{}, err
	}
	if err := validateInternalStatusBoardURL(
		"SYNARA_INTERNAL_STATUS_BOARD_URL",
		cfg.InternalStatusBoardURL,
		cfg.PublicControlPlaneURL,
		cfg.PublicAdminURL,
	); err != nil {
		return Config{}, err
	}
	if err := validateInternalIncidentPublisher(cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.AgentdBinaryPath) == "" {
		return Config{}, errors.New("SYNARA_AGENTD_BINARY_PATH must not be empty")
	}
	for name, root := range map[string]string{
		"SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT": cfg.DockerWorkerObservabilityRoot,
		"SYNARA_SSH_WORKER_OBSERVABILITY_ROOT":    cfg.SSHWorkerObservabilityRoot,
	} {
		if root != "" && (!filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator)) {
			return Config{}, fmt.Errorf("%s must be a non-root absolute directory", name)
		}
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
	if cfg.KubernetesPodPendingFailureThreshold < cfg.KubernetesReconcileInterval ||
		cfg.KubernetesPodPendingFailureThreshold > 24*time.Hour {
		return Config{}, errors.New("SYNARA_KUBERNETES_POD_PENDING_FAILURE_THRESHOLD must be between SYNARA_KUBERNETES_RECONCILE_INTERVAL and 24h")
	}
	if cfg.ResourceLifecycleSweepInterval <= 0 {
		return Config{}, errors.New("SYNARA_RESOURCE_LIFECYCLE_SWEEP_INTERVAL must be positive")
	}
	if cfg.WorkerAutoRollbackInterval <= 0 {
		return Config{}, errors.New("SYNARA_WORKER_AUTO_ROLLBACK_INTERVAL must be positive")
	}
	if cfg.RetentionSweepInterval <= 0 {
		return Config{}, errors.New("SYNARA_RETENTION_SWEEP_INTERVAL must be positive")
	}
	if cfg.MetricRollupInterval <= 0 {
		return Config{}, errors.New("SYNARA_METRIC_ROLLUP_INTERVAL must be positive")
	}
	if cfg.MetricRollupBatchSize <= 0 || cfg.MetricRollupBatchSize > 10000 {
		return Config{}, errors.New("SYNARA_METRIC_ROLLUP_BATCH_SIZE must be between 1 and 10000")
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
	if cfg.OutboxMaxBatchSize < cfg.OutboxBatchSize || cfg.OutboxMaxBatchSize > 10000 {
		return Config{}, errors.New("SYNARA_OUTBOX_MAX_BATCH_SIZE must be between SYNARA_OUTBOX_BATCH_SIZE and 10000")
	}
	if cfg.OutboxMaxConcurrency < 1 || cfg.OutboxMaxConcurrency > 128 {
		return Config{}, errors.New("SYNARA_OUTBOX_MAX_CONCURRENCY must be between 1 and 128")
	}
	if cfg.OutboxScaleUpDepth < 1 || cfg.OutboxThrottleDepth <= cfg.OutboxScaleUpDepth {
		return Config{}, errors.New("SYNARA_OUTBOX_SCALE_UP_DEPTH must be positive and SYNARA_OUTBOX_THROTTLE_DEPTH must be greater")
	}
	if cfg.OutboxTargetDelay < time.Millisecond || cfg.OutboxTargetDelay > time.Hour {
		return Config{}, errors.New("SYNARA_OUTBOX_TARGET_DELAY must be between 1ms and 1h")
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
	if cfg.ProviderCursorKeyID != "" {
		if len(cfg.ProviderCursorKeyID) > 200 || strings.ContainsAny(cfg.ProviderCursorKeyID, "\r\n\t") {
			return Config{}, errors.New("SYNARA_PROVIDER_CURSOR_KEY_ID is invalid")
		}
		if len(cfg.ProviderCursorKey) != 32 {
			return Config{}, errors.New("SYNARA_PROVIDER_CURSOR_KEY is required when SYNARA_PROVIDER_CURSOR_KEY_ID is configured")
		}
	}
	switch cfg.CredentialKMSProvider {
	case "":
		if cfg.CredentialKMSKeyID != "" || cfg.CredentialKMSAWSRegion != "" || credentialKMSTLSConfigured(cfg) {
			return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_PROVIDER is required when credential KMS options are configured")
		}
	case "local":
		if len(cfg.CredentialKMSLocalKey) != 32 {
			return Config{}, errors.New("SYNARA_CREDENTIAL_MASTER_KEY is required for local credential KMS")
		}
		if cfg.CredentialKMSAWSRegion != "" || credentialKMSTLSConfigured(cfg) {
			return Config{}, errors.New("local credential KMS must not configure AWS or Synara KMS options")
		}
	case "aws-kms":
		if cfg.CredentialKMSKeyID == "" {
			return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_KEY_ID is required for AWS KMS")
		}
		if len(cfg.CredentialKMSLocalKey) != 0 {
			return Config{}, errors.New("SYNARA_CREDENTIAL_MASTER_KEY must not be set with AWS KMS")
		}
		if credentialKMSTLSConfigured(cfg) {
			return Config{}, errors.New("AWS credential KMS must not configure Synara KMS options")
		}
	case "synara-kms":
		if cfg.CredentialKMSKeyID == "" || cfg.CredentialKMSEndpoint == "" || cfg.CredentialKMSCAFile == "" ||
			cfg.CredentialKMSClientCertFile == "" || cfg.CredentialKMSClientKeyFile == "" {
			return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_KEY_ID, SYNARA_CREDENTIAL_KMS_ENDPOINT, SYNARA_CREDENTIAL_KMS_CA_FILE, SYNARA_CREDENTIAL_KMS_CLIENT_CERT_FILE and SYNARA_CREDENTIAL_KMS_CLIENT_KEY_FILE are required for Synara KMS")
		}
		if len(cfg.CredentialKMSLocalKey) != 0 || cfg.CredentialKMSAWSRegion != "" {
			return Config{}, errors.New("Synara credential KMS must not configure local or AWS KMS key material")
		}
	default:
		return Config{}, errors.New("SYNARA_CREDENTIAL_KMS_PROVIDER must be local, aws-kms or synara-kms")
	}
	if strings.TrimSpace(cfg.ArtifactBucket) == "" {
		return Config{}, errors.New("SYNARA_ARTIFACT_BUCKET must not be empty")
	}
	artifactAccessKeyConfigured := cfg.ArtifactAccessKeyID != ""
	artifactSecretKeyConfigured := cfg.ArtifactSecretAccessKey != ""
	if artifactAccessKeyConfigured != artifactSecretKeyConfigured {
		return Config{}, errors.New("SYNARA_ARTIFACT_ACCESS_KEY_ID and SYNARA_ARTIFACT_SECRET_ACCESS_KEY must be configured together")
	}
	if cfg.ArtifactSessionToken != "" && !artifactAccessKeyConfigured {
		return Config{}, errors.New("SYNARA_ARTIFACT_SESSION_TOKEN requires an Artifact access key pair")
	}
	if cfg.Platform.ArtifactStore == platform.ArtifactLocal && strings.TrimSpace(cfg.ArtifactLocalPath) == "" {
		return Config{}, errors.New("SYNARA_ARTIFACT_LOCAL_PATH must not be empty")
	}
	if cfg.Platform.ArtifactStore == platform.ArtifactLocal && (artifactAccessKeyConfigured || cfg.ArtifactSessionToken != "") {
		return Config{}, errors.New("Artifact S3 credentials must not be configured for Local Artifact storage")
	}
	if cfg.Platform.ArtifactStore != platform.ArtifactLocal {
		if cfg.ArtifactPresignTTL > 15*time.Minute {
			return Config{}, errors.New("SYNARA_ARTIFACT_PRESIGN_TTL must be at most 15m for MinIO/S3 Artifact storage")
		}
		if err := validateArtifactEndpoint("SYNARA_ARTIFACT_ENDPOINT", cfg.ArtifactEndpoint, cfg.Platform.Profile == platform.ProfileEnterprise); err != nil {
			return Config{}, err
		}
		if err := validateArtifactEndpoint("SYNARA_ARTIFACT_PUBLIC_ENDPOINT", cfg.ArtifactPublicEndpoint, cfg.Platform.Profile == platform.ProfileEnterprise); err != nil {
			return Config{}, err
		}
	}
	if cfg.Platform.Profile == platform.ProfileEnterprise && artifactAccessKeyConfigured && cfg.ArtifactSessionToken == "" {
		return Config{}, errors.New("enterprise Artifact static credentials must be temporary and include SYNARA_ARTIFACT_SESSION_TOKEN; prefer workload identity")
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

func validateInternalIncidentPublisher(cfg Config) error {
	configured := cfg.InternalIncidentPublisherURL != "" || cfg.InternalIncidentPublisherHMACKeyID != "" || len(cfg.InternalIncidentPublisherHMACKey) != 0
	if !configured {
		if cfg.InternalStatusBoardURL != "" {
			return errors.New("SYNARA_INTERNAL_STATUS_BOARD_URL requires SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL, SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY_ID and SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY")
		}
		return nil
	}
	_, keyIDValid := validation.OpaqueIdentifier(cfg.InternalIncidentPublisherHMACKeyID, 2, 200)
	if cfg.InternalIncidentPublisherURL == "" || !keyIDValid || len(cfg.InternalIncidentPublisherHMACKey) != 32 {
		return errors.New("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL, SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY_ID and SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY must be configured together")
	}
	if cfg.InternalStatusBoardURL == "" {
		return errors.New("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL requires SYNARA_INTERNAL_STATUS_BOARD_URL")
	}
	parsed, err := url.Parse(cfg.InternalIncidentPublisherURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL must be a credential-free HTTPS URL without query or fragment")
	}
	if sameURLOrigin(parsed, mustParseURL(cfg.PublicControlPlaneURL)) ||
		sameURLOrigin(parsed, mustParseURL(cfg.PublicAdminURL)) {
		return errors.New("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL must use a failure-independent origin from Synara application services")
	}
	if cfg.InternalIncidentPublisherTimeout <= 0 || cfg.InternalIncidentPublisherTimeout > 30*time.Second {
		return errors.New("SYNARA_INTERNAL_INCIDENT_PUBLISHER_TIMEOUT must be positive and at most 30s")
	}
	return nil
}

func mustParseURL(value string) *url.URL {
	parsed, _ := url.Parse(value)
	return parsed
}

func validateArtifactEndpoint(name, value string, requireHTTPS bool) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an HTTP(S) origin without credentials, path, query or fragment", name)
	}
	if requireHTTPS && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use HTTPS for enterprise deployments", name)
	}
	return nil
}

func validatePublicHTTPURL(name, value string, cookieSecure bool) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an HTTP(S) origin", name)
	}
	if !isLoopbackHostname(parsed.Hostname()) {
		if parsed.Scheme != "https" {
			return fmt.Errorf("%s must use HTTPS outside loopback", name)
		}
		if !cookieSecure {
			return errors.New("SYNARA_LOGIN_COOKIE_SECURE must be true outside loopback")
		}
	}
	return nil
}

func validateInternalStatusBoardURL(name, value string, coupledURLs ...string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be a credential-free HTTPS URL without query or fragment", name)
	}
	for _, coupledValue := range coupledURLs {
		if strings.TrimSpace(coupledValue) == "" {
			continue
		}
		coupled, parseErr := url.Parse(coupledValue)
		if parseErr == nil && sameURLOrigin(parsed, coupled) {
			return fmt.Errorf("%s must use a failure-independent origin from Synara application services", name)
		}
	}
	return nil
}

func sameURLOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectiveURLPort(left) == effectiveURLPort(right)
}

func effectiveURLPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(value.Scheme, "http") {
		return "80"
	}
	return ""
}

func parseProviderCursorDecryptKeys(
	raw, primaryKeyID string,
	primaryKey []byte,
) ([]ProviderCursorDecryptKeyConfig, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var inputs []providerCursorDecryptKeyInput
	if err := decoder.Decode(&inputs); err != nil {
		return nil, fmt.Errorf("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON must be a JSON array: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON must contain one JSON value")
	}
	if len(inputs) == 0 || len(inputs) > 8 {
		return nil, errors.New("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON must contain between 1 and 8 keys")
	}
	if primaryKeyID == "" || len(primaryKey) != 32 {
		return nil, errors.New("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON requires a named primary Provider Cursor key")
	}
	seenIDs := map[string]struct{}{primaryKeyID: {}}
	seenKeys := [][]byte{primaryKey}
	result := make([]ProviderCursorDecryptKeyConfig, 0, len(inputs))
	for index, input := range inputs {
		keyID := strings.TrimSpace(input.KeyID)
		keyEnvironment := strings.TrimSpace(input.KeyEnvironment)
		if keyID == "" || len(keyID) > 200 || strings.ContainsAny(keyID, "\r\n\t") {
			return nil, fmt.Errorf("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON[%d].keyId is invalid", index)
		}
		if _, exists := seenIDs[keyID]; exists {
			return nil, fmt.Errorf("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON[%d] duplicates a key ID", index)
		}
		if !validEnvironmentName(keyEnvironment) {
			return nil, fmt.Errorf("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON[%d].keyEnvironment is invalid", index)
		}
		encoded := strings.TrimSpace(os.Getenv(keyEnvironment))
		if encoded == "" {
			return nil, fmt.Errorf("%s is required for Provider Cursor fallback decryption", keyEnvironment)
		}
		decoded, err := decodeKey(encoded, keyEnvironment)
		if err != nil {
			return nil, err
		}
		for _, seen := range seenKeys {
			if bytes.Equal(decoded, seen) {
				return nil, fmt.Errorf("%s must not reuse another Provider Cursor key", keyEnvironment)
			}
		}
		seenIDs[keyID] = struct{}{}
		seenKeys = append(seenKeys, decoded)
		result = append(result, ProviderCursorDecryptKeyConfig{KeyID: keyID, Key: decoded})
	}
	return result, nil
}

func parseCredentialKMSDecryptKeys(
	raw, primaryProvider, primaryKeyID string,
	primaryLocalKey []byte,
) ([]CredentialKMSDecryptKeyConfig, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var inputs []credentialKMSDecryptKeyInput
	if err := decoder.Decode(&inputs); err != nil {
		return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON must be a JSON array: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON must contain one JSON value")
	}
	if len(inputs) == 0 || len(inputs) > 8 {
		return nil, errors.New("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON must contain between 1 and 8 keys")
	}
	if strings.TrimSpace(primaryProvider) == "" || strings.TrimSpace(primaryKeyID) == "" {
		return nil, errors.New("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON requires a primary credential KMS key")
	}
	seen := map[string]struct{}{strings.ToLower(strings.TrimSpace(primaryProvider)) + "\x00" + strings.TrimSpace(primaryKeyID): {}}
	keys := make([]CredentialKMSDecryptKeyConfig, 0, len(inputs))
	for index, input := range inputs {
		provider := strings.ToLower(strings.TrimSpace(input.Provider))
		keyID := strings.TrimSpace(input.KeyID)
		region := strings.TrimSpace(input.Region)
		keyEnvironment := strings.TrimSpace(input.LocalKeyEnvironment)
		endpoint := strings.TrimRight(strings.TrimSpace(input.Endpoint), "/")
		caFile := strings.TrimSpace(input.CAFile)
		clientCertFile := strings.TrimSpace(input.ClientCertFile)
		clientKeyFile := strings.TrimSpace(input.ClientKeyFile)
		timeout := 10 * time.Second
		if strings.TrimSpace(input.Timeout) != "" {
			var err error
			timeout, err = time.ParseDuration(strings.TrimSpace(input.Timeout))
			if err != nil || timeout <= 0 || timeout > time.Minute {
				return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d].timeout is invalid", index)
			}
		}
		if keyID == "" || len(keyID) > 1024 || strings.ContainsAny(keyID, "\r\n\t") {
			return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d].keyId is invalid", index)
		}
		identity := provider + "\x00" + keyID
		if _, exists := seen[identity]; exists {
			return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d] duplicates a KMS key", index)
		}
		seen[identity] = struct{}{}
		key := CredentialKMSDecryptKeyConfig{
			Provider: provider, KeyID: keyID, Region: region, Endpoint: endpoint, CAFile: caFile,
			ClientCertFile: clientCertFile, ClientKeyFile: clientKeyFile, Timeout: timeout,
		}
		switch provider {
		case "local":
			if !validEnvironmentName(keyEnvironment) || region != "" || endpoint != "" || caFile != "" || clientCertFile != "" || clientKeyFile != "" || strings.TrimSpace(input.Timeout) != "" {
				return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d] local key configuration is invalid", index)
			}
			encoded := strings.TrimSpace(os.Getenv(keyEnvironment))
			if encoded == "" {
				return nil, fmt.Errorf("%s is required for credential KMS fallback decryption", keyEnvironment)
			}
			decoded, err := decodeKey(encoded, keyEnvironment)
			if err != nil {
				return nil, err
			}
			if len(primaryLocalKey) > 0 && bytes.Equal(decoded, primaryLocalKey) {
				return nil, fmt.Errorf("%s must not reuse the primary local credential KMS key", keyEnvironment)
			}
			key.LocalKey = decoded
		case "aws-kms":
			if keyEnvironment != "" || endpoint != "" || caFile != "" || clientCertFile != "" || clientKeyFile != "" || strings.TrimSpace(input.Timeout) != "" {
				return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d] AWS KMS key must not declare localKeyEnvironment", index)
			}
		case "synara-kms":
			if keyEnvironment != "" || region != "" || endpoint == "" || caFile == "" || clientCertFile == "" || clientKeyFile == "" {
				return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d] Synara KMS configuration is invalid", index)
			}
		default:
			return nil, fmt.Errorf("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON[%d].provider must be local, aws-kms or synara-kms", index)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func credentialKMSTLSConfigured(cfg Config) bool {
	return cfg.CredentialKMSEndpoint != "" || cfg.CredentialKMSCAFile != "" ||
		cfg.CredentialKMSClientCertFile != "" || cfg.CredentialKMSClientKeyFile != ""
}

func validEnvironmentName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'A' && character <= 'Z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
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

func loadResourceLifecycleConfig(profile platform.DeploymentProfile) (lifecyclepolicy.Config, error) {
	config := lifecyclepolicy.DefaultConfig(profile)
	storageBounds := config.Bounds
	boundFields := []struct {
		minimumName string
		maximumName string
		bounds      *lifecyclepolicy.IntBounds
		storage     lifecyclepolicy.IntBounds
	}{
		{"SYNARA_RESOURCE_WAITING_KEEP_ALIVE_MIN_SECONDS", "SYNARA_RESOURCE_WAITING_KEEP_ALIVE_MAX_SECONDS", &config.Bounds.WaitingKeepAliveSeconds, storageBounds.WaitingKeepAliveSeconds},
		{"SYNARA_RESOURCE_SUSPEND_AFTER_IDLE_MIN_SECONDS", "SYNARA_RESOURCE_SUSPEND_AFTER_IDLE_MAX_SECONDS", &config.Bounds.SuspendAfterIdleSeconds, storageBounds.SuspendAfterIdleSeconds},
		{"SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME_MIN_SECONDS", "SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME_MAX_SECONDS", &config.Bounds.AbsoluteSessionLifetimeSeconds, storageBounds.AbsoluteSessionLifetimeSeconds},
		{"SYNARA_RESOURCE_WORKSPACE_RETENTION_MIN_DAYS", "SYNARA_RESOURCE_WORKSPACE_RETENTION_MAX_DAYS", &config.Bounds.WorkspaceRetentionDays, storageBounds.WorkspaceRetentionDays},
	}
	for _, field := range boundFields {
		minimum, err := envInt(field.minimumName, field.bounds.Minimum)
		if err != nil {
			return lifecyclepolicy.Config{}, err
		}
		maximum, err := envInt(field.maximumName, field.bounds.Maximum)
		if err != nil {
			return lifecyclepolicy.Config{}, err
		}
		if minimum < field.storage.Minimum || maximum > field.storage.Maximum {
			return lifecyclepolicy.Config{}, fmt.Errorf(
				"%s and %s must stay within the database-supported range %d..%d",
				field.minimumName, field.maximumName, field.storage.Minimum, field.storage.Maximum,
			)
		}
		*field.bounds = lifecyclepolicy.IntBounds{Minimum: minimum, Maximum: maximum}
	}

	waiting, err := envWholeSeconds(
		"SYNARA_RESOURCE_WAITING_KEEP_ALIVE", time.Duration(config.Defaults.WaitingKeepAliveSeconds)*time.Second,
	)
	if err != nil {
		return lifecyclepolicy.Config{}, err
	}
	suspendAfterIdle, err := envWholeSeconds(
		"SYNARA_RESOURCE_SUSPEND_AFTER_IDLE", time.Duration(config.Defaults.SuspendAfterIdleSeconds)*time.Second,
	)
	if err != nil {
		return lifecyclepolicy.Config{}, err
	}
	config.Defaults.WaitingKeepAliveSeconds = waiting
	config.Defaults.SuspendAfterIdleSeconds = suspendAfterIdle

	absoluteRaw := strings.TrimSpace(os.Getenv("SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME"))
	if absoluteRaw != "" {
		absoluteDuration, err := time.ParseDuration(absoluteRaw)
		if err != nil || absoluteDuration < 0 || absoluteDuration%time.Second != 0 {
			return lifecyclepolicy.Config{}, errors.New("SYNARA_RESOURCE_ABSOLUTE_SESSION_LIFETIME must be zero or a whole-second duration")
		}
		if absoluteDuration == 0 {
			config.Defaults.AbsoluteSessionLifetimeSeconds = nil
		} else {
			seconds := int(absoluteDuration / time.Second)
			config.Defaults.AbsoluteSessionLifetimeSeconds = &seconds
		}
	}
	if config.Defaults.WorkspaceRetentionDays, err = envInt(
		"SYNARA_RESOURCE_WORKSPACE_RETENTION_DAYS", config.Defaults.WorkspaceRetentionDays,
	); err != nil {
		return lifecyclepolicy.Config{}, err
	}
	config.Defaults.WarmPoolMode = strings.ToLower(envOrDefault(
		"SYNARA_RESOURCE_WARM_POOL_MODE", config.Defaults.WarmPoolMode,
	))
	if err := config.Validate(); err != nil {
		return lifecyclepolicy.Config{}, fmt.Errorf("invalid Resource Lifecycle configuration: %w", err)
	}
	return config, nil
}

func envWholeSeconds(name string, fallback time.Duration) (int, error) {
	duration, err := envDurationStrict(name, fallback)
	if err != nil {
		return 0, err
	}
	if duration <= 0 || duration%time.Second != 0 {
		return 0, fmt.Errorf("%s must be a positive whole-second duration", name)
	}
	return int(duration / time.Second), nil
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

type billingImportMappingEnvelope struct {
	Imports *[]billingImportMapping `json:"imports"`
}

type billingImportMapping struct {
	TenantID            string                     `json:"tenantId"`
	Provider            string                     `json:"provider"`
	ExternalImportID    string                     `json:"externalImportId"`
	Format              billing.ExportObjectFormat `json:"format"`
	ObjectKey           string                     `json:"objectKey"`
	ObjectVersion       string                     `json:"objectVersion"`
	ExecutionTargetIDs  []string                   `json:"executionTargetIds"`
	ScheduleInterval    string                     `json:"scheduleInterval"`
	Reconcile           bool                       `json:"reconcile"`
	EstimateAfterImport bool                       `json:"estimateAfterImport"`
}

type billingSharedAllocationMappingEnvelope struct {
	Allocations *[]billingSharedAllocationMapping `json:"allocations"`
}

type billingSharedAllocationMapping struct {
	ExecutionTargetID    string `json:"executionTargetId"`
	Provider             string `json:"provider"`
	CurrencyCode         string `json:"currencyCode"`
	BillingPeriodStartAt string `json:"billingPeriodStartAt"`
	BillingPeriodEndAt   string `json:"billingPeriodEndAt"`
	Calendar             string `json:"calendar"`
	FirstPeriodStartAt   string `json:"firstPeriodStartAt"`
	LastPeriodEndAt      string `json:"lastPeriodEndAt"`
	SettlementDelay      string `json:"settlementDelay"`
	ScheduleInterval     string `json:"scheduleInterval"`
}

func parseBillingImportMappings(raw string) ([]billing.ConfiguredImport, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var items []billingImportMapping
	switch raw[0] {
	case '[':
		if err := decodeStrictConfigJSON(raw, &items); err != nil {
			return nil, err
		}
	case '{':
		envelope := billingImportMappingEnvelope{}
		if err := decodeStrictConfigJSON(raw, &envelope); err != nil {
			return nil, err
		}
		if envelope.Imports == nil {
			return nil, errors.New("must be a JSON array or an object with an imports array")
		}
		items = *envelope.Imports
	default:
		return nil, errors.New("must be a JSON array or an object with an imports array")
	}
	imports := make([]billing.ConfiguredImport, 0, len(items))
	for index, item := range items {
		tenantID, err := uuid.Parse(strings.TrimSpace(item.TenantID))
		if err != nil {
			return nil, fmt.Errorf("imports[%d].tenantId must be a UUID", index)
		}
		var scheduleInterval time.Duration
		if strings.TrimSpace(item.ScheduleInterval) != "" {
			scheduleInterval, err = time.ParseDuration(strings.TrimSpace(item.ScheduleInterval))
			if err != nil {
				return nil, fmt.Errorf("imports[%d].scheduleInterval must be a valid duration", index)
			}
		}
		executionTargetIDs := make([]uuid.UUID, 0, len(item.ExecutionTargetIDs))
		for targetIndex, rawTargetID := range item.ExecutionTargetIDs {
			targetID, parseErr := uuid.Parse(strings.TrimSpace(rawTargetID))
			if parseErr != nil || targetID == uuid.Nil {
				return nil, fmt.Errorf("imports[%d].executionTargetIds[%d] must be a UUID", index, targetIndex)
			}
			executionTargetIDs = append(executionTargetIDs, targetID)
		}
		imports = append(imports, billing.ConfiguredImport{
			TenantID:            tenantID,
			Provider:            item.Provider,
			ExternalImportID:    item.ExternalImportID,
			Format:              item.Format,
			ObjectKey:           item.ObjectKey,
			ObjectVersion:       item.ObjectVersion,
			ExecutionTargetIDs:  executionTargetIDs,
			ScheduleInterval:    scheduleInterval,
			Reconcile:           item.Reconcile,
			EstimateAfterImport: item.EstimateAfterImport,
		})
	}
	return imports, nil
}

func parseBillingSharedAllocationMappings(raw string) ([]billing.ConfiguredSharedAllocation, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var items []billingSharedAllocationMapping
	switch raw[0] {
	case '[':
		if err := decodeStrictConfigJSON(raw, &items); err != nil {
			return nil, err
		}
	case '{':
		envelope := billingSharedAllocationMappingEnvelope{}
		if err := decodeStrictConfigJSON(raw, &envelope); err != nil {
			return nil, err
		}
		if envelope.Allocations == nil {
			return nil, errors.New("must be a JSON array or an object with an allocations array")
		}
		items = *envelope.Allocations
	default:
		return nil, errors.New("must be a JSON array or an object with an allocations array")
	}
	allocations := make([]billing.ConfiguredSharedAllocation, 0, len(items))
	for index, item := range items {
		targetID, err := uuid.Parse(strings.TrimSpace(item.ExecutionTargetID))
		if err != nil || targetID == uuid.Nil {
			return nil, fmt.Errorf("allocations[%d].executionTargetId must be a UUID", index)
		}
		periodStart, err := parseOptionalBillingTimestamp(item.BillingPeriodStartAt)
		if err != nil {
			return nil, fmt.Errorf("allocations[%d].billingPeriodStartAt must use RFC3339", index)
		}
		periodEnd, err := parseOptionalBillingTimestamp(item.BillingPeriodEndAt)
		if err != nil {
			return nil, fmt.Errorf("allocations[%d].billingPeriodEndAt must use RFC3339", index)
		}
		firstPeriodStart, err := parseOptionalBillingTimestamp(item.FirstPeriodStartAt)
		if err != nil {
			return nil, fmt.Errorf("allocations[%d].firstPeriodStartAt must use RFC3339", index)
		}
		var lastPeriodEnd *time.Time
		if strings.TrimSpace(item.LastPeriodEndAt) != "" {
			parsed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.LastPeriodEndAt))
			if parseErr != nil {
				return nil, fmt.Errorf("allocations[%d].lastPeriodEndAt must use RFC3339", index)
			}
			lastPeriodEnd = &parsed
		}
		settlementDelay, err := time.ParseDuration(strings.TrimSpace(item.SettlementDelay))
		if err != nil {
			return nil, fmt.Errorf("allocations[%d].settlementDelay must be a valid duration", index)
		}
		scheduleInterval, err := time.ParseDuration(strings.TrimSpace(item.ScheduleInterval))
		if err != nil {
			return nil, fmt.Errorf("allocations[%d].scheduleInterval must be a valid duration", index)
		}
		allocations = append(allocations, billing.ConfiguredSharedAllocation{
			ExecutionTargetID: targetID, Provider: item.Provider, CurrencyCode: item.CurrencyCode,
			BillingPeriodStartAt: periodStart, BillingPeriodEndAt: periodEnd,
			Calendar: item.Calendar, FirstPeriodStartAt: firstPeriodStart, LastPeriodEndAt: lastPeriodEnd,
			SettlementDelay: settlementDelay, ScheduleInterval: scheduleInterval,
		})
	}
	return allocations, nil
}

func parseOptionalBillingTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func decodeStrictConfigJSON(raw string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("unexpected trailing JSON content")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON content")
		}
		return err
	}
	return nil
}

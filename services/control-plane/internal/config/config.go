package config

import (
	"encoding/base64"
	"errors"
	"fmt"
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
	Platform                    platform.Config
	ListenAddress               string
	DatabaseURL                 string
	SQLitePath                  string
	ArtifactLocalPath           string
	ArtifactBucket              string
	ArtifactRegion              string
	ArtifactEndpoint            string
	ArtifactPublicEndpoint      string
	ArtifactAccessKeyID         string
	ArtifactSecretAccessKey     string
	ArtifactSessionToken        string
	ArtifactUsePathStyle        bool
	ArtifactPresignTTL          time.Duration
	ArtifactMaxUploadBytes      int64
	InstallationID              string
	CookieName                  string
	CookieSecure                bool
	DevBootstrapEnabled         bool
	SessionTTL                  time.Duration
	ShutdownTimeout             time.Duration
	WorkerRegistrationToken     string
	WorkerLeaseTTL              time.Duration
	WorkerHeartbeatTimeout      time.Duration
	WorkerReceiptTTL            time.Duration
	ProviderCursorKey           []byte
	LocalAgentdRunnerCommand    []string
	LocalAgentdWorkspaceRoot    string
	LocalAgentdRestartBackoff   time.Duration
	CredentialKMSProvider       string
	CredentialKMSKeyID          string
	CredentialKMSLocalKey       []byte
	CredentialKMSAWSRegion      string
	PublicControlPlaneURL       string
	AgentdBinaryPath            string
	SSHProvisionTimeout         time.Duration
	DockerReconcileInterval     time.Duration
	KubernetesReconcileInterval time.Duration
	RetentionSweepInterval      time.Duration
	OutboxPollInterval          time.Duration
	OutboxClaimTTL              time.Duration
	OutboxBatchSize             int
	OutboxMaxAttempts           int
	OutboxBaseBackoff           time.Duration
	OutboxMaxBackoff            time.Duration
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
	if cfg.ShutdownTimeout, err = envDurationStrict("SYNARA_CONTROL_PLANE_SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
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
	if cfg.SessionTTL <= 0 {
		return Config{}, errors.New("SYNARA_LOGIN_SESSION_TTL must be positive")
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
	if cfg.ArtifactPresignTTL <= 0 || cfg.ArtifactPresignTTL > 24*time.Hour {
		return Config{}, errors.New("SYNARA_ARTIFACT_PRESIGN_TTL must be positive and at most 24h")
	}
	if cfg.ArtifactMaxUploadBytes <= 0 {
		return Config{}, errors.New("SYNARA_ARTIFACT_MAX_UPLOAD_BYTES must be positive")
	}
	if cfg.LocalAgentdRestartBackoff <= 0 {
		return Config{}, errors.New("SYNARA_LOCAL_AGENTD_RESTART_BACKOFF must be positive")
	}
	if len(cfg.LocalAgentdRunnerCommand) > 0 && strings.TrimSpace(cfg.LocalAgentdWorkspaceRoot) == "" {
		return Config{}, errors.New("SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT must not be empty")
	}
	if cfg.PublicControlPlaneURL != "" {
		parsed, parseErr := url.Parse(cfg.PublicControlPlaneURL)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return Config{}, errors.New("SYNARA_PUBLIC_CONTROL_PLANE_URL must be an HTTP(S) origin")
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

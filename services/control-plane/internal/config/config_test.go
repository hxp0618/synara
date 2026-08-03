package config

import (
	"encoding/base64"
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

func TestLoadHardensArtifactStorageCredentialsEndpointsAndPresignTTL(t *testing.T) {
	setValidEnterpriseArtifactConfig := func() {
		clearConfigEnvironment(t)
		t.Setenv("SYNARA_DEPLOYMENT_PROFILE", "enterprise")
		t.Setenv("SYNARA_DATABASE_URL", "postgres://synara:test@db/synara")
		t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "https://control.synara.example")
		t.Setenv("SYNARA_LOGIN_COOKIE_SECURE", "true")
	}

	setValidEnterpriseArtifactConfig()
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ArtifactAccessKeyID != "" || cfg.ArtifactSecretAccessKey != "" || cfg.ArtifactSessionToken != "" {
		t.Fatalf("enterprise Artifact workload identity unexpectedly contains static credentials: %#v", cfg)
	}

	for _, endpoint := range []struct {
		name  string
		value string
	}{
		{name: "SYNARA_ARTIFACT_ENDPOINT", value: "http://s3.internal.example"},
		{name: "SYNARA_ARTIFACT_PUBLIC_ENDPOINT", value: "http://objects.synara.example"},
		{name: "SYNARA_ARTIFACT_ENDPOINT", value: "https://operator:secret@s3.example"},
		{name: "SYNARA_ARTIFACT_PUBLIC_ENDPOINT", value: "https://objects.synara.example/tenant"},
	} {
		setValidEnterpriseArtifactConfig()
		t.Setenv(endpoint.name, endpoint.value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), endpoint.name) {
			t.Fatalf("expected hardened Artifact endpoint rejection for %s=%q, got %v", endpoint.name, endpoint.value, err)
		}
	}

	setValidEnterpriseArtifactConfig()
	t.Setenv("SYNARA_ARTIFACT_PRESIGN_TTL", "15m1s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at most 15m") {
		t.Fatalf("expected remote Artifact presign TTL rejection, got %v", err)
	}

	setValidEnterpriseArtifactConfig()
	t.Setenv("SYNARA_ARTIFACT_ACCESS_KEY_ID", "temporary-access")
	t.Setenv("SYNARA_ARTIFACT_SECRET_ACCESS_KEY", "temporary-secret")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be temporary") {
		t.Fatalf("expected long-lived enterprise Artifact key rejection, got %v", err)
	}

	setValidEnterpriseArtifactConfig()
	t.Setenv("SYNARA_ARTIFACT_ACCESS_KEY_ID", "temporary-access")
	t.Setenv("SYNARA_ARTIFACT_SECRET_ACCESS_KEY", "temporary-secret")
	t.Setenv("SYNARA_ARTIFACT_SESSION_TOKEN", "temporary-session")
	if _, err := Load(); err != nil {
		t.Fatalf("temporary enterprise Artifact credentials should be accepted: %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_ARTIFACT_ACCESS_KEY_ID", "orphan-access")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("expected partial Artifact credential rejection, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_ARTIFACT_ACCESS_KEY_ID", "unused-access")
	t.Setenv("SYNARA_ARTIFACT_SECRET_ACCESS_KEY", "unused-secret")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "Local Artifact storage") {
		t.Fatalf("expected ignored Local Artifact credential rejection, got %v", err)
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

func TestLoadBoundsDesktopEnrollmentTTL(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DesktopEnrollmentTTL != 3*time.Minute {
		t.Fatalf("default Desktop Enrollment TTL = %s, want 3m", cfg.DesktopEnrollmentTTL)
	}

	for _, value := range []string{"59s", "5m1s"} {
		clearConfigEnvironment(t)
		t.Setenv("SYNARA_DESKTOP_ENROLLMENT_TTL", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_DESKTOP_ENROLLMENT_TTL") {
			t.Fatalf("expected bounded Desktop Enrollment TTL error for %q, got %v", value, err)
		}
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

func TestLoadValidatesPublicAdminURL(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_ADMIN_URL", "http://127.0.0.1:3774")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicAdminURL != "http://127.0.0.1:3774" {
		t.Fatalf("public Admin URL = %q", cfg.PublicAdminURL)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_ADMIN_URL", "http://admin.synara.example")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PUBLIC_ADMIN_URL must use HTTPS") {
		t.Fatalf("expected non-loopback Admin HTTPS requirement, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_ADMIN_URL", "ssh://admin.synara.example")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PUBLIC_ADMIN_URL must be an HTTP(S) origin") {
		t.Fatalf("expected invalid public Admin URL rejection, got %v", err)
	}
}

func TestLoadRequiresCompleteInternalIncidentDeliveryConfiguration(t *testing.T) {
	setBase := func() {
		clearConfigEnvironment(t)
		t.Setenv("SYNARA_LOGIN_COOKIE_SECURE", "true")
		t.Setenv("SYNARA_PUBLIC_CONTROL_PLANE_URL", "https://control.synara.example")
		t.Setenv("SYNARA_PUBLIC_ADMIN_URL", "https://admin.synara.example")
		t.Setenv("SYNARA_INTERNAL_STATUS_BOARD_URL", "https://status.synara.example/history")
	}
	setBase()
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "INTERNAL_INCIDENT_PUBLISHER") {
		t.Fatalf("expected Status Board without publisher to fail, got %v", err)
	}

	setBase()
	t.Setenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL", "https://notify.synara.example/hooks/incidents")
	t.Setenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY", base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalIncidentPublisherURL == "" || len(cfg.InternalIncidentPublisherHMACKey) != 32 {
		t.Fatalf("unexpected incident publisher config: %#v", cfg)
	}

	setBase()
	t.Setenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL", "https://control.synara.example/hooks/incidents")
	t.Setenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY", base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "failure-independent") {
		t.Fatalf("expected application-coupled publisher origin rejection, got %v", err)
	}
}

func TestLoadValidatesIndependentInternalStatusBoardURL(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_INTERNAL_STATUS_BOARD_URL", "https://status.synara.example/history")
	t.Setenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL", "https://notify.synara.example/hooks/incidents")
	t.Setenv("SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY", base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalStatusBoardURL != "https://status.synara.example/history" {
		t.Fatalf("internal Status Board URL = %q", cfg.InternalStatusBoardURL)
	}

	for _, value := range []string{
		"http://status.synara.example",
		"https://operator:secret@status.synara.example",
		"https://status.synara.example?tenant=secret",
		"https://status.synara.example#internal",
	} {
		clearConfigEnvironment(t)
		t.Setenv("SYNARA_INTERNAL_STATUS_BOARD_URL", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_INTERNAL_STATUS_BOARD_URL") {
			t.Fatalf("expected invalid internal Status Board URL rejection for %q, got %v", value, err)
		}
	}

	for _, coupled := range []struct {
		name  string
		value string
	}{
		{name: "SYNARA_PUBLIC_CONTROL_PLANE_URL", value: "https://synara.example/api"},
		{name: "SYNARA_PUBLIC_ADMIN_URL", value: "https://synara.example:443/admin"},
	} {
		clearConfigEnvironment(t)
		t.Setenv("SYNARA_LOGIN_COOKIE_SECURE", "true")
		t.Setenv(coupled.name, coupled.value)
		t.Setenv("SYNARA_INTERNAL_STATUS_BOARD_URL", "https://synara.example/status")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "failure-independent origin") {
			t.Fatalf("expected coupled internal Status Board rejection for %s, got %v", coupled.name, err)
		}
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PUBLIC_STATUS_PAGE_URL", "https://legacy-status.synara.example")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "is retired; use SYNARA_INTERNAL_STATUS_BOARD_URL") {
		t.Fatalf("expected legacy public Status Page configuration rejection, got %v", err)
	}
}

func TestLoadRejectsPaymentConfigurationForInternalSelfHostedProduct(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CommercializationMode != CommercializationModeInternalSelfHosted {
		t.Fatalf("unexpected self-hosted product configuration: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_COMMERCIALIZATION_MODE", "external-subscription")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be internal-self-hosted") {
		t.Fatalf("expected external product mode rejection, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_COMMERCIAL_BILLING_PROVIDER", "stripe")
	t.Setenv("SYNARA_COMMERCIAL_BILLING_RETURN_URL", "https://app.synara.example/settings")
	t.Setenv("SYNARA_STRIPE_SECRET_KEY", "sk_test_"+strings.Repeat("x", 32))
	t.Setenv("SYNARA_STRIPE_WEBHOOK_SECRET", "whsec_"+strings.Repeat("y", 32))
	t.Setenv("SYNARA_STRIPE_PRICE_MAP_JSON", `{"enterprise":"price_enterprise123"}`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unsupported by the internal-self-hosted product") {
		t.Fatalf("expected self-hosted product mode to reject Stripe configuration, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_STRIPE_SECRET_KEY", "sk_test_"+strings.Repeat("x", 32))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unsupported by the internal-self-hosted product") {
		t.Fatalf("expected disabled provider to reject retained Stripe secret, got %v", err)
	}

	for _, name := range []string{
		"SYNARA_COMMERCIAL_BILLING_PROVIDER",
		"SYNARA_COMMERCIAL_BILLING_RETURN_URL",
		"SYNARA_STRIPE_SECRET_KEY",
		"SYNARA_STRIPE_WEBHOOK_SECRET",
		"SYNARA_STRIPE_PRICE_MAP_JSON",
		"SYNARA_STRIPE_AUTOMATIC_TAX_ENABLED",
		"SYNARA_STRIPE_CHECKOUT_TTL",
		"SYNARA_STRIPE_PORTAL_CONFIGURATION_ID",
	} {
		clearConfigEnvironment(t)
		t.Setenv(name, "retained-payment-configuration")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unsupported by the internal-self-hosted product") {
			t.Errorf("expected %s to be rejected by internal-self-hosted configuration, got %v", name, err)
		}
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_BILLING_BLOB_SOURCE", "s3")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_COST_ACCOUNTING_BLOB_SOURCE") {
		t.Fatalf("expected retired Billing-prefixed cost configuration to fail with its replacement, got %v", err)
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

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CREDENTIAL_KMS_PROVIDER", "local")
	t.Setenv("SYNARA_CREDENTIAL_KMS_KEY_ID", "local-v2")
	t.Setenv("SYNARA_CREDENTIAL_MASTER_KEY", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	t.Setenv("SYNARA_OLD_CREDENTIAL_KEY", "Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M=")
	t.Setenv("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON", `[{"provider":"local","keyId":"local-v1","localKeyEnvironment":"SYNARA_OLD_CREDENTIAL_KEY"}]`)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CredentialKMSDecryptKeys) != 1 || cfg.CredentialKMSDecryptKeys[0].KeyID != "local-v1" ||
		len(cfg.CredentialKMSDecryptKeys[0].LocalKey) != 32 {
		t.Fatalf("unexpected credential KMS fallback config: %#v", cfg.CredentialKMSDecryptKeys)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CREDENTIAL_KMS_PROVIDER", "local")
	t.Setenv("SYNARA_CREDENTIAL_KMS_KEY_ID", "local-v2")
	t.Setenv("SYNARA_CREDENTIAL_MASTER_KEY", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	t.Setenv("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON", `[{"provider":"local","keyId":"local-v2","localKeyEnvironment":"SYNARA_OLD_CREDENTIAL_KEY"}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "duplicates a KMS key") {
		t.Fatalf("expected duplicate fallback KMS error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CREDENTIAL_KMS_PROVIDER", "local")
	t.Setenv("SYNARA_CREDENTIAL_KMS_KEY_ID", "local-v2")
	t.Setenv("SYNARA_CREDENTIAL_MASTER_KEY", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	t.Setenv("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON", `[{"provider":"local","keyId":"local-v1","localKeyEnvironment":"SYNARA_OLD_CREDENTIAL_KEY","unexpected":true}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be a JSON array") {
		t.Fatalf("expected unknown fallback field error, got %v", err)
	}

	clearConfigEnvironment(t)
	keyID := uuid.NewString() + "/versions/1"
	t.Setenv("SYNARA_CREDENTIAL_KMS_PROVIDER", "synara-kms")
	t.Setenv("SYNARA_CREDENTIAL_KMS_KEY_ID", keyID)
	t.Setenv("SYNARA_CREDENTIAL_KMS_ENDPOINT", "https://kms.internal.example/")
	t.Setenv("SYNARA_CREDENTIAL_KMS_CA_FILE", "/var/run/kms/ca.crt")
	t.Setenv("SYNARA_CREDENTIAL_KMS_CLIENT_CERT_FILE", "/var/run/kms/tls.crt")
	t.Setenv("SYNARA_CREDENTIAL_KMS_CLIENT_KEY_FILE", "/var/run/kms/tls.key")
	t.Setenv("SYNARA_CREDENTIAL_KMS_TIMEOUT", "8s")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CredentialKMSProvider != "synara-kms" || cfg.CredentialKMSKeyID != keyID ||
		cfg.CredentialKMSEndpoint != "https://kms.internal.example" || cfg.CredentialKMSTimeout != 8*time.Second {
		t.Fatalf("unexpected Synara KMS config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_CREDENTIAL_KMS_PROVIDER", "aws-kms")
	t.Setenv("SYNARA_CREDENTIAL_KMS_KEY_ID", "arn:aws:kms:eu-west-1:123456789012:key/new")
	t.Setenv("SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON", `[{"provider":"synara-kms","keyId":"`+keyID+`","endpoint":"https://old-kms.internal.example","caFile":"/old/ca.crt","clientCertFile":"/old/tls.crt","clientKeyFile":"/old/tls.key","timeout":"12s"}]`)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CredentialKMSDecryptKeys) != 1 || cfg.CredentialKMSDecryptKeys[0].Provider != "synara-kms" ||
		cfg.CredentialKMSDecryptKeys[0].Endpoint != "https://old-kms.internal.example" ||
		cfg.CredentialKMSDecryptKeys[0].Timeout != 12*time.Second {
		t.Fatalf("unexpected mixed-provider decrypt config: %#v", cfg.CredentialKMSDecryptKeys)
	}
}

func TestLoadValidatesProviderCursorKeyringConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY_ID", "runtime-v2")
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY_OLD_V1", "Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M=")
	t.Setenv("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON", `[{"keyId":"runtime-v1","keyEnvironment":"SYNARA_PROVIDER_CURSOR_KEY_OLD_V1"}]`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCursorKeyID != "runtime-v2" || len(cfg.ProviderCursorDecryptKeys) != 1 ||
		cfg.ProviderCursorDecryptKeys[0].KeyID != "runtime-v1" || len(cfg.ProviderCursorDecryptKeys[0].Key) != 32 {
		t.Fatalf("unexpected Provider Cursor keyring config: %#v", cfg)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY_ID", "runtime-v2")
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY_OLD_V1", "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=")
	t.Setenv("SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON", `[{"keyId":"runtime-v1","keyEnvironment":"SYNARA_PROVIDER_CURSOR_KEY_OLD_V1"}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must not reuse") {
		t.Fatalf("expected duplicate Provider Cursor key material error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PROVIDER_CURSOR_KEY_ID", "runtime-v2")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PROVIDER_CURSOR_KEY is required") {
		t.Fatalf("expected named Provider Cursor primary key requirement, got %v", err)
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
	t.Setenv("SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT", "/srv/synara/docker-observability")
	t.Setenv("SYNARA_SSH_WORKER_OBSERVABILITY_ROOT", "/etc/synara/targets")
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
		cfg.DockerReconcileInterval.String() != "7s" ||
		cfg.DockerWorkerObservabilityRoot != "/srv/synara/docker-observability" ||
		cfg.SSHWorkerObservabilityRoot != "/etc/synara/targets" {
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
	t.Setenv("SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT", "relative")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT") {
		t.Fatalf("expected invalid Docker observability root error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_SSH_WORKER_OBSERVABILITY_ROOT", "/")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_SSH_WORKER_OBSERVABILITY_ROOT") {
		t.Fatalf("expected invalid SSH observability root error, got %v", err)
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
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "s3")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_BUCKET", "billing-bucket")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_REGION", "us-east-1")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_ENDPOINT", "https://billing.example.com/")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_ALLOW_CUSTOM_ENDPOINT", "true")
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_PREFIX", " exports/aws ")
	t.Setenv("SYNARA_COST_ACCOUNTING_MAX_OBJECT_BYTES", "4096")
	t.Setenv("SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID", tenantID.String())
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_USE_PATH_STYLE", "true")
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON", `{"allocations":[{
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

func TestLoadParsesPlatformOperatorTenant(t *testing.T) {
	clearConfigEnvironment(t)
	tenantID := uuid.New()
	t.Setenv("SYNARA_PLATFORM_OPERATOR_TENANT_ID", tenantID.String())

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlatformOperatorTenantID != tenantID {
		t.Fatalf("platform operator Tenant = %s, want %s", cfg.PlatformOperatorTenantID, tenantID)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PLATFORM_OPERATOR_TENANT_ID", "not-a-uuid")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_PLATFORM_OPERATOR_TENANT_ID must be a UUID") {
		t.Fatalf("expected invalid Platform Operator Tenant error, got %v", err)
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
	t.Setenv("SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON", `{"allocations":[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID", "not-a-uuid")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID must be a UUID") {
		t.Fatalf("expected invalid tariff operator tenant error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "s3")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_BUCKET", "billing-bucket")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_REGION", "us-east-1")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_ENDPOINT", "https://billing.example.com")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_ALLOW_CUSTOM_ENDPOINT", "true")
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "s3")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_BUCKET", "billing-bucket")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_REGION", "us-east-1")
	t.Setenv("SYNARA_COST_ACCOUNTING_S3_ENDPOINT", "https://billing.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "custom endpoint requires explicit enablement") {
		t.Fatalf("expected explicit custom endpoint allow error, got %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "azure")
	t.Setenv("SYNARA_COST_ACCOUNTING_AZURE_CONTAINER_URL", "http://127.0.0.1:10000/devstoreaccount1/invoices")
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "local")
	t.Setenv("SYNARA_COST_ACCOUNTING_LOCAL_BASE_DIR", t.TempDir())
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "local")
	t.Setenv("SYNARA_COST_ACCOUNTING_LOCAL_BASE_DIR", t.TempDir())
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
	t.Setenv("SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "local")
	t.Setenv("SYNARA_COST_ACCOUNTING_LOCAL_BASE_DIR", t.TempDir())
	t.Setenv("SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", `[{
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
		"SYNARA_ARTIFACT_LOCAL_PATH", "SYNARA_ARTIFACT_BUCKET", "SYNARA_ARTIFACT_REGION",
		"SYNARA_ARTIFACT_ENDPOINT", "SYNARA_ARTIFACT_PUBLIC_ENDPOINT",
		"SYNARA_ARTIFACT_ACCESS_KEY_ID", "SYNARA_ARTIFACT_SECRET_ACCESS_KEY",
		"SYNARA_ARTIFACT_SESSION_TOKEN", "SYNARA_ARTIFACT_USE_PATH_STYLE",
		"SYNARA_ARTIFACT_PRESIGN_TTL", "SYNARA_ARTIFACT_MAX_UPLOAD_BYTES",
		"SYNARA_QUEUE_DRIVER", "SYNARA_CONTROL_PLANE_REPLICAS", "SYNARA_WORKER_LEASES_ENABLED",
		"SYNARA_WORKER_FENCING_ENABLED", "SYNARA_DATABASE_URL", "SYNARA_LOGIN_COOKIE_SECURE",
		"SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP", "SYNARA_LOGIN_SESSION_TTL",
		"SYNARA_LOGIN_SESSION_IDLE_TTL", "SYNARA_LOGIN_COOKIE_NAME", "SYNARA_LOGIN_COOKIE_DOMAIN",
		"SYNARA_DESKTOP_ENROLLMENT_TTL",
		"SYNARA_LOGIN_COOKIE_PATH", "SYNARA_LOGIN_COOKIE_SAME_SITE", "SYNARA_TRUSTED_PROXY_CIDRS",
		"SYNARA_CONTROL_PLANE_SHUTDOWN_TIMEOUT", "SYNARA_WORKER_LEASE_TTL",
		"SYNARA_DATABASE_MAX_OPEN_CONNECTIONS", "SYNARA_DATABASE_MAX_IDLE_CONNECTIONS",
		"SYNARA_DATABASE_CONNECTION_MAX_LIFETIME", "SYNARA_DATABASE_CONNECTION_MAX_IDLE_TIME",
		"SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT",
		"SYNARA_WORKER_HEARTBEAT_TIMEOUT", "SYNARA_WORKER_RECEIPT_TTL",
		"SYNARA_PROVIDER_CREDENTIAL_ACCESS_TTL",
		"SYNARA_PROVIDER_CURSOR_KEY", "SYNARA_PROVIDER_CURSOR_KEY_ID",
		"SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON", "SYNARA_PROVIDER_CURSOR_KEY_OLD_V1",
		"SYNARA_PROVIDER_CURSOR_MAX_AGE",
		"SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON", "SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT",
		"SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT", "SYNARA_LOCAL_AGENTD_RESTART_BACKOFF",
		"SYNARA_CREDENTIAL_KMS_PROVIDER", "SYNARA_CREDENTIAL_KMS_KEY_ID",
		"SYNARA_CREDENTIAL_MASTER_KEY", "SYNARA_CREDENTIAL_KMS_AWS_REGION",
		"SYNARA_CREDENTIAL_KMS_ENDPOINT", "SYNARA_CREDENTIAL_KMS_CA_FILE",
		"SYNARA_CREDENTIAL_KMS_CLIENT_CERT_FILE", "SYNARA_CREDENTIAL_KMS_CLIENT_KEY_FILE",
		"SYNARA_CREDENTIAL_KMS_TIMEOUT",
		"SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON", "SYNARA_OLD_CREDENTIAL_KEY",
		"SYNARA_PUBLIC_CONTROL_PLANE_URL", "SYNARA_PUBLIC_ADMIN_URL", "SYNARA_INTERNAL_STATUS_BOARD_URL",
		"SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL", "SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY",
		"SYNARA_INTERNAL_INCIDENT_PUBLISHER_TIMEOUT",
		"SYNARA_PUBLIC_STATUS_PAGE_URL",
		"SYNARA_COMMERCIALIZATION_MODE", "SYNARA_COMMERCIAL_BILLING_PROVIDER", "SYNARA_COMMERCIAL_BILLING_RETURN_URL",
		"SYNARA_STRIPE_SECRET_KEY", "SYNARA_STRIPE_WEBHOOK_SECRET", "SYNARA_STRIPE_PRICE_MAP_JSON",
		"SYNARA_STRIPE_AUTOMATIC_TAX_ENABLED", "SYNARA_STRIPE_CHECKOUT_TTL",
		"SYNARA_STRIPE_PORTAL_CONFIGURATION_ID",
		"SYNARA_AGENTD_BINARY_PATH",
		"SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT", "SYNARA_SSH_WORKER_OBSERVABILITY_ROOT",
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
		"SYNARA_COST_ACCOUNTING_BLOB_SOURCE", "SYNARA_COST_ACCOUNTING_LOCAL_BASE_DIR",
		"SYNARA_COST_ACCOUNTING_S3_BUCKET", "SYNARA_COST_ACCOUNTING_S3_REGION", "SYNARA_COST_ACCOUNTING_S3_ENDPOINT",
		"SYNARA_COST_ACCOUNTING_S3_USE_PATH_STYLE", "SYNARA_COST_ACCOUNTING_S3_ALLOW_CUSTOM_ENDPOINT",
		"SYNARA_COST_ACCOUNTING_S3_ALLOW_HTTP", "SYNARA_COST_ACCOUNTING_GCS_BUCKET",
		"SYNARA_COST_ACCOUNTING_AZURE_CONTAINER_URL", "SYNARA_COST_ACCOUNTING_AZURE_ALLOW_HTTP",
		"SYNARA_COST_ACCOUNTING_BLOB_PREFIX", "SYNARA_COST_ACCOUNTING_MAX_OBJECT_BYTES",
		"SYNARA_COST_ACCOUNTING_IMPORT_MAPPINGS_JSON", "SYNARA_COST_ACCOUNTING_SHARED_ALLOCATION_MAPPINGS_JSON",
		"SYNARA_COST_ACCOUNTING_OPERATOR_TENANT_ID",
		"SYNARA_PLATFORM_OPERATOR_TENANT_ID",
	} {
		t.Setenv(name, "")
	}
}

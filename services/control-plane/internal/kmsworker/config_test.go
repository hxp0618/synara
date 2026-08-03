package kmsworker

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRuntimeConfigRequiresSeparatedMTLSRolesAndSecureSealMaterial(t *testing.T) {
	directory := t.TempDir()
	sealFile := filepath.Join(directory, "seal.key")
	if err := os.WriteFile(sealFile, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	setRuntimeConfigEnvironment(t)
	t.Setenv("SYNARA_KMS_SEAL_KEY_FILE", sealFile)
	t.Setenv("SYNARA_KMS_TLS_CERT_FILE", "/run/kms/server.crt")
	t.Setenv("SYNARA_KMS_TLS_KEY_FILE", "/run/kms/server.key")
	t.Setenv("SYNARA_KMS_TLS_CLIENT_CA_FILE", "/run/kms/client-ca.crt")
	t.Setenv("SYNARA_KMS_IDENTITY_ROLES_JSON", `{
        "spiffe://synara/control-plane":["control-plane-cryptor"],
        "spiffe://synara/manager":["kms-key-manager"],
        "spiffe://synara/disabler":["kms-key-disabler"],
        "spiffe://synara/approver":["kms-deletion-approver"],
        "spiffe://synara/auditor":["kms-auditor"]
    }`)
	t.Setenv("SYNARA_KMS_MINIMUM_DELETE_DELAY", "240h")
	config, err := LoadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Store.Driver != "sqlite" || config.ListenAddress != ":3790" || len(config.SealKey) != 32 ||
		config.MinimumDeleteDelay != 10*24*time.Hour || len(config.IdentityRoles) != 5 {
		t.Fatalf("unexpected runtime config: %#v", config)
	}

	setRuntimeConfigEnvironment(t)
	t.Setenv("SYNARA_KMS_SEAL_KEY_FILE", sealFile)
	t.Setenv("SYNARA_KMS_TLS_CERT_FILE", "/run/kms/server.crt")
	t.Setenv("SYNARA_KMS_TLS_KEY_FILE", "/run/kms/server.key")
	t.Setenv("SYNARA_KMS_TLS_CLIENT_CA_FILE", "/run/kms/client-ca.crt")
	t.Setenv("SYNARA_KMS_IDENTITY_ROLES_JSON", `{
        "shared":["control-plane-cryptor","kms-key-manager","kms-key-disabler","kms-deletion-approver","kms-auditor"]
    }`)
	if _, err := LoadRuntimeConfig(); err == nil || !strings.Contains(err.Error(), "must not have management roles") {
		t.Fatalf("expected Control Plane role separation rejection, got %v", err)
	}
}

func TestLoadRuntimeConfigRejectsAmbiguousOrWeakSealConfiguration(t *testing.T) {
	setRuntimeConfigEnvironment(t)
	t.Setenv("SYNARA_KMS_SEAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)))
	t.Setenv("SYNARA_KMS_SEAL_KEY_FILE", "/also/configured")
	if _, err := LoadRuntimeConfig(); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected ambiguous sealing key rejection, got %v", err)
	}

	setRuntimeConfigEnvironment(t)
	t.Setenv("SYNARA_KMS_SEAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)))
	t.Setenv("SYNARA_KMS_TLS_CERT_FILE", "/run/kms/server.crt")
	t.Setenv("SYNARA_KMS_TLS_KEY_FILE", "/run/kms/server.key")
	t.Setenv("SYNARA_KMS_TLS_CLIENT_CA_FILE", "/run/kms/client-ca.crt")
	t.Setenv("SYNARA_KMS_IDENTITY_ROLES_JSON", `{
        "cryptor":["control-plane-cryptor"],"manager":["kms-key-manager"],
        "disabler":["kms-key-disabler"],"approver":["kms-deletion-approver"],"auditor":["kms-auditor"]
    }`)
	t.Setenv("SYNARA_KMS_MINIMUM_DELETE_DELAY", "24h")
	if _, err := LoadRuntimeConfig(); err == nil || !strings.Contains(err.Error(), "168h") {
		t.Fatalf("expected minimum deletion delay rejection, got %v", err)
	}
}

func TestReadBoundedRegularFileRejectsWritableSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("secret"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedRegularFile(path, 100); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("expected writable secret rejection, got %v", err)
	}
}

func setRuntimeConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SYNARA_KMS_LISTEN_ADDRESS", "SYNARA_KMS_DATABASE_DRIVER", "SYNARA_KMS_DATABASE_URL", "SYNARA_KMS_SQLITE_PATH",
		"SYNARA_KMS_SEAL_KEY", "SYNARA_KMS_SEAL_KEY_FILE", "SYNARA_KMS_FALLBACK_SEAL_KEY_FILES_JSON",
		"SYNARA_KMS_TLS_CERT_FILE", "SYNARA_KMS_TLS_KEY_FILE", "SYNARA_KMS_TLS_CLIENT_CA_FILE",
		"SYNARA_KMS_IDENTITY_ROLES_JSON", "SYNARA_KMS_MINIMUM_DELETE_DELAY", "SYNARA_KMS_INVENTORY_MAXIMUM_AGE",
		"SYNARA_KMS_SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

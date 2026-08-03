package kmsworker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RuntimeConfig struct {
	ListenAddress       string
	Store               StoreConfig
	SealKey             []byte
	FallbackSealKeys    [][]byte
	TLSCertificateFile  string
	TLSPrivateKeyFile   string
	TLSClientCAFile     string
	IdentityRoles       map[string][]Role
	MinimumDeleteDelay  time.Duration
	InventoryMaximumAge time.Duration
	ShutdownTimeout     time.Duration
}

func LoadRuntimeConfig() (RuntimeConfig, error) {
	config := RuntimeConfig{
		ListenAddress: envDefault("SYNARA_KMS_LISTEN_ADDRESS", ":3790"),
		Store: StoreConfig{
			Driver:      strings.ToLower(envDefault("SYNARA_KMS_DATABASE_DRIVER", "sqlite")),
			DatabaseURL: strings.TrimSpace(os.Getenv("SYNARA_KMS_DATABASE_URL")),
			SQLitePath:  envDefault("SYNARA_KMS_SQLITE_PATH", "/data/kms/kms.db"),
		},
		TLSCertificateFile: strings.TrimSpace(os.Getenv("SYNARA_KMS_TLS_CERT_FILE")),
		TLSPrivateKeyFile:  strings.TrimSpace(os.Getenv("SYNARA_KMS_TLS_KEY_FILE")),
		TLSClientCAFile:    strings.TrimSpace(os.Getenv("SYNARA_KMS_TLS_CLIENT_CA_FILE")),
	}
	var err error
	if config.MinimumDeleteDelay, err = durationEnv("SYNARA_KMS_MINIMUM_DELETE_DELAY", 7*24*time.Hour); err != nil {
		return RuntimeConfig{}, err
	}
	if config.InventoryMaximumAge, err = durationEnv("SYNARA_KMS_INVENTORY_MAXIMUM_AGE", time.Hour); err != nil {
		return RuntimeConfig{}, err
	}
	if config.ShutdownTimeout, err = durationEnv("SYNARA_KMS_SHUTDOWN_TIMEOUT", 20*time.Second); err != nil {
		return RuntimeConfig{}, err
	}
	if config.MinimumDeleteDelay < 7*24*time.Hour {
		return RuntimeConfig{}, errors.New("SYNARA_KMS_MINIMUM_DELETE_DELAY must be at least 168h")
	}
	config.SealKey, err = loadKeyMaterial("SYNARA_KMS_SEAL_KEY", "SYNARA_KMS_SEAL_KEY_FILE")
	if err != nil {
		return RuntimeConfig{}, err
	}
	config.FallbackSealKeys, err = loadFallbackKeyFiles(os.Getenv("SYNARA_KMS_FALLBACK_SEAL_KEY_FILES_JSON"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	config.IdentityRoles, err = parseIdentityRoles(os.Getenv("SYNARA_KMS_IDENTITY_ROLES_JSON"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	if config.TLSCertificateFile == "" || config.TLSPrivateKeyFile == "" || config.TLSClientCAFile == "" {
		return RuntimeConfig{}, errors.New("SYNARA_KMS_TLS_CERT_FILE, SYNARA_KMS_TLS_KEY_FILE and SYNARA_KMS_TLS_CLIENT_CA_FILE are required")
	}
	if _, _, err := net.SplitHostPort(config.ListenAddress); err != nil {
		return RuntimeConfig{}, fmt.Errorf("SYNARA_KMS_LISTEN_ADDRESS is invalid: %w", err)
	}
	return config, nil
}

func (c RuntimeConfig) ServerTLSConfig() (*tls.Config, error) {
	encodedCertificate, err := readBoundedRegularFile(c.TLSCertificateFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("load KMS server certificate: %w", err)
	}
	encodedPrivateKey, err := readBoundedRegularFile(c.TLSPrivateKeyFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("load KMS server private key: %w", err)
	}
	certificate, err := tls.X509KeyPair(encodedCertificate, encodedPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("load KMS server certificate: %w", err)
	}
	encodedCA, err := readBoundedRegularFile(c.TLSClientCAFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("load KMS client CA: %w", err)
	}
	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(encodedCA) {
		return nil, errors.New("SYNARA_KMS_TLS_CLIENT_CA_FILE contains no certificates")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientRoots,
	}, nil
}

func loadKeyMaterial(environmentName, fileEnvironmentName string) ([]byte, error) {
	encodedEnvironment := strings.TrimSpace(os.Getenv(environmentName))
	path := strings.TrimSpace(os.Getenv(fileEnvironmentName))
	if (encodedEnvironment == "") == (path == "") {
		return nil, fmt.Errorf("exactly one of %s and %s is required", environmentName, fileEnvironmentName)
	}
	encoded := []byte(encodedEnvironment)
	if path != "" {
		var err error
		encoded, err = readBoundedRegularFile(path, 4096)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", fileEnvironmentName, err)
		}
	}
	return decode32ByteKey(strings.TrimSpace(string(encoded)), environmentName)
}

func loadFallbackKeyFiles(raw string) ([][]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var paths []string
	if err := decoder.Decode(&paths); err != nil {
		return nil, fmt.Errorf("SYNARA_KMS_FALLBACK_SEAL_KEY_FILES_JSON must be an array of paths: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("SYNARA_KMS_FALLBACK_SEAL_KEY_FILES_JSON must contain exactly one JSON value")
	}
	if len(paths) == 0 || len(paths) > 8 {
		return nil, errors.New("SYNARA_KMS_FALLBACK_SEAL_KEY_FILES_JSON must contain between one and eight paths")
	}
	result := make([][]byte, 0, len(paths))
	seen := map[string]struct{}{}
	for index, rawPath := range paths {
		path := strings.TrimSpace(rawPath)
		if path == "" {
			return nil, fmt.Errorf("fallback sealing key path %d is empty", index)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolve fallback sealing key path %d: %w", index, err)
		}
		if _, exists := seen[resolved]; exists {
			return nil, fmt.Errorf("fallback sealing key path %d is duplicated", index)
		}
		seen[resolved] = struct{}{}
		encoded, err := readBoundedRegularFile(path, 4096)
		if err != nil {
			return nil, fmt.Errorf("read fallback sealing key path %d: %w", index, err)
		}
		key, err := decode32ByteKey(strings.TrimSpace(string(encoded)), fmt.Sprintf("fallback sealing key %d", index))
		if err != nil {
			return nil, err
		}
		result = append(result, key)
	}
	return result, nil
}

func parseIdentityRoles(raw string) (map[string][]Role, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("SYNARA_KMS_IDENTITY_ROLES_JSON is required")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var encoded map[string][]Role
	if err := decoder.Decode(&encoded); err != nil {
		return nil, fmt.Errorf("SYNARA_KMS_IDENTITY_ROLES_JSON is invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("SYNARA_KMS_IDENTITY_ROLES_JSON must contain exactly one JSON value")
	}
	if _, err := NewAuthorizer(encoded); err != nil {
		return nil, err
	}
	required := map[Role]bool{
		RoleControlPlaneCryptor: false, RoleKeyManager: false, RoleKeyDisabler: false,
		RoleDeletionApprover: false, RoleAuditor: false,
	}
	for identity, roles := range encoded {
		roleSet := map[Role]bool{}
		for _, role := range roles {
			required[role] = true
			roleSet[role] = true
		}
		if roleSet[RoleControlPlaneCryptor] && len(roleSet) != 1 {
			return nil, fmt.Errorf("Control Plane cryptor identity %q must not have management roles", identity)
		}
		if roleSet[RoleKeyDisabler] && roleSet[RoleDeletionApprover] {
			return nil, fmt.Errorf("KMS identity %q must not both request and approve deletion", identity)
		}
	}
	for role, configured := range required {
		if !configured {
			return nil, fmt.Errorf("SYNARA_KMS_IDENTITY_ROLES_JSON requires role %q", role)
		}
	}
	return encoded, nil
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path must resolve to a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("file must not be writable by group or other users")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return encoded, nil
}

func decode32ByteKey(encoded, name string) ([]byte, error) {
	var decoded []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err = encoding.DecodeString(encoded)
		if err == nil {
			break
		}
	}
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%s must be a base64-encoded 32-byte key", name)
	}
	return decoded, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func boolEnv(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

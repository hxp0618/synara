package agentd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	ObservabilityEnvironmentFileVariable = "SYNARA_AGENTD_OBSERVABILITY_ENV_FILE"
	observabilityEnvironmentFilename     = "observability.env"
	maximumObservabilityEnvironmentBytes = 32 * 1024
)

var observabilityEnvironmentNames = map[string]struct{}{
	"OTEL_EXPORTER_OTLP_ENDPOINT":      {},
	"OTEL_EXPORTER_OTLP_PROTOCOL":      {},
	"OTEL_EXPORTER_OTLP_CERTIFICATE":   {},
	"SYNARA_OTEL_TRACE_SAMPLE_RATIO":   {},
	"SYNARA_OTEL_COLLECTOR_REGION":     {},
	"SYNARA_OTEL_TRACE_RETENTION_DAYS": {},
}

var observabilityForbiddenAmbientNames = []string{
	"OTEL_SDK_DISABLED",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_INSECURE",
	"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
	"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_TIMEOUT",
	"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
	"OTEL_EXPORTER_OTLP_COMPRESSION",
	"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
}

// LoadObservabilityEnvironment reads only non-secret exporter settings and an
// optional server CA path from a deployment-authority-owned, Target-scoped file.
// It must run after LoadConfig and before tracing.Configure.
func LoadObservabilityEnvironment(cfg Config) error {
	rawPath := strings.TrimSpace(os.Getenv(ObservabilityEnvironmentFileVariable))
	if rawPath == "" {
		return nil
	}
	for _, name := range observabilityForbiddenAmbientNames {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return fmt.Errorf("agentd observability file authority forbids ambient %s", name)
		}
	}
	if cfg.TargetKind == "local" {
		return errors.New(ObservabilityEnvironmentFileVariable + " is only valid for non-Local workers")
	}
	path, err := validateObservabilityEnvironmentFile(rawPath, cfg)
	if err != nil {
		return err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read agentd observability environment: %w", err)
	}
	if len(payload) > maximumObservabilityEnvironmentBytes || !utf8.Valid(payload) || bytes.IndexByte(payload, 0) >= 0 {
		return errors.New("agentd observability environment must be bounded UTF-8 text without NUL bytes")
	}
	values, err := parseObservabilityEnvironment(payload)
	if err != nil {
		return err
	}
	if err := validateObservabilityCertificatePaths(filepath.Dir(path), values); err != nil {
		return err
	}
	for name, value := range values {
		if current := strings.TrimSpace(os.Getenv(name)); current != "" && current != value {
			return fmt.Errorf("agentd observability environment conflicts with process %s", name)
		}
	}
	for name, value := range values {
		if value == "" || strings.TrimSpace(os.Getenv(name)) != "" {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("apply agentd observability environment %s: %w", name, err)
		}
	}
	return nil
}

func validateObservabilityCertificatePaths(directory string, values map[string]string) error {
	want := map[string]string{
		"OTEL_EXPORTER_OTLP_CERTIFICATE": "ca.crt",
	}
	for name, filename := range want {
		value := strings.TrimSpace(values[name])
		if value == "" {
			continue
		}
		if filepath.Clean(value) != filepath.Join(directory, filename) {
			return fmt.Errorf("agentd observability environment %s must use the Target-local %s path", name, filename)
		}
	}
	return nil
}

func validateObservabilityEnvironmentFile(rawPath string, cfg Config) (string, error) {
	path := filepath.Clean(rawPath)
	if !filepath.IsAbs(path) || cfg.ExecutionTargetID == uuid.Nil ||
		filepath.Base(path) != observabilityEnvironmentFilename ||
		filepath.Base(filepath.Dir(path)) != cfg.ExecutionTargetID.String() {
		return "", errors.New(ObservabilityEnvironmentFileVariable + " must be an absolute Target-scoped observability.env path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect agentd observability environment: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("agentd observability environment must be a regular file, not a symlink")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(resolved) != path {
		return "", errors.New("agentd observability environment path must not traverse symlinks")
	}
	if err := validateObservabilityEnvironmentPermissions(path, info); err != nil {
		return "", err
	}
	return path, nil
}

func parseObservabilityEnvironment(payload []byte) (map[string]string, error) {
	result := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 1024), 4096)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !found {
			return nil, fmt.Errorf("agentd observability environment line %d must use NAME=VALUE", lineNumber)
		}
		if _, allowed := observabilityEnvironmentNames[name]; !allowed {
			return nil, fmt.Errorf("agentd observability environment line %d uses forbidden key %s", lineNumber, name)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("agentd observability environment repeats %s", name)
		}
		if len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("agentd observability environment %s has an invalid value", name)
		}
		result[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("agentd observability environment contains an overlong line")
	}
	return result, nil
}

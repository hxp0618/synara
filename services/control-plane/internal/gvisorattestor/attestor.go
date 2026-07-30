package gvisorattestor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	runtimeHandlerAnnotation   = "synara.io/gvisor-runtime-handler"
	runtimeVersionAnnotation   = "synara.io/gvisor-runtime-version"
	runtimeBinaryAnnotation    = "synara.io/gvisor-runtime-binary-sha256"
	runtimeConfigAnnotation    = "synara.io/gvisor-runtime-config-sha256"
	attestorInstanceAnnotation = "synara.io/gvisor-attestor-instance"
	attestedAtAnnotation       = "synara.io/gvisor-attested-at"
	approvedRuntimeHandler     = "runsc"
	approvedContainerdRuntime  = "io.containerd.runsc.v1"
	maximumConfigBytes         = 1 << 20
	maximumRuntimeBinaryBytes  = 512 << 20
	maximumRuntimeVersionBytes = 4 << 10
	maximumTokenBytes          = 64 << 10
	maximumCACertificateBytes  = 1 << 20
)

type Config struct {
	NodeName            string
	RuntimePath         string
	ContainerdConfig    string
	APIServer           string
	BearerTokenFile     string
	CACertificateFile   string
	Interval            time.Duration
	RequestTimeout      time.Duration
	RuntimeProbeTimeout time.Duration
}

type Attestor struct {
	config     Config
	client     *http.Client
	token      string
	instanceID uuid.UUID
	now        func() time.Time
	logger     *slog.Logger
}

func LoadConfig() (Config, error) {
	config := Config{
		NodeName:            strings.TrimSpace(os.Getenv("SYNARA_GVISOR_NODE_NAME")),
		RuntimePath:         envDefault("SYNARA_GVISOR_RUNTIME_PATH", "/host/runsc"),
		ContainerdConfig:    envDefault("SYNARA_GVISOR_CONTAINERD_CONFIG_PATH", "/host/etc/containerd/config.toml"),
		BearerTokenFile:     envDefault("SYNARA_GVISOR_ATTESTOR_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		CACertificateFile:   envDefault("SYNARA_GVISOR_ATTESTOR_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
		Interval:            15 * time.Second,
		RequestTimeout:      10 * time.Second,
		RuntimeProbeTimeout: 5 * time.Second,
	}
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = "443"
	}
	config.APIServer = "https://" + host + ":" + port
	if config.NodeName == "" || host == "" {
		return Config{}, errors.New("gVisor attestor requires its Node name and Kubernetes Service host")
	}
	for _, item := range []struct {
		name string
		path string
	}{
		{name: "runtime", path: config.RuntimePath},
		{name: "containerd config", path: config.ContainerdConfig},
		{name: "bearer token", path: config.BearerTokenFile},
		{name: "CA certificate", path: config.CACertificateFile},
	} {
		if !filepath.IsAbs(item.path) || strings.Contains(item.path, "..") {
			return Config{}, fmt.Errorf("gVisor attestor %s path must be a safe absolute path", item.name)
		}
	}
	return config, nil
}

func New(config Config, logger *slog.Logger) (*Attestor, error) {
	token, err := readBoundedFile(config.BearerTokenFile, maximumTokenBytes)
	if err != nil {
		return nil, fmt.Errorf("read gVisor attestor bearer token: %w", err)
	}
	caCertificate, err := readBoundedFile(config.CACertificateFile, maximumCACertificateBytes)
	if err != nil {
		return nil, fmt.Errorf("read gVisor attestor CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertificate) {
		return nil, errors.New("gVisor attestor CA certificate is invalid")
	}
	return &Attestor{
		config: config,
		client: &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}},
			Timeout:   config.RequestTimeout,
		},
		token: strings.TrimSpace(string(token)), instanceID: uuid.New(),
		now: func() time.Time { return time.Now().UTC() }, logger: logger,
	}, nil
}

func (a *Attestor) Run(ctx context.Context) error {
	if err := a.ObserveAndPublish(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(a.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := a.ObserveAndPublish(ctx); err != nil {
				a.logger.Error("gVisor node attestation failed", "error", err)
			}
		}
	}
}

func (a *Attestor) ObserveAndPublish(ctx context.Context) error {
	probeContext, cancel := context.WithTimeout(ctx, a.config.RuntimeProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeContext, a.config.RuntimePath, "--version")
	version := &boundedWriter{maximum: maximumRuntimeVersionBytes}
	command.Stdout = version
	command.Stderr = io.Discard
	err := command.Run()
	if err != nil {
		return fmt.Errorf("probe runsc version: %w", err)
	}
	if version.overflow {
		return errors.New("runsc version output exceeds the attestor size limit")
	}
	runtimeBinaryDigest, err := hashBoundedFile(a.config.RuntimePath, maximumRuntimeBinaryBytes)
	if err != nil {
		return fmt.Errorf("hash runsc runtime binary: %w", err)
	}
	configFile, err := os.Open(a.config.ContainerdConfig)
	if err != nil {
		return fmt.Errorf("open containerd runtime configuration: %w", err)
	}
	configuration, readErr := io.ReadAll(io.LimitReader(configFile, maximumConfigBytes+1))
	closeErr := configFile.Close()
	if readErr != nil {
		return fmt.Errorf("read containerd runtime configuration: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close containerd runtime configuration: %w", closeErr)
	}
	if len(configuration) > maximumConfigBytes {
		return errors.New("containerd runtime configuration exceeds the attestor size limit")
	}
	observation, err := ObserveRuntime(version.data, configuration, runtimeBinaryDigest)
	if err != nil {
		return err
	}
	return a.patchNode(ctx, observation)
}

type RuntimeObservation struct {
	Version      string
	BinaryDigest string
	ConfigDigest string
}

func ObserveRuntime(version, configuration []byte, runtimeBinaryDigest string) (RuntimeObservation, error) {
	normalizedVersion := strings.Join(strings.Fields(string(version)), " ")
	if normalizedVersion == "" || !strings.Contains(strings.ToLower(normalizedVersion), approvedRuntimeHandler) {
		return RuntimeObservation{}, errors.New("runtime executable did not identify itself as runsc")
	}
	if !lowerHexSHA256(runtimeBinaryDigest) {
		return RuntimeObservation{}, errors.New("runtime executable digest is invalid")
	}
	if !containerdRunscHandlerConfigured(configuration) {
		return RuntimeObservation{}, errors.New("containerd configuration does not map the runsc handler to the gVisor shim")
	}
	configDigest := sha256.Sum256(configuration)
	return RuntimeObservation{
		Version: normalizedVersion, BinaryDigest: runtimeBinaryDigest,
		ConfigDigest: hex.EncodeToString(configDigest[:]),
	}, nil
}

func hashBoundedFile(path string, maximum int64) (string, error) {
	if maximum <= 0 {
		return "", errors.New("file digest size limit must be positive")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return "", errors.New("runtime binary is not a bounded regular file")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	if written > maximum {
		return "", errors.New("runtime binary exceeds the attestor size limit")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func lowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type boundedWriter struct {
	data     []byte
	maximum  int
	overflow bool
}

func (w *boundedWriter) Write(value []byte) (int, error) {
	remaining := w.maximum - len(w.data)
	if remaining > 0 {
		stored := len(value)
		if stored > remaining {
			stored = remaining
		}
		w.data = append(w.data, value[:stored]...)
	}
	if len(value) > remaining {
		w.overflow = true
	}
	return len(value), nil
}

func readBoundedFile(path string, maximum int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maximum {
		return nil, errors.New("file exceeds the attestor size limit")
	}
	return value, nil
}

func containerdRunscHandlerConfigured(configuration []byte) bool {
	lines := bytes.Split(configuration, []byte("\n"))
	inRunscSection := false
	runtimeTypeSeen := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "[") {
			inRunscSection = approvedContainerdRunscSection(trimmed)
			continue
		}
		if !inRunscSection || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != "runtime_type" {
			continue
		}
		if runtimeTypeSeen {
			return false
		}
		runtimeTypeSeen = true
		if !exactQuotedTOMLString(parts[1], approvedContainerdRuntime) {
			return false
		}
	}
	return runtimeTypeSeen
}

func approvedContainerdRunscSection(line string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	switch line {
	case `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]`,
		`[plugins.'io.containerd.grpc.v1.cri'.containerd.runtimes.runsc]`,
		`[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]`,
		`[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]`:
		return true
	default:
		return false
	}
}

func exactQuotedTOMLString(raw, expected string) bool {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || (raw[0] != '\'' && raw[0] != '"') {
		return false
	}
	quote := raw[0]
	closing := strings.IndexByte(raw[1:], quote)
	if closing < 0 {
		return false
	}
	closing++
	if raw[1:closing] != expected {
		return false
	}
	remainder := strings.TrimSpace(raw[closing+1:])
	return remainder == "" || strings.HasPrefix(remainder, "#")
}

func (a *Attestor) patchNode(ctx context.Context, observation RuntimeObservation) error {
	payload, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		runtimeHandlerAnnotation: approvedRuntimeHandler, runtimeVersionAnnotation: observation.Version,
		runtimeBinaryAnnotation: observation.BinaryDigest, runtimeConfigAnnotation: observation.ConfigDigest,
		attestorInstanceAnnotation: a.instanceID.String(),
		attestedAtAnnotation:       a.now().Format(time.RFC3339Nano),
	}}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPatch,
		strings.TrimRight(a.config.APIServer, "/")+"/api/v1/nodes/"+url.PathEscape(a.config.NodeName),
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish gVisor Node attestation: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("publish gVisor Node attestation: Kubernetes API returned status %d", response.StatusCode)
	}
	return nil
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

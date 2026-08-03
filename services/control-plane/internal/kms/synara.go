package kms

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maximumSynaraKMSResponseBytes = 256 << 10

type SynaraKMSConfig struct {
	Endpoint       string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	Timeout        time.Duration
}

type SynaraKeyWrapper struct {
	keyID    string
	logical  string
	version  int64
	endpoint *url.URL
	client   *http.Client
}

func NewSynaraKeyWrapper(ctx context.Context, keyID string, config SynaraKMSConfig, requireEncryptable bool) (*SynaraKeyWrapper, error) {
	client, endpoint, err := newSynaraKMSHTTPClient(config)
	if err != nil {
		return nil, err
	}
	wrapper, err := newSynaraKeyWrapperWithClient(ctx, keyID, endpoint, client, requireEncryptable)
	if err != nil {
		client.CloseIdleConnections()
		return nil, err
	}
	return wrapper, nil
}

func newSynaraKeyWrapperWithClient(
	ctx context.Context,
	keyID string,
	endpoint *url.URL,
	client *http.Client,
	requireEncryptable bool,
) (*SynaraKeyWrapper, error) {
	logical, version, err := parseSynaraVersionedKeyID(keyID)
	if err != nil {
		return nil, err
	}
	if endpoint == nil || client == nil {
		return nil, errors.New("Synara credential KMS endpoint and HTTP client are required")
	}
	wrapper := &SynaraKeyWrapper{
		keyID: keyID, logical: logical, version: version,
		endpoint: endpoint, client: client,
	}
	description, err := wrapper.describe(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe Synara credential KMS key: %w", err)
	}
	var matched *synaraKMSVersionDescription
	for index := range description.Versions {
		if description.Versions[index].KeyID == keyID {
			matched = &description.Versions[index]
			break
		}
	}
	if matched == nil {
		return nil, errors.New("Synara credential KMS key version was not returned by the server")
	}
	if requireEncryptable && !matched.Encryptable {
		return nil, errors.New("Synara credential KMS primary key version is not encryptable")
	}
	if !requireEncryptable && !matched.Decryptable {
		return nil, errors.New("Synara credential KMS decrypt key version is not decryptable")
	}
	return wrapper, nil
}

func (w *SynaraKeyWrapper) Provider() string { return "synara-kms" }
func (w *SynaraKeyWrapper) KeyID() string    { return w.keyID }

func (w *SynaraKeyWrapper) WrapKey(ctx context.Context, dataKey, aad []byte) ([]byte, error) {
	if len(dataKey) != 32 || len(aad) == 0 {
		return nil, errors.New("Synara credential KMS data key or AAD is invalid")
	}
	request := struct {
		DataKey []byte `json:"dataKey"`
		AAD     []byte `json:"aad"`
	}{DataKey: dataKey, AAD: aad}
	var response struct {
		KeyID          string `json:"keyId"`
		WrappedDataKey []byte `json:"wrappedDataKey"`
	}
	if err := w.call(ctx, http.MethodPost, fmt.Sprintf("/v1/keys/%s/versions/%d:wrap", w.logical, w.version), request, &response); err != nil {
		return nil, err
	}
	if response.KeyID != w.keyID || len(response.WrappedDataKey) == 0 {
		return nil, errors.New("Synara credential KMS returned a mismatched Wrap identity")
	}
	return response.WrappedDataKey, nil
}

func (w *SynaraKeyWrapper) UnwrapKey(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	if len(wrapped) == 0 || len(aad) == 0 {
		return nil, errors.New("Synara credential KMS wrapped key or AAD is invalid")
	}
	request := struct {
		WrappedDataKey []byte `json:"wrappedDataKey"`
		AAD            []byte `json:"aad"`
	}{WrappedDataKey: wrapped, AAD: aad}
	var response struct {
		KeyID   string `json:"keyId"`
		DataKey []byte `json:"dataKey"`
	}
	if err := w.call(ctx, http.MethodPost, fmt.Sprintf("/v1/keys/%s/versions/%d:unwrap", w.logical, w.version), request, &response); err != nil {
		return nil, err
	}
	if response.KeyID != w.keyID || len(response.DataKey) != 32 {
		zero(response.DataKey)
		return nil, errors.New("Synara credential KMS returned a mismatched Unwrap identity")
	}
	return response.DataKey, nil
}

func (w *SynaraKeyWrapper) describe(ctx context.Context) (synaraKMSKeyDescription, error) {
	var response synaraKMSKeyDescription
	err := w.call(ctx, http.MethodGet, "/v1/keys/"+w.logical, nil, &response)
	return response, err
}

func (w *SynaraKeyWrapper) call(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	var encoded []byte
	if input != nil {
		var err error
		encoded, err = json.Marshal(input)
		if err != nil {
			return err
		}
		defer zero(encoded)
		body = bytes.NewReader(encoded)
	}
	requestURL := *w.endpoint
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := w.client.Do(request)
	if err != nil {
		return fmt.Errorf("call Synara credential KMS: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumSynaraKMSResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Synara credential KMS response: %w", err)
	}
	defer zero(responseBody)
	if len(responseBody) > maximumSynaraKMSResponseBytes {
		return errors.New("Synara credential KMS response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(responseBody, &problem)
		code := strings.TrimSpace(problem.Error.Code)
		if code == "" {
			code = "kms_unavailable"
		}
		return fmt.Errorf("Synara credential KMS rejected request with %s (%d)", code, response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.New("Synara credential KMS response JSON is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("Synara credential KMS response contains trailing JSON")
	}
	return nil
}

type synaraKMSKeyDescription struct {
	KeyID            string                        `json:"keyId"`
	Name             string                        `json:"name"`
	Description      string                        `json:"description"`
	PolicyReference  string                        `json:"policyReference"`
	Labels           map[string]string             `json:"labels"`
	MetadataRevision int64                         `json:"metadataRevision"`
	ActiveVersion    int64                         `json:"activeVersion"`
	CreatedAt        time.Time                     `json:"createdAt"`
	UpdatedAt        time.Time                     `json:"updatedAt"`
	Versions         []synaraKMSVersionDescription `json:"versions"`
}

type synaraKMSVersionDescription struct {
	KeyID           string     `json:"keyId"`
	Version         int64      `json:"version"`
	Algorithm       string     `json:"algorithm"`
	State           string     `json:"state"`
	Encryptable     bool       `json:"encryptable"`
	Decryptable     bool       `json:"decryptable"`
	EncryptNotAfter *time.Time `json:"encryptNotAfter,omitempty"`
	DecryptNotAfter *time.Time `json:"decryptNotAfter,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	DisabledAt      *time.Time `json:"disabledAt,omitempty"`
	DestroyAfter    *time.Time `json:"destroyAfter,omitempty"`
	DestroyedAt     *time.Time `json:"destroyedAt,omitempty"`
}

func newSynaraKMSHTTPClient(config SynaraKMSConfig) (*http.Client, *url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, nil, errors.New("Synara credential KMS endpoint must be an HTTPS origin")
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return nil, nil, errors.New("Synara credential KMS endpoint must not contain a path")
	}
	encodedCertificate, err := readSynaraKMSFile(strings.TrimSpace(config.ClientCertFile), 1<<20)
	if err != nil {
		return nil, nil, fmt.Errorf("load Synara credential KMS client certificate: %w", err)
	}
	encodedPrivateKey, err := readSynaraKMSFile(strings.TrimSpace(config.ClientKeyFile), 1<<20)
	if err != nil {
		return nil, nil, fmt.Errorf("load Synara credential KMS client private key: %w", err)
	}
	certificate, err := tls.X509KeyPair(encodedCertificate, encodedPrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load Synara credential KMS client certificate: %w", err)
	}
	encodedCA, err := readSynaraKMSFile(strings.TrimSpace(config.CAFile), 1<<20)
	if err != nil {
		return nil, nil, fmt.Errorf("read Synara credential KMS CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(encodedCA) {
		return nil, nil, errors.New("Synara credential KMS CA file contains no certificates")
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if timeout <= 0 || timeout > time.Minute {
		return nil, nil, errors.New("Synara credential KMS timeout is invalid")
	}
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate},
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: timeout}, endpoint, nil
}

func readSynaraKMSFile(path string, limit int64) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("KMS TLS file must be regular and not writable by group or other users")
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
		return nil, errors.New("KMS TLS file exceeds size limit")
	}
	return encoded, nil
}

func parseSynaraVersionedKeyID(value string) (string, int64, error) {
	value = strings.TrimSpace(value)
	logical, rawVersion, found := strings.Cut(value, "/versions/")
	parsed, parseErr := uuid.Parse(logical)
	version, versionErr := strconv.ParseInt(rawVersion, 10, 64)
	if !found || parseErr != nil || parsed.String() != logical || versionErr != nil || version <= 0 || fmt.Sprintf("%s/versions/%d", logical, version) != value {
		return "", 0, errors.New("Synara credential KMS key ID must be an immutable UUID/version identity")
	}
	return logical, version, nil
}

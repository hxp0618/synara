package credentials

import (
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	PurposeProvider = "provider"
	PurposeGit      = "git"
	PurposeRegistry = "registry"
	PurposePackage  = "package"

	GitProvider            = "git"
	GitHTTPSCredentialType = "https_token"
	GitSSHCredentialType   = "ssh_key"

	RegistryProviderOci             = "oci"
	RegistryBasicCredentialType     = "basic"
	RegistryBearerCredentialType    = "bearer_token"
	PackageProviderNPM              = "npm"
	PackageProviderPyPI             = "pypi"
	PackageNPMTokenCredentialType   = "npm_token"
	PackagePyPITokenCredentialType  = "pypi_token"
	minimumGuardableSecretLength    = 8
	maximumCredentialStringLength   = 16 << 10
	maximumCredentialUsernameLength = 512
)

type GitHTTPSPayload struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type GitSSHPayload struct {
	Host                 string `json:"host"`
	Port                 int    `json:"port"`
	Username             string `json:"username"`
	PrivateKey           string `json:"privateKey"`
	PrivateKeyPassphrase string `json:"privateKeyPassphrase,omitempty"`
	HostKey              string `json:"hostKey"`
}

type RegistryBasicPayload struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type RegistryBearerPayload struct {
	Host  string `json:"host"`
	Token string `json:"token"`
}

type PackageNPMPayload struct {
	RegistryURL string   `json:"registryUrl"`
	Token       string   `json:"token"`
	Scopes      []string `json:"scopes,omitempty"`
}

type PackagePyPIPayload struct {
	IndexURL string `json:"indexUrl"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func normalizePurpose(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = PurposeProvider
	}
	if value != PurposeProvider && value != PurposeGit && value != PurposeRegistry && value != PurposePackage {
		return "", problem.New(400, "invalid_credential_purpose", "Credential purpose must be provider, git, registry, or package.")
	}
	return value, nil
}

func normalizeCredentialPayload(
	purpose, provider, credentialType string,
	payload map[string]any,
) (map[string]any, []byte, error) {
	if purpose == PurposeProvider {
		encoded, err := encodePayload(payload)
		return payload, encoded, err
	}
	var normalized map[string]any
	var err error
	switch purpose {
	case PurposeGit:
		normalized, err = normalizeGitPayload(provider, credentialType, payload)
	case PurposeRegistry:
		normalized, err = normalizeRegistryPayload(provider, credentialType, payload)
	case PurposePackage:
		normalized, err = normalizePackagePayload(provider, credentialType, payload)
	default:
		err = problem.New(400, "invalid_credential_purpose", "Credential purpose is invalid.")
	}
	if err != nil {
		return nil, nil, err
	}
	encoded, err := encodePayload(normalized)
	return normalized, encoded, err
}

func normalizeGitPayload(provider, credentialType string, payload map[string]any) (map[string]any, error) {
	if provider != GitProvider {
		return nil, problem.New(400, "invalid_git_credential_type", "Git Credentials require provider git.")
	}
	switch credentialType {
	case GitHTTPSCredentialType:
		normalized, err := normalizeGitHTTPSPayload(payload)
		if err != nil {
			return nil, err
		}
		return map[string]any{"host": normalized.Host, "username": normalized.Username, "token": normalized.Token}, nil
	case GitSSHCredentialType:
		normalized, err := normalizeGitSSHPayload(payload)
		if err != nil {
			return nil, err
		}
		value := map[string]any{
			"host": normalized.Host, "port": normalized.Port, "username": normalized.Username,
			"privateKey": normalized.PrivateKey, "hostKey": normalized.HostKey,
		}
		if normalized.PrivateKeyPassphrase != "" {
			value["privateKeyPassphrase"] = normalized.PrivateKeyPassphrase
		}
		return value, nil
	default:
		return nil, problem.New(400, "invalid_git_credential_type", "Git Credentials support https_token or ssh_key.")
	}
}

func normalizeGitHTTPSPayload(payload map[string]any) (GitHTTPSPayload, error) {
	if len(payload) != 3 {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	host, hostOK := payload["host"].(string)
	username, usernameOK := payload["username"].(string)
	token, tokenOK := payload["token"].(string)
	if !hostOK || !usernameOK || !tokenOK {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	normalizedHost, err := normalizeGitHost(host)
	if err != nil {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maximumCredentialUsernameLength || containsControl(username) {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	if !validSecret(token) {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	return GitHTTPSPayload{Host: normalizedHost, Username: username, Token: token}, nil
}

func normalizeGitSSHPayload(payload map[string]any) (GitSSHPayload, error) {
	if len(payload) < 5 || len(payload) > 6 || !payloadHasOnlyKeys(
		payload, "host", "port", "username", "privateKey", "privateKeyPassphrase", "hostKey",
	) {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	host, hostOK := payload["host"].(string)
	username, usernameOK := payload["username"].(string)
	privateKey, privateKeyOK := payload["privateKey"].(string)
	hostKey, hostKeyOK := payload["hostKey"].(string)
	port, portOK := integerPayloadValue(payload["port"])
	passphrase := ""
	if raw, present := payload["privateKeyPassphrase"]; present {
		var ok bool
		passphrase, ok = raw.(string)
		if !ok || !validSecret(passphrase) {
			return GitSSHPayload{}, invalidGitSSHCredentialPayload()
		}
	}
	if !hostOK || !usernameOK || !privateKeyOK || !hostKeyOK || !portOK || port < 1 || port > 65535 {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	normalizedHost, err := normalizeGitHost(host)
	if err != nil {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maximumCredentialUsernameLength || containsControl(username) {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	if len(privateKey) < minimumGuardableSecretLength || len(privateKey) > maxCredentialPayloadBytes {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	signer, err := parseSSHPrivateKey([]byte(privateKey), []byte(passphrase))
	if err != nil || !allowedSSHKey(signer.PublicKey()) {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	parsedHostKey, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil || strings.TrimSpace(comment) != "" || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 || !allowedSSHKey(parsedHostKey) {
		return GitSSHPayload{}, invalidGitSSHCredentialPayload()
	}
	normalizedHostKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(parsedHostKey)))
	return GitSSHPayload{
		Host: normalizedHost, Port: port, Username: username, PrivateKey: privateKey,
		PrivateKeyPassphrase: passphrase, HostKey: normalizedHostKey,
	}, nil
}

func normalizeRegistryPayload(provider, credentialType string, payload map[string]any) (map[string]any, error) {
	if provider != RegistryProviderOci {
		return nil, problem.New(400, "invalid_registry_credential_type", "Registry Credentials require provider oci.")
	}
	switch credentialType {
	case RegistryBasicCredentialType:
		if len(payload) != 3 {
			return nil, invalidRegistryCredentialPayload()
		}
		host, hostOK := payload["host"].(string)
		username, usernameOK := payload["username"].(string)
		password, passwordOK := payload["password"].(string)
		normalizedHost, err := normalizeRegistryHost(host)
		username = strings.TrimSpace(username)
		if !hostOK || !usernameOK || !passwordOK || err != nil || username == "" ||
			len(username) > maximumCredentialUsernameLength || containsControl(username) || !validSecret(password) {
			return nil, invalidRegistryCredentialPayload()
		}
		return map[string]any{"host": normalizedHost, "username": username, "password": password}, nil
	case RegistryBearerCredentialType:
		if len(payload) != 2 {
			return nil, invalidRegistryCredentialPayload()
		}
		host, hostOK := payload["host"].(string)
		token, tokenOK := payload["token"].(string)
		normalizedHost, err := normalizeRegistryHost(host)
		if !hostOK || !tokenOK || err != nil || !validSecret(token) {
			return nil, invalidRegistryCredentialPayload()
		}
		return map[string]any{"host": normalizedHost, "token": token}, nil
	default:
		return nil, problem.New(400, "invalid_registry_credential_type", "OCI Registry Credentials support basic or bearer_token.")
	}
}

func normalizePackagePayload(provider, credentialType string, payload map[string]any) (map[string]any, error) {
	switch {
	case provider == PackageProviderNPM && credentialType == PackageNPMTokenCredentialType:
		if len(payload) < 2 || len(payload) > 3 || !payloadHasOnlyKeys(payload, "registryUrl", "token", "scopes") {
			return nil, invalidPackageCredentialPayload()
		}
		registryURL, urlOK := payload["registryUrl"].(string)
		token, tokenOK := payload["token"].(string)
		normalizedURL, err := normalizeHTTPSCredentialURL(registryURL)
		if !urlOK || !tokenOK || err != nil || !validSecret(token) {
			return nil, invalidPackageCredentialPayload()
		}
		value := map[string]any{"registryUrl": normalizedURL, "token": token}
		if rawScopes, present := payload["scopes"]; present {
			scopes, scopeErr := normalizePackageScopes(rawScopes)
			if scopeErr != nil {
				return nil, invalidPackageCredentialPayload()
			}
			value["scopes"] = scopes
		}
		return value, nil
	case provider == PackageProviderPyPI && credentialType == PackagePyPITokenCredentialType:
		if len(payload) != 3 {
			return nil, invalidPackageCredentialPayload()
		}
		indexURL, urlOK := payload["indexUrl"].(string)
		username, usernameOK := payload["username"].(string)
		token, tokenOK := payload["token"].(string)
		normalizedURL, err := normalizeHTTPSCredentialURL(indexURL)
		username = strings.TrimSpace(username)
		if !urlOK || !usernameOK || !tokenOK || err != nil || username == "" ||
			len(username) > maximumCredentialUsernameLength || containsControl(username) || !validSecret(token) {
			return nil, invalidPackageCredentialPayload()
		}
		return map[string]any{"indexUrl": normalizedURL, "username": username, "token": token}, nil
	default:
		return nil, problem.New(400, "invalid_package_credential_type", "Package Credentials support npm/npm_token or pypi/pypi_token.")
	}
}

func decodeGitHTTPSPayload(payload map[string]any) (GitHTTPSPayload, error) {
	return normalizeGitHTTPSPayload(payload)
}

func decodeGitSSHPayload(payload map[string]any) (GitSSHPayload, error) {
	return normalizeGitSSHPayload(payload)
}

func normalizeGitHost(value string) (string, error) {
	normalized, err := gitpolicy.NormalizeHostname(value)
	if err != nil {
		return "", problem.New(400, "invalid_git_credential_host", "Git Credential host is invalid.")
	}
	return normalized, nil
}

func normalizeRegistryHost(value string) (string, error) {
	normalized, err := gitpolicy.NormalizeHostname(value)
	if err != nil || !publicCredentialHostname(normalized) {
		return "", errors.New("invalid public registry host")
	}
	return normalized, nil
}

func normalizeHTTPSCredentialURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 || containsControl(value) {
		return "", errors.New("invalid Credential URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid HTTPS Credential URL")
	}
	hostname, err := normalizeRegistryHost(parsed.Hostname())
	port := parsed.Port()
	if err != nil || (port != "" && port != "443") {
		return "", errors.New("invalid HTTPS Credential URL host")
	}
	parsed.Scheme = "https"
	parsed.Host = hostname
	if port == "443" {
		parsed.Host = net.JoinHostPort(hostname, "443")
	}
	parsed.RawPath = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), nil
}

func publicCredentialHostname(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return true
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

func normalizePackageScopes(raw any) ([]string, error) {
	values := make([]string, 0)
	switch typed := raw.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			value, ok := item.(string)
			if !ok {
				return nil, errors.New("invalid package scope")
			}
			values = append(values, value)
		}
	default:
		return nil, errors.New("invalid package scopes")
	}
	if len(values) == 0 || len(values) > 64 {
		return nil, errors.New("invalid package scopes")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !validPackageScope(value) {
			return nil, errors.New("invalid package scope")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func validPackageScope(value string) bool {
	if len(value) < 2 || len(value) > 214 || value[0] != '@' {
		return false
	}
	for _, character := range value[1:] {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func parseSSHPrivateKey(privateKey, passphrase []byte) (ssh.Signer, error) {
	if len(passphrase) > 0 {
		return ssh.ParsePrivateKeyWithPassphrase(privateKey, passphrase)
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, errors.New("encrypted private key requires passphrase")
	}
	return nil, err
}

func allowedSSHKey(key ssh.PublicKey) bool {
	cryptoKey, ok := key.(ssh.CryptoPublicKey)
	if !ok {
		return false
	}
	switch publicKey := cryptoKey.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		return publicKey.N.BitLen() >= 2048
	case *dsa.PublicKey:
		return false
	case *ecdsa.PublicKey:
		return publicKey.Curve != nil
	case ed25519.PublicKey:
		return len(publicKey) == ed25519.PublicKeySize
	default:
		return false
	}
}

func integerPayloadValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), int64(int(typed)) == typed
	case float64:
		converted := int(typed)
		return converted, float64(converted) == typed
	case json.Number:
		converted, err := strconv.Atoi(string(typed))
		return converted, err == nil
	default:
		return 0, false
	}
}

func validSecret(value string) bool {
	return len(value) >= minimumGuardableSecretLength && len(value) <= maximumCredentialStringLength &&
		strings.TrimSpace(value) == value && !containsControl(value)
}

func payloadHasOnlyKeys(payload map[string]any, allowed ...string) bool {
	accepted := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		accepted[key] = struct{}{}
	}
	for key := range payload {
		if _, ok := accepted[key]; !ok {
			return false
		}
	}
	return true
}

func encodePayload(payload map[string]any) ([]byte, error) {
	if len(payload) == 0 {
		return nil, problem.New(400, "invalid_credential_payload", "Credential payload must not be empty.")
	}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maxCredentialPayloadBytes {
		return nil, problem.New(400, "invalid_credential_payload", "Credential payload must be valid JSON no larger than 65536 bytes.")
	}
	return encoded, nil
}

func invalidGitCredentialPayload() error {
	return problem.New(
		400,
		"invalid_git_credential_payload",
		"Git https_token payload must contain only non-empty host, username, and token strings.",
	)
}

func invalidGitSSHCredentialPayload() error {
	return problem.New(
		400,
		"invalid_git_ssh_credential_payload",
		"Git ssh_key payload must contain one supported private key, its fixed host key, host, port, and username.",
	)
}

func invalidRegistryCredentialPayload() error {
	return problem.New(
		400,
		"invalid_registry_credential_payload",
		"Registry Credential payload is invalid.",
	)
}

func invalidPackageCredentialPayload() error {
	return problem.New(
		400,
		"invalid_package_credential_payload",
		"Package Credential payload is invalid.",
	)
}

func credentialPayloadIdentity(
	purpose, provider, credentialType string,
	payload map[string]any,
) (string, error) {
	switch purpose {
	case PurposeProvider:
		return "", nil
	case PurposeGit:
		switch credentialType {
		case GitHTTPSCredentialType:
			value, err := normalizeGitHTTPSPayload(payload)
			if err != nil {
				return "", err
			}
			return value.Host, nil
		case GitSSHCredentialType:
			value, err := normalizeGitSSHPayload(payload)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s\x00%d\x00%s\x00%s", value.Host, value.Port, value.Username, value.HostKey), nil
		}
	case PurposeRegistry:
		value, err := normalizeRegistryPayload(provider, credentialType, payload)
		if err != nil {
			return "", err
		}
		host, _ := value["host"].(string)
		return host, nil
	case PurposePackage:
		value, err := normalizePackagePayload(provider, credentialType, payload)
		if err != nil {
			return "", err
		}
		if provider == PackageProviderNPM {
			selector, _ := value["registryUrl"].(string)
			return selector, nil
		}
		selector, _ := value["indexUrl"].(string)
		return selector, nil
	}
	return "", problem.New(400, "invalid_credential_type", "Credential type is invalid.")
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

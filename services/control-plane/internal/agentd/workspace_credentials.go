package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"golang.org/x/crypto/ssh"
)

type GitSSHCredential struct {
	Host                 string
	Port                 int
	Username             string
	PrivateKey           string
	PrivateKeyPassphrase string
	HostKey              string
}

type RegistryCredential struct {
	Host           string
	CredentialType string
	Username       string
	Password       string
	Token          string
}

type PackageCredential struct {
	Provider       string
	CredentialType string
	RegistryURL    string
	IndexURL       string
	Username       string
	Token          string
	Scopes         []string
}

type WorkspaceGitCredential struct {
	HTTPS *GitHTTPSCredential
	SSH   *GitSSHCredential
}

func resolveWorkspaceGitCredentialStage(
	ctx context.Context,
	client *Client,
	executionID uuid.UUID,
	lease executions.Lease,
	grants []executions.CredentialGrantDescriptor,
	bindingKind string,
	guard *executionSecretGuard,
) (*WorkspaceGitCredential, error) {
	grant, err := credentialGrantForStage(grants, bindingKind)
	if err != nil || grant == nil {
		return nil, err
	}
	resolved, err := client.ResolveCredentialStage(ctx, executionID, lease, grants, bindingKind)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return nil, errors.New("Credential Grant resolution returned no stage payload")
	}
	defer clearResolvedWorkspaceCredential(resolved)
	credential, err := workspaceGitCredentialFromGrant(*grant, *resolved)
	if err != nil {
		return nil, err
	}
	if guard != nil {
		if err := guard.AddWorkspaceGitCredential(credential); err != nil {
			clearWorkspaceGitCredential(credential)
			return nil, err
		}
	}
	return credential, nil
}

func resolveRegistryCredentialStage(
	ctx context.Context,
	client *Client,
	executionID uuid.UUID,
	lease executions.Lease,
	grants []executions.CredentialGrantDescriptor,
	bindingKind string,
	guard *executionSecretGuard,
) (*RegistryCredential, error) {
	grant, err := credentialGrantForStage(grants, bindingKind)
	if err != nil || grant == nil {
		return nil, err
	}
	resolved, err := client.ResolveCredentialStage(ctx, executionID, lease, grants, bindingKind)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return nil, errors.New("Credential Grant resolution returned no stage payload")
	}
	defer clearResolvedWorkspaceCredential(resolved)
	credential, err := registryCredentialFromGrant(*grant, *resolved)
	if err != nil {
		return nil, err
	}
	if guard != nil {
		if err := guard.AddRegistryCredential(&credential); err != nil {
			clearRegistryCredential(&credential)
			return nil, err
		}
	}
	return &credential, nil
}

func resolvePackageCredentialStage(
	ctx context.Context,
	client *Client,
	executionID uuid.UUID,
	lease executions.Lease,
	grants []executions.CredentialGrantDescriptor,
	bindingKind string,
	guard *executionSecretGuard,
) (*PackageCredential, error) {
	grant, err := credentialGrantForStage(grants, bindingKind)
	if err != nil || grant == nil {
		return nil, err
	}
	resolved, err := client.ResolveCredentialStage(ctx, executionID, lease, grants, bindingKind)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return nil, errors.New("Credential Grant resolution returned no stage payload")
	}
	defer clearResolvedWorkspaceCredential(resolved)
	credential, err := packageCredentialFromGrant(*grant, *resolved)
	if err != nil {
		return nil, err
	}
	if guard != nil {
		if err := guard.AddPackageCredential(&credential); err != nil {
			clearPackageCredential(&credential)
			return nil, err
		}
	}
	return &credential, nil
}

func credentialGrantForStage(
	grants []executions.CredentialGrantDescriptor,
	bindingKind string,
) (*executions.CredentialGrantDescriptor, error) {
	var selected *executions.CredentialGrantDescriptor
	for index := range grants {
		if grants[index].BindingKind != bindingKind {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple active %s Credential Grants are ambiguous", bindingKind)
		}
		selected = &grants[index]
	}
	return selected, nil
}

func gitHTTPSCredentialFromGrant(
	grant executions.CredentialGrantDescriptor,
	resolved ResolvedWorkspaceCredential,
) (GitHTTPSCredential, error) {
	if err := validateResolvedGrant(grant, resolved, "git_fetch", "git", "git", "https_token"); err != nil {
		return GitHTTPSCredential{}, errors.New("resolved Git Credential Grant metadata is invalid")
	}
	if !payloadHasOnlyKeys(resolved.Payload, "host", "username", "token") {
		return GitHTTPSCredential{}, errors.New("resolved Git HTTPS Credential payload is invalid")
	}
	host, hostOK := resolved.Payload["host"].(string)
	username, usernameOK := resolved.Payload["username"].(string)
	token, tokenOK := resolved.Payload["token"].(string)
	host, hostErr := gitpolicy.NormalizeHostname(host)
	username = strings.TrimSpace(username)
	if !hostOK || !usernameOK || !tokenOK || hostErr != nil || host == "" ||
		!safeCredentialUsername(username) || !guardableSecret(token) {
		return GitHTTPSCredential{}, errors.New("resolved Git HTTPS Credential payload is invalid")
	}
	return GitHTTPSCredential{Host: host, Username: username, Token: token}, nil
}

func gitSSHCredentialFromGrant(
	grant executions.CredentialGrantDescriptor,
	resolved ResolvedWorkspaceCredential,
) (GitSSHCredential, error) {
	if err := validateResolvedGrant(grant, resolved, "git_fetch", "git", "git", "ssh_key"); err != nil {
		return GitSSHCredential{}, errors.New("resolved Git SSH Credential Grant metadata is invalid")
	}
	if !payloadHasOnlyKeys(
		resolved.Payload, "host", "port", "username", "privateKey", "privateKeyPassphrase", "hostKey",
	) || len(resolved.Payload) < 5 {
		return GitSSHCredential{}, errors.New("resolved Git SSH Credential payload is invalid")
	}
	host, hostOK := resolved.Payload["host"].(string)
	port, portOK := resolvedPayloadInteger(resolved.Payload["port"])
	username, usernameOK := resolved.Payload["username"].(string)
	privateKey, privateKeyOK := resolved.Payload["privateKey"].(string)
	hostKey, hostKeyOK := resolved.Payload["hostKey"].(string)
	passphrase := ""
	if raw, present := resolved.Payload["privateKeyPassphrase"]; present {
		var passphraseOK bool
		passphrase, passphraseOK = raw.(string)
		if !passphraseOK || !guardableSecret(passphrase) {
			return GitSSHCredential{}, errors.New("resolved Git SSH Credential payload is invalid")
		}
	}
	host, hostErr := gitpolicy.NormalizeHostname(host)
	username = strings.TrimSpace(username)
	if !hostOK || !portOK || !usernameOK || !privateKeyOK || !hostKeyOK || hostErr != nil ||
		port < 1 || port > 65535 || !safeCredentialUsername(username) ||
		len(privateKey) < 8 || len(privateKey) > 64<<10 || !validResolvedSSHKey(privateKey, passphrase, hostKey) {
		return GitSSHCredential{}, errors.New("resolved Git SSH Credential payload is invalid")
	}
	return GitSSHCredential{
		Host: host, Port: port, Username: username, PrivateKey: privateKey,
		PrivateKeyPassphrase: passphrase, HostKey: strings.TrimSpace(hostKey),
	}, nil
}

func workspaceGitCredentialFromGrant(
	grant executions.CredentialGrantDescriptor,
	resolved ResolvedWorkspaceCredential,
) (*WorkspaceGitCredential, error) {
	switch grant.CredentialType {
	case "https_token":
		credential, err := gitHTTPSCredentialFromGrant(grant, resolved)
		if err != nil {
			return nil, err
		}
		return &WorkspaceGitCredential{HTTPS: &credential}, nil
	case "ssh_key":
		credential, err := gitSSHCredentialFromGrant(grant, resolved)
		if err != nil {
			return nil, err
		}
		return &WorkspaceGitCredential{SSH: &credential}, nil
	default:
		return nil, errors.New("resolved Git Credential type is unsupported")
	}
}

func clearWorkspaceGitCredential(credential *WorkspaceGitCredential) {
	if credential == nil {
		return
	}
	clearGitHTTPSCredential(credential.HTTPS)
	clearGitSSHCredential(credential.SSH)
	credential.HTTPS = nil
	credential.SSH = nil
}

func registryCredentialFromGrant(
	grant executions.CredentialGrantDescriptor,
	resolved ResolvedWorkspaceCredential,
) (RegistryCredential, error) {
	if grant.BindingKind != "registry_pull" && grant.BindingKind != "registry_push" {
		return RegistryCredential{}, errors.New("resolved Registry Credential Grant stage is invalid")
	}
	if err := validateResolvedGrant(
		grant, resolved, grant.BindingKind, "registry", "oci", grant.CredentialType,
	); err != nil {
		return RegistryCredential{}, errors.New("resolved Registry Credential Grant metadata is invalid")
	}
	credential := RegistryCredential{CredentialType: resolved.CredentialType}
	switch resolved.CredentialType {
	case "basic":
		if !payloadHasOnlyKeys(resolved.Payload, "host", "username", "password") {
			return RegistryCredential{}, errors.New("resolved Registry Credential payload is invalid")
		}
		host, hostOK := resolved.Payload["host"].(string)
		username, usernameOK := resolved.Payload["username"].(string)
		password, passwordOK := resolved.Payload["password"].(string)
		host, hostErr := gitpolicy.NormalizeHostname(host)
		username = strings.TrimSpace(username)
		if !hostOK || !usernameOK || !passwordOK || hostErr != nil || !publicWorkspaceCredentialHost(host) ||
			host != resolved.Selector ||
			!safeCredentialUsername(username) || !guardableSecret(password) {
			return RegistryCredential{}, errors.New("resolved Registry Credential payload is invalid")
		}
		credential.Host, credential.Username, credential.Password = host, username, password
	case "bearer_token":
		if !payloadHasOnlyKeys(resolved.Payload, "host", "token") {
			return RegistryCredential{}, errors.New("resolved Registry Credential payload is invalid")
		}
		host, hostOK := resolved.Payload["host"].(string)
		token, tokenOK := resolved.Payload["token"].(string)
		host, hostErr := gitpolicy.NormalizeHostname(host)
		if !hostOK || !tokenOK || hostErr != nil || !publicWorkspaceCredentialHost(host) ||
			host != resolved.Selector || !guardableSecret(token) {
			return RegistryCredential{}, errors.New("resolved Registry Credential payload is invalid")
		}
		credential.Host, credential.Token = host, token
	default:
		return RegistryCredential{}, errors.New("resolved Registry Credential type is unsupported")
	}
	return credential, nil
}

func packageCredentialFromGrant(
	grant executions.CredentialGrantDescriptor,
	resolved ResolvedWorkspaceCredential,
) (PackageCredential, error) {
	if grant.BindingKind != "package_read" && grant.BindingKind != "package_publish" {
		return PackageCredential{}, errors.New("resolved Package Credential Grant stage is invalid")
	}
	if err := validateResolvedGrant(
		grant, resolved, grant.BindingKind, "package", grant.Provider, grant.CredentialType,
	); err != nil {
		return PackageCredential{}, errors.New("resolved Package Credential Grant metadata is invalid")
	}
	credential := PackageCredential{Provider: resolved.Provider, CredentialType: resolved.CredentialType}
	switch {
	case resolved.Provider == "npm" && resolved.CredentialType == "npm_token":
		if !payloadHasOnlyKeys(resolved.Payload, "registryUrl", "token", "scopes") || len(resolved.Payload) < 2 {
			return PackageCredential{}, errors.New("resolved npm Credential payload is invalid")
		}
		registryURL, urlOK := resolved.Payload["registryUrl"].(string)
		token, tokenOK := resolved.Payload["token"].(string)
		if !urlOK || !tokenOK || !safeHTTPSCredentialURL(registryURL) ||
			registryURL != resolved.Selector || !guardableSecret(token) {
			return PackageCredential{}, errors.New("resolved npm Credential payload is invalid")
		}
		credential.RegistryURL, credential.Token = registryURL, token
		if rawScopes, present := resolved.Payload["scopes"]; present {
			scopes, ok := resolvedStringSlice(rawScopes)
			if !ok {
				return PackageCredential{}, errors.New("resolved npm Credential payload is invalid")
			}
			credential.Scopes = scopes
		}
	case resolved.Provider == "pypi" && resolved.CredentialType == "pypi_token":
		if !payloadHasOnlyKeys(resolved.Payload, "indexUrl", "username", "token") {
			return PackageCredential{}, errors.New("resolved PyPI Credential payload is invalid")
		}
		indexURL, urlOK := resolved.Payload["indexUrl"].(string)
		username, usernameOK := resolved.Payload["username"].(string)
		token, tokenOK := resolved.Payload["token"].(string)
		username = strings.TrimSpace(username)
		if !urlOK || !usernameOK || !tokenOK || !safeHTTPSCredentialURL(indexURL) ||
			indexURL != resolved.Selector || !safeCredentialUsername(username) || !guardableSecret(token) {
			return PackageCredential{}, errors.New("resolved PyPI Credential payload is invalid")
		}
		credential.IndexURL, credential.Username, credential.Token = indexURL, username, token
	default:
		return PackageCredential{}, errors.New("resolved Package Credential type is unsupported")
	}
	return credential, nil
}

func validateResolvedGrant(
	grant executions.CredentialGrantDescriptor,
	resolved ResolvedWorkspaceCredential,
	bindingKind, purpose, provider, credentialType string,
) error {
	if grant.GrantID != resolved.GrantID || grant.BindingKind != bindingKind ||
		grant.Purpose != purpose || grant.Provider != provider || grant.CredentialType != credentialType ||
		resolved.BindingKind != grant.BindingKind || resolved.Purpose != grant.Purpose ||
		resolved.Provider != grant.Provider || resolved.CredentialType != grant.CredentialType ||
		resolved.Selector == "" || resolved.Selector != grant.Selector {
		return errors.New("resolved Credential Grant metadata is invalid")
	}
	return nil
}

func payloadHasOnlyKeys(payload map[string]any, keys ...string) bool {
	if len(payload) == 0 || len(payload) > len(keys) {
		return false
	}
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	for key := range payload {
		if _, found := allowed[key]; !found {
			return false
		}
	}
	return true
}

func resolvedPayloadInteger(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		if int64(int(typed)) != typed {
			return 0, false
		}
		return int(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed ||
			typed < float64(math.MinInt) || typed > float64(math.MaxInt) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		value, err := typed.Int64()
		if err != nil || int64(int(value)) != value {
			return 0, false
		}
		return int(value), true
	default:
		return 0, false
	}
}

func resolvedStringSlice(value any) ([]string, bool) {
	var values []any
	switch typed := value.(type) {
	case []any:
		values = typed
	case []string:
		result := append([]string(nil), typed...)
		return validateResolvedScopes(result)
	default:
		return nil, false
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		item, ok := raw.(string)
		if !ok {
			return nil, false
		}
		result = append(result, item)
	}
	return validateResolvedScopes(result)
}

func validateResolvedScopes(scopes []string) ([]string, bool) {
	if len(scopes) == 0 || len(scopes) > 64 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(scopes))
	for index, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if len(scope) < 2 || len(scope) > 214 || scope[0] != '@' {
			return nil, false
		}
		for _, character := range scope[1:] {
			if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
				character == '-' || character == '_' || character == '.' {
				continue
			}
			return nil, false
		}
		if _, duplicate := seen[scope]; duplicate {
			continue
		}
		seen[scope] = struct{}{}
		scopes[index] = scope
	}
	return scopes, true
}

func safeHTTPSCredentialURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	host, hostErr := gitpolicy.NormalizeHostname(parsed.Hostname())
	return hostErr == nil && publicWorkspaceCredentialHost(host)
}

func publicWorkspaceCredentialHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip == nil || (!ip.IsPrivate() && !ip.IsLoopback() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast())
}

func safeCredentialUsername(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func guardableSecret(value string) bool {
	return len(value) >= 8 && len(value) <= 16<<10 && strings.TrimSpace(value) == value && !containsControlString(value)
}

func containsControlString(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validResolvedSSHKey(privateKey, passphrase, hostKey string) bool {
	privateBytes := []byte(privateKey)
	passphraseBytes := []byte(passphrase)
	defer zeroBytes(privateBytes)
	defer zeroBytes(passphraseBytes)
	var privateErr error
	if passphrase == "" {
		_, privateErr = ssh.ParseRawPrivateKey(privateBytes)
	} else {
		_, privateErr = ssh.ParseRawPrivateKeyWithPassphrase(privateBytes, passphraseBytes)
	}
	if privateErr != nil {
		return false
	}
	parsedHostKey, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(hostKey))
	return err == nil && parsedHostKey != nil && strings.TrimSpace(comment) == "" && len(options) == 0 &&
		len(strings.TrimSpace(string(rest))) == 0
}

func clearResolvedWorkspaceCredential(resolved *ResolvedWorkspaceCredential) {
	if resolved == nil {
		return
	}
	for key, raw := range resolved.Payload {
		if value, ok := raw.(string); ok {
			resolved.Payload[key] = strings.Repeat("\x00", len(value))
		}
		delete(resolved.Payload, key)
	}
	resolved.Payload = nil
}

func clearGitSSHCredential(credential *GitSSHCredential) {
	if credential == nil {
		return
	}
	credential.PrivateKey = strings.Repeat("\x00", len(credential.PrivateKey))
	credential.PrivateKeyPassphrase = strings.Repeat("\x00", len(credential.PrivateKeyPassphrase))
	*credential = GitSSHCredential{}
}

func clearGitHTTPSCredential(credential *GitHTTPSCredential) {
	if credential == nil {
		return
	}
	credential.Host = ""
	credential.Username = ""
	credential.Token = ""
}

func clearRegistryCredential(credential *RegistryCredential) {
	if credential == nil {
		return
	}
	credential.Password = strings.Repeat("\x00", len(credential.Password))
	credential.Token = strings.Repeat("\x00", len(credential.Token))
	*credential = RegistryCredential{}
}

func clearPackageCredential(credential *PackageCredential) {
	if credential == nil {
		return
	}
	credential.Token = strings.Repeat("\x00", len(credential.Token))
	for index := range credential.Scopes {
		credential.Scopes[index] = ""
	}
	*credential = PackageCredential{}
}

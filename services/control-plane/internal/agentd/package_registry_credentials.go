package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const (
	providerNPMConfigAlias = "SYNARA_PROVIDER_NPM_CONFIG_USERCONFIG"
	providerPIPConfigAlias = "SYNARA_PROVIDER_PIP_CONFIG_FILE"
)

func resolvePackageCredentialEnvironment(
	ctx context.Context,
	client *Client,
	executionID uuid.UUID,
	lease executions.Lease,
	grants []executions.CredentialGrantDescriptor,
	guard *executionSecretGuard,
	root string,
) (map[string]string, error) {
	selected := map[string]PackageCredential{}
	defer func() {
		for provider, credential := range selected {
			clearPackageCredential(&credential)
			delete(selected, provider)
		}
	}()
	for _, grant := range grants {
		if grant.BindingKind != "package_read" {
			continue
		}
		if err := validateCredentialGrantDescriptor(grant); err != nil {
			return nil, err
		}
		if _, duplicate := selected[grant.Provider]; duplicate {
			return nil, fmt.Errorf("multiple active %s package_read Credential Grants are ambiguous", grant.Provider)
		}
		resolved, err := client.ResolveCredentialGrant(ctx, executionID, grant.GrantID, lease)
		if err != nil {
			return nil, err
		}
		credential, convertErr := packageCredentialFromGrant(grant, resolved)
		clearResolvedWorkspaceCredential(&resolved)
		if convertErr != nil {
			return nil, convertErr
		}
		if guard != nil {
			if err := guard.AddPackageCredential(&credential); err != nil {
				clearPackageCredential(&credential)
				return nil, err
			}
		}
		selected[grant.Provider] = credential
	}
	if len(selected) == 0 {
		return nil, nil
	}
	directory := filepath.Join(root, ".synara-package-config")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create package Credential directory: %w", err)
	}
	environment := map[string]string{}
	if credential, found := selected["npm"]; found {
		path := filepath.Join(directory, "npmrc")
		contents, err := npmCredentialConfig(credential)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return nil, fmt.Errorf("write npm Credential config: %w", err)
		}
		environment[providerNPMConfigAlias] = path
		clearPackageCredential(&credential)
	}
	if credential, found := selected["pypi"]; found {
		path := filepath.Join(directory, "pip.conf")
		contents, err := pipCredentialConfig(credential)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return nil, fmt.Errorf("write PyPI Credential config: %w", err)
		}
		environment[providerPIPConfigAlias] = path
		clearPackageCredential(&credential)
	}
	return environment, nil
}

func npmCredentialConfig(credential PackageCredential) (string, error) {
	if credential.Provider != "npm" || credential.RegistryURL == "" || credential.Token == "" {
		return "", errors.New("npm package Credential is invalid")
	}
	parsed, err := url.Parse(credential.RegistryURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("npm package registry URL is invalid")
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/") + "/"
	authKey := "//" + parsed.Host + path + ":_authToken"
	var builder strings.Builder
	builder.WriteString("registry=")
	builder.WriteString(credential.RegistryURL)
	builder.WriteString("\nalways-auth=true\n")
	builder.WriteString(authKey)
	builder.WriteByte('=')
	builder.WriteString(credential.Token)
	builder.WriteByte('\n')
	for _, scope := range credential.Scopes {
		builder.WriteString(scope)
		builder.WriteString(":registry=")
		builder.WriteString(credential.RegistryURL)
		builder.WriteByte('\n')
	}
	return builder.String(), nil
}

func pipCredentialConfig(credential PackageCredential) (string, error) {
	if credential.Provider != "pypi" || credential.IndexURL == "" || credential.Username == "" || credential.Token == "" {
		return "", errors.New("PyPI package Credential is invalid")
	}
	parsed, err := url.Parse(credential.IndexURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("PyPI package index URL is invalid")
	}
	parsed.User = url.UserPassword(credential.Username, credential.Token)
	return "[global]\nindex-url = " + parsed.String() + "\ndisable-pip-version-check = true\n", nil
}

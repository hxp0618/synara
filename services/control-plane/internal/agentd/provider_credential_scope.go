package agentd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const providerGitConfigMaximumBytes = 64 << 10

var providerAmbientCredentialEnvironmentNames = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AZURE_CLIENT_SECRET",
	"AZURE_FEDERATED_TOKEN_FILE",
	"DOCKER_AUTH_CONFIG",
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GITLAB_TOKEN",
	"GIT_ASKPASS",
	"GIT_SSH",
	"GIT_SSH_COMMAND",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"NODE_AUTH_TOKEN",
	"NPM_TOKEN",
	"PYPI_TOKEN",
	"SSH_ASKPASS",
	"SSH_AUTH_SOCK",
	"YARN_NPM_AUTH_TOKEN",
}

var providerAmbientCredentialPaths = []string{
	".aws/credentials",
	".azure/accessTokens.json",
	".config/gcloud/application_default_credentials.json",
	".config/gcloud/credentials.db",
	".config/gh/hosts.yml",
	".config/glab-cli/config.yml",
	".docker/config.json",
	".kube/config",
	".netrc",
	".npmrc",
	".pypirc",
	".ssh/id_ed25519",
	".ssh/id_rsa",
	"/var/run/secrets/kubernetes.io/serviceaccount/token",
}

var providerGitConfigUserInfoPattern = regexp.MustCompile(`(?i)https?://[^\s/]+@`)

// RunProviderCredentialScopeVerifier exposes a bounded, value-free runtime
// self-test that can be invoked from the exact Provider process boundary.
func RunProviderCredentialScopeVerifier(args []string) (bool, error) {
	if len(args) != 2 || args[1] != platform.ProviderCredentialScopeVerifyArgument {
		return false, nil
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		home = "/nonexistent"
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return true, errors.New("Provider credential scope could not resolve the workspace")
	}
	return true, verifyProviderCredentialScope(home, workingDirectory, os.LookupEnv)
}

func verifyProviderCredentialScope(
	home string,
	workingDirectory string,
	lookupEnvironment func(string) (string, bool),
) error {
	if strings.TrimSpace(home) == "" || strings.TrimSpace(workingDirectory) == "" || lookupEnvironment == nil {
		return errors.New("invalid Provider credential scope probe")
	}
	for _, name := range providerAmbientCredentialEnvironmentNames {
		if _, present := lookupEnvironment(name); present {
			return errors.New("ambient Provider credential material is present")
		}
	}
	for _, configuredPath := range providerAmbientCredentialPaths {
		path := configuredPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(home, path)
		}
		if _, err := os.Stat(path); err == nil {
			return errors.New("ambient Provider credential material is present")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("ambient Provider credential path could not be verified")
		}
	}
	gitConfiguration, err := readProviderGitConfiguration(filepath.Join(workingDirectory, ".git", "config"))
	if err != nil {
		return err
	}
	if providerGitConfigUserInfoPattern.Match(gitConfiguration) {
		return errors.New("ambient Provider credential material is present")
	}
	return nil
}

func readProviderGitConfiguration(path string) ([]byte, error) {
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("workspace Git configuration could not be verified")
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = syscall.Close(descriptor)
		return nil, errors.New("workspace Git configuration could not be verified")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > providerGitConfigMaximumBytes {
		return nil, errors.New("workspace Git configuration could not be verified")
	}
	content, err := io.ReadAll(io.LimitReader(file, providerGitConfigMaximumBytes+1))
	if err != nil || len(content) > providerGitConfigMaximumBytes {
		return nil, errors.New("workspace Git configuration could not be verified")
	}
	return content, nil
}

package agentd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageCredentialConfigsUseControlledFiles(t *testing.T) {
	npm, err := npmCredentialConfig(PackageCredential{
		Provider: "npm", CredentialType: "npm_token",
		RegistryURL: "https://packages.corp.example/npm/", Token: "npm-secret-token",
		Scopes: []string{"@synara", "@platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"registry=https://packages.corp.example/npm/",
		"//packages.corp.example/npm/:_authToken=npm-secret-token",
		"@synara:registry=https://packages.corp.example/npm/",
	} {
		if !strings.Contains(npm, expected) {
			t.Fatalf("npm config omitted %q:\n%s", expected, npm)
		}
	}
	pip, err := pipCredentialConfig(PackageCredential{
		Provider: "pypi", CredentialType: "pypi_token",
		IndexURL: "https://packages.corp.example/simple/", Username: "__token__", Token: "pypi-secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pip, "https://__token__:pypi-secret-token@packages.corp.example/simple/") {
		t.Fatalf("PyPI config did not contain the exact authenticated index: %s", pip)
	}
}

func TestProviderHostExecutionEnvironmentAcceptsOnlyControlledAbsoluteConfigPaths(t *testing.T) {
	npmPath := filepath.Join(t.TempDir(), "npmrc")
	environment, err := providerHostEnvironmentForExecution([]string{
		providerNPMConfigAlias + "=/tmp/ambient-npmrc",
		providerPIPConfigAlias + "=/tmp/ambient-pip.conf",
	}, nil, map[string]string{
		providerNPMConfigAlias: npmPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(environment, providerNPMConfigAlias+"="+npmPath) {
		t.Fatalf("Provider Host environment omitted controlled npm config: %#v", environment)
	}
	if containsString(environment, providerPIPConfigAlias+"=/tmp/ambient-pip.conf") {
		t.Fatalf("Provider Host inherited an ambient package Credential path: %#v", environment)
	}
	for _, invalid := range []map[string]string{
		{"NPM_CONFIG_USERCONFIG": npmPath},
		{providerNPMConfigAlias: "relative/npmrc"},
		{providerPIPConfigAlias: "/tmp/pip.conf\nINJECTED=1"},
	} {
		if _, err := providerHostEnvironmentForExecution(nil, nil, invalid); err == nil {
			t.Fatalf("unsupported Provider execution environment was accepted: %#v", invalid)
		}
	}
}

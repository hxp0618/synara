package credentials

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestNormalizeGitSSHPayloadRequiresOneStrongKeyAndPinnedHostKey(t *testing.T) {
	privateKey, publicKey := testSSHCredentialKey(t, 0)
	normalized, encoded, err := normalizeCredentialPayload(
		PurposeGit,
		GitProvider,
		GitSSHCredentialType,
		map[string]any{
			"host": "GIT.EXAMPLE.COM.", "port": float64(22), "username": "git",
			"privateKey": privateKey, "hostKey": publicKey,
		},
	)
	if err != nil || len(encoded) == 0 {
		t.Fatalf("valid Git SSH payload was rejected: %v", err)
	}
	if normalized["host"] != "git.example.com" || normalized["port"] != 22 || normalized["username"] != "git" {
		t.Fatalf("Git SSH public metadata was not normalized: %#v", normalized)
	}

	for name, mutate := range map[string]func(map[string]any){
		"unknown field":    func(value map[string]any) { value["command"] = "forbidden" },
		"missing host key": func(value map[string]any) { delete(value, "hostKey") },
		"invalid port":     func(value map[string]any) { value["port"] = float64(0) },
		"host key comment": func(value map[string]any) { value["hostKey"] = publicKey + " comment" },
	} {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{
				"host": "git.example.com", "port": float64(22), "username": "git",
				"privateKey": privateKey, "hostKey": publicKey,
			}
			mutate(payload)
			if _, _, err := normalizeCredentialPayload(PurposeGit, GitProvider, GitSSHCredentialType, payload); err == nil {
				t.Fatal("invalid Git SSH payload was accepted")
			}
		})
	}

	weakPrivateKey, _ := testSSHCredentialKey(t, 1024)
	if _, _, err := normalizeCredentialPayload(
		PurposeGit,
		GitProvider,
		GitSSHCredentialType,
		map[string]any{
			"host": "git.example.com", "port": 22, "username": "git",
			"privateKey": weakPrivateKey, "hostKey": publicKey,
		},
	); err == nil {
		t.Fatal("weak RSA private key was accepted")
	}
}

func TestNormalizeRegistryCredentialPayloads(t *testing.T) {
	basic, _, err := normalizeCredentialPayload(
		PurposeRegistry,
		RegistryProviderOci,
		RegistryBasicCredentialType,
		map[string]any{"host": "REGISTRY.EXAMPLE.COM.", "username": "robot", "password": "registry-secret"},
	)
	if err != nil || basic["host"] != "registry.example.com" {
		t.Fatalf("valid Registry basic payload was rejected: %v", err)
	}
	bearer, _, err := normalizeCredentialPayload(
		PurposeRegistry,
		RegistryProviderOci,
		RegistryBearerCredentialType,
		map[string]any{"host": "registry.example.com", "token": "registry-bearer-secret"},
	)
	if err != nil || bearer["token"] != "registry-bearer-secret" {
		t.Fatalf("valid Registry bearer payload was rejected: %v", err)
	}

	invalid := []map[string]any{
		{"host": "127.0.0.1", "token": "registry-bearer-secret"},
		{"host": "registry.example.com", "token": "short"},
		{"host": "registry.example.com", "token": "registry-bearer-secret", "env": "forbidden"},
	}
	for _, payload := range invalid {
		if _, _, err := normalizeCredentialPayload(PurposeRegistry, RegistryProviderOci, RegistryBearerCredentialType, payload); err == nil {
			t.Fatal("invalid Registry payload was accepted")
		}
	}
}

func TestNormalizePackageCredentialPayloads(t *testing.T) {
	npm, _, err := normalizeCredentialPayload(
		PurposePackage,
		PackageProviderNPM,
		PackageNPMTokenCredentialType,
		map[string]any{
			"registryUrl": "https://registry.npmjs.org/", "token": "npm-package-secret",
			"scopes": []any{"@Synara", "@shared", "@synara"},
		},
	)
	if err != nil {
		t.Fatalf("valid npm payload was rejected: %v", err)
	}
	scopes, ok := npm["scopes"].([]string)
	if !ok || len(scopes) != 2 || scopes[0] != "@synara" || scopes[1] != "@shared" {
		t.Fatalf("npm scopes were not normalized and deduplicated: %#v", npm["scopes"])
	}
	pypi, _, err := normalizeCredentialPayload(
		PurposePackage,
		PackageProviderPyPI,
		PackagePyPITokenCredentialType,
		map[string]any{
			"indexUrl": "https://pypi.org/simple/", "username": "__token__", "token": "pypi-package-secret",
		},
	)
	if err != nil || pypi["indexUrl"] != "https://pypi.org/simple/" {
		t.Fatalf("valid PyPI payload was rejected: %v", err)
	}

	for _, registryURL := range []string{
		"http://registry.npmjs.org/",
		"https://token@registry.npmjs.org/",
		"https://registry.npmjs.org/?token=forbidden",
		"https://127.0.0.1/",
	} {
		if _, _, err := normalizeCredentialPayload(
			PurposePackage,
			PackageProviderNPM,
			PackageNPMTokenCredentialType,
			map[string]any{"registryUrl": registryURL, "token": "npm-package-secret"},
		); err == nil {
			t.Fatal("unsafe Package registry URL was accepted")
		}
	}
}

func TestWorkspaceCredentialPayloadIdentityIsStableAcrossSecretRotation(t *testing.T) {
	current := map[string]any{"host": "registry.example.com", "token": "registry-secret-one"}
	next := map[string]any{"host": "registry.example.com", "token": "registry-secret-two"}
	currentIdentity, err := credentialPayloadIdentity(
		PurposeRegistry, RegistryProviderOci, RegistryBearerCredentialType, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	nextIdentity, err := credentialPayloadIdentity(
		PurposeRegistry, RegistryProviderOci, RegistryBearerCredentialType, next,
	)
	if err != nil || nextIdentity != currentIdentity {
		t.Fatalf("secret rotation changed the Registry selector identity: %v", err)
	}
}

func testSSHCredentialKey(t *testing.T, rsaBits int) (string, string) {
	t.Helper()
	var private any
	var err error
	if rsaBits > 0 {
		private, err = rsa.GenerateKey(rand.Reader, rsaBits)
	} else {
		_, private, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

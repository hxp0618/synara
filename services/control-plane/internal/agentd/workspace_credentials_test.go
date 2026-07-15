package agentd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestCredentialGrantForStageFailsClosedOnAmbiguity(t *testing.T) {
	first := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "git_fetch", Purpose: "git",
	}
	selected, err := credentialGrantForStage(
		[]executions.CredentialGrantDescriptor{first, {
			GrantID: uuid.New(), BindingKind: "registry_pull", Purpose: "registry",
		}},
		"git_fetch",
	)
	if err != nil || selected == nil || selected.GrantID != first.GrantID {
		t.Fatalf("select one stage Grant: selected=%#v err=%v", selected, err)
	}
	_, err = credentialGrantForStage(
		[]executions.CredentialGrantDescriptor{first, {
			GrantID: uuid.New(), BindingKind: "git_fetch", Purpose: "git",
		}},
		"git_fetch",
	)
	if err == nil {
		t.Fatal("ambiguous stage Credential Grants were accepted")
	}
}

func TestGitHTTPSCredentialFromGrantRequiresExactMetadataAndAllowlist(t *testing.T) {
	grant := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "git_fetch", Purpose: "git", Provider: "git",
		CredentialType: "https_token", Selector: "https://git.example.com/team/repository.git",
	}
	resolved := ResolvedWorkspaceCredential{
		GrantID: grant.GrantID, BindingKind: grant.BindingKind, Purpose: "git", Provider: "git",
		CredentialType: "https_token", Selector: grant.Selector,
		Payload: map[string]any{
			"host": "git.example.com", "username": "synara", "token": "git-secret-token",
		},
	}
	credential, err := gitHTTPSCredentialFromGrant(grant, resolved)
	if err != nil || credential.Token != "git-secret-token" {
		t.Fatalf("decode Git HTTPS Grant: credential=%#v err=%v", credential, err)
	}
	resolved.Payload["extra"] = "forbidden"
	if _, err := gitHTTPSCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("Git HTTPS Grant accepted an unknown payload field")
	}
}

func TestResolvedWorkspaceCredentialPayloadIsCleared(t *testing.T) {
	resolved := ResolvedWorkspaceCredential{Payload: map[string]any{
		"host": "git.example.com", "token": "token-secret", "privateKey": "private-key-secret",
		"privateKeyPassphrase": "passphrase-secret", "hostKey": "public-host-key",
	}}
	clearResolvedWorkspaceCredential(&resolved)
	if resolved.Payload != nil {
		t.Fatalf("Workspace Credential payload was retained: %#v", resolved.Payload)
	}
}

func TestGitSSHCredentialFromGrantRequiresPinnedKeyAndExactPayload(t *testing.T) {
	privateKey, hostKey := testGitSSHKeyPair(t, "")
	grant := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "git_fetch", Purpose: "git", Provider: "git",
		CredentialType: "ssh_key", Selector: "ssh://git@git.example.com/team/repository.git",
	}
	resolved := ResolvedWorkspaceCredential{
		GrantID: grant.GrantID, BindingKind: grant.BindingKind, Purpose: grant.Purpose,
		Provider: grant.Provider, CredentialType: grant.CredentialType, Selector: grant.Selector,
		Payload: map[string]any{
			"host": "git.example.com", "port": float64(22), "username": "git",
			"privateKey": privateKey, "hostKey": hostKey,
		},
	}
	credential, err := gitSSHCredentialFromGrant(grant, resolved)
	if err != nil || credential.Host != "git.example.com" || credential.Port != 22 ||
		credential.Username != "git" || credential.PrivateKey == "" || credential.HostKey != strings.TrimSpace(hostKey) {
		t.Fatalf("decode Git SSH Grant: credential=%#v err=%v", credential, err)
	}
	clearGitSSHCredential(&credential)
	if credential != (GitSSHCredential{}) {
		t.Fatalf("Git SSH Credential was retained: %#v", credential)
	}
	resolved.Payload["unknown"] = "forbidden"
	if _, err := gitSSHCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("Git SSH Grant accepted an unknown payload field")
	}
	delete(resolved.Payload, "unknown")
	resolved.Payload["hostKey"] = "ssh-ed25519 invalid"
	if _, err := gitSSHCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("Git SSH Grant accepted an invalid pinned Host Key")
	}
}

func TestWorkspaceGitCredentialFromGrantCreatesExactlyOneTransport(t *testing.T) {
	grant := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "git_fetch", Purpose: "git", Provider: "git",
		CredentialType: "https_token", Selector: "https://git.example.com/team/repository.git",
	}
	resolved := ResolvedWorkspaceCredential{
		GrantID: grant.GrantID, BindingKind: grant.BindingKind, Purpose: grant.Purpose,
		Provider: grant.Provider, CredentialType: grant.CredentialType, Selector: grant.Selector,
		Payload: map[string]any{
			"host": "git.example.com", "username": "synara", "token": "git-secret-token",
		},
	}
	credential, err := workspaceGitCredentialFromGrant(grant, resolved)
	if err != nil || credential == nil || credential.HTTPS == nil || credential.SSH != nil {
		t.Fatalf("decode Workspace Git Credential: credential=%#v err=%v", credential, err)
	}
	clearWorkspaceGitCredential(credential)
	if credential.HTTPS != nil || credential.SSH != nil {
		t.Fatalf("Workspace Git Credential survived cleanup: %#v", credential)
	}
}

func TestRegistryCredentialFromGrantIsStageBound(t *testing.T) {
	grant := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "registry_pull", Purpose: "registry", Provider: "oci",
		CredentialType: "basic", Selector: "registry.example.com",
	}
	resolved := ResolvedWorkspaceCredential{
		GrantID: grant.GrantID, BindingKind: grant.BindingKind, Purpose: grant.Purpose,
		Provider: grant.Provider, CredentialType: grant.CredentialType, Selector: grant.Selector,
		Payload: map[string]any{
			"host": "registry.example.com", "username": "robot", "password": "registry-secret",
		},
	}
	credential, err := registryCredentialFromGrant(grant, resolved)
	if err != nil || credential.Host != grant.Selector || credential.Password != "registry-secret" {
		t.Fatalf("decode Registry Grant: credential=%#v err=%v", credential, err)
	}
	clearRegistryCredential(&credential)
	if credential != (RegistryCredential{}) {
		t.Fatalf("Registry Credential was retained: %#v", credential)
	}
	grant.BindingKind = "registry_push"
	if _, err := registryCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("Registry pull Grant was reused for the push stage")
	}
	grant.BindingKind = "registry_pull"
	resolved.Payload["host"] = "127.0.0.1"
	resolved.Selector = "127.0.0.1"
	grant.Selector = "127.0.0.1"
	if _, err := registryCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("Registry Grant accepted a loopback endpoint")
	}
}

func TestPackageCredentialFromGrantUsesExactRegistrySelector(t *testing.T) {
	grant := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "package_read", Purpose: "package", Provider: "npm",
		CredentialType: "npm_token", Selector: "https://registry.example.com/npm/",
	}
	resolved := ResolvedWorkspaceCredential{
		GrantID: grant.GrantID, BindingKind: grant.BindingKind, Purpose: grant.Purpose,
		Provider: grant.Provider, CredentialType: grant.CredentialType, Selector: grant.Selector,
		Payload: map[string]any{
			"registryUrl": grant.Selector, "token": "package-secret", "scopes": []any{"@synara"},
		},
	}
	credential, err := packageCredentialFromGrant(grant, resolved)
	if err != nil || credential.RegistryURL != grant.Selector || credential.Token != "package-secret" ||
		len(credential.Scopes) != 1 || credential.Scopes[0] != "@synara" {
		t.Fatalf("decode npm Grant: credential=%#v err=%v", credential, err)
	}
	clearPackageCredential(&credential)
	if credential.Token != "" || credential.RegistryURL != "" || len(credential.Scopes) != 0 {
		t.Fatalf("Package Credential was retained: %#v", credential)
	}
	resolved.Payload["registryUrl"] = "https://other.example.com/npm/"
	if _, err := packageCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("Package Grant accepted a payload that did not match its immutable selector")
	}
}

func TestPyPICredentialFromGrantRejectsUnknownFields(t *testing.T) {
	grant := executions.CredentialGrantDescriptor{
		GrantID: uuid.New(), BindingKind: "package_publish", Purpose: "package", Provider: "pypi",
		CredentialType: "pypi_token", Selector: "https://packages.example.com/simple/",
	}
	resolved := ResolvedWorkspaceCredential{
		GrantID: grant.GrantID, BindingKind: grant.BindingKind, Purpose: grant.Purpose,
		Provider: grant.Provider, CredentialType: grant.CredentialType, Selector: grant.Selector,
		Payload: map[string]any{
			"indexUrl": grant.Selector, "username": "__token__", "token": "pypi-secret-token",
		},
	}
	credential, err := packageCredentialFromGrant(grant, resolved)
	if err != nil || credential.IndexURL != grant.Selector || credential.Username != "__token__" {
		t.Fatalf("decode PyPI Grant: credential=%#v err=%v", credential, err)
	}
	resolved.Payload["password"] = "forbidden"
	if _, err := packageCredentialFromGrant(grant, resolved); err == nil {
		t.Fatal("PyPI Grant accepted an unknown payload field")
	}
}

func testGitSSHKeyPair(t *testing.T, passphrase string) (string, string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(privateKey, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

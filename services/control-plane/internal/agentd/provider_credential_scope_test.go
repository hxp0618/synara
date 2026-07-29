package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestRunProviderCredentialScopeVerifierDispatchesOnlyExactMode(t *testing.T) {
	handled, err := RunProviderCredentialScopeVerifier([]string{"synara-agentd"})
	if handled || err != nil {
		t.Fatalf("unrelated mode = (%t, %v)", handled, err)
	}
	handled, err = RunProviderCredentialScopeVerifier([]string{
		"synara-agentd", platform.ProviderCredentialScopeVerifyArgument, "extra",
	})
	if handled || err != nil {
		t.Fatalf("extra argument mode = (%t, %v)", handled, err)
	}
}

func TestVerifyProviderCredentialScopeAcceptsEmptyBoundary(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	if err := verifyProviderCredentialScope(home, workspace, func(string) (string, bool) {
		return "", false
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyProviderCredentialScopeRejectsEnvironmentPresenceWithoutReadingValue(t *testing.T) {
	lookedUp := ""
	err := verifyProviderCredentialScope(t.TempDir(), t.TempDir(), func(name string) (string, bool) {
		lookedUp = name
		return "value-that-must-not-be-reported", name == "AWS_ACCESS_KEY_ID"
	})
	if err == nil || lookedUp != "AWS_ACCESS_KEY_ID" || err.Error() != "ambient Provider credential material is present" {
		t.Fatalf("credential environment result = (%q, %v)", lookedUp, err)
	}
}

func TestVerifyProviderCredentialScopeRejectsCredentialPath(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".aws", "credentials")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("must-not-be-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyProviderCredentialScope(home, t.TempDir(), func(string) (string, bool) {
		return "", false
	})
	if err == nil || err.Error() != "ambient Provider credential material is present" {
		t.Fatalf("credential path result = %v", err)
	}
}

func TestVerifyProviderCredentialScopeRejectsGitUserInfoAndUnsafeConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "userinfo", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("url = https://user:secret@example.test/repo.git\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", prepare: func(t *testing.T, path string) {
			target := filepath.Join(filepath.Dir(path), "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, make([]byte, providerGitConfigMaximumBytes+1), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			gitDirectory := filepath.Join(workspace, ".git")
			if err := os.Mkdir(gitDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, filepath.Join(gitDirectory, "config"))
			err := verifyProviderCredentialScope(t.TempDir(), workspace, func(string) (string, bool) {
				return "", false
			})
			if err == nil {
				t.Fatal("unsafe Git configuration was accepted")
			}
		})
	}
}

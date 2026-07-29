package agentd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStageKubernetesRegistrationTokenCreatesRestrictedOneShotFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "projected", "token")
	destination := filepath.Join(root, "staged", "token")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("pod-bound-token\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := stageKubernetesRegistrationToken(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("staged registration token mode = %o, want 600", info.Mode().Perm())
	}
	token, err := readRegularRegistrationTokenFile(destination)
	if err != nil || token != "pod-bound-token" {
		t.Fatalf("staged registration token = %q, %v", token, err)
	}
	if err := removeConsumedRegistrationTokenFile(destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed registration token remains readable: %v", err)
	}
}

func TestRegistrationTokenStagingRejectsSymlinkAndMalformedContent(t *testing.T) {
	root := t.TempDir()
	realToken := filepath.Join(root, "real-token")
	if err := os.WriteFile(realToken, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "token-link")
	if err := os.Symlink(realToken, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularRegistrationTokenFile(symlink); err == nil {
		t.Fatal("registration token symlink was accepted")
	}
	malformed := filepath.Join(root, "malformed")
	if err := os.WriteFile(malformed, []byte("first\nsecond"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularRegistrationTokenFile(malformed); err == nil {
		t.Fatal("multiline registration token was accepted")
	}
}

func TestProjectedKubernetesRegistrationTokenAcceptsAtomicWriterSymlink(t *testing.T) {
	root := t.TempDir()
	versionDirectory := filepath.Join(root, "..2026_07_28_01_02_03")
	if err := os.MkdirAll(versionDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDirectory, "token"), []byte("pod-bound-token\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(versionDirectory), filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	projectedPath := filepath.Join(root, "token")
	if err := os.Symlink(filepath.Join("..data", "token"), projectedPath); err != nil {
		t.Fatal(err)
	}

	token, err := readProjectedKubernetesRegistrationTokenFile(projectedPath)
	if err != nil || token != "pod-bound-token" {
		t.Fatalf("projected Kubernetes registration token = %q, %v", token, err)
	}
	if _, err := readRegularRegistrationTokenFile(projectedPath); err == nil {
		t.Fatal("staged registration token reader accepted an AtomicWriter symlink")
	}
}

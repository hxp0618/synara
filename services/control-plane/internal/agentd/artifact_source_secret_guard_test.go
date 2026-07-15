package agentd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

func TestGuardedArtifactUploadSourceRedactsTextAcrossReadBoundary(t *testing.T) {
	secret := "artifact-secret-123456"
	guard := executionGuardForSecretTest(t, secret)
	prefix := bytes.Repeat([]byte("a"), (64<<10)-len(secret)/2)
	want := append(append(append([]byte(nil), prefix...), secretguard.RedactionMarker...), []byte("-suffix")...)
	payload := append(append(append([]byte(nil), prefix...), secret...), []byte("-suffix")...)

	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openRegularArtifactSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	safeSource, cleanup, err := guardedArtifactUploadSource(
		context.Background(), guard, "text/plain; charset=utf-8", source,
	)
	if err != nil {
		t.Fatal(err)
	}
	stagingPath := safeSource.path
	safePayload, err := io.ReadAll(safeSource.file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(safePayload, want) || bytes.Contains(safePayload, []byte(secret)) {
		t.Fatalf("guarded text Artifact mismatch: got %d bytes, want %d", len(safePayload), len(want))
	}
	cleanup()
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("guarded staging still exists after cleanup: %v", err)
	}
}

func TestGuardedArtifactUploadSourceBlocksBinaryAndRemovesStaging(t *testing.T) {
	secret := "binary-artifact-secret-123456"
	guard := executionGuardForSecretTest(t, secret)
	sourceRoot := t.TempDir()
	stagingRoot := t.TempDir()
	t.Setenv("TMPDIR", stagingRoot)
	payload := append(bytes.Repeat([]byte{0x01}, (64<<10)-len(secret)/2), secret...)
	payload = append(payload, 0x00, 0xff)
	path := filepath.Join(sourceRoot, "report.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openRegularArtifactSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	safeSource, cleanup, err := guardedArtifactUploadSource(
		context.Background(), guard, "application/octet-stream", source,
	)
	if !secretguard.IsExposure(err) || safeSource != nil || cleanup != nil {
		t.Fatalf("binary guarded source = %#v, cleanup=%v, error=%T %v", safeSource, cleanup != nil, err, err)
	}
	entries, readErr := os.ReadDir(stagingRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("blocked binary Artifact retained staging files: %#v", entries)
	}
}

func executionGuardForSecretTest(t *testing.T, secret string) *executionSecretGuard {
	t.Helper()
	guard, err := newExecutionSecretGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.AddProviderCredential(&RunnerCredential{Payload: map[string]any{"apiKey": secret}}); err != nil {
		_ = guard.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close execution SecretGuard: %v", err)
		}
	})
	return guard
}

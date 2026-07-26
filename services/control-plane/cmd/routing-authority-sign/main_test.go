package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

func TestRunSignsCanonicalPublicationAndPrintsPublicKey(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := writeTestPrivateKey(t, privateKey, 0o600)
	now := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	unsigned := routing.PlatformAuthorityPublication{
		SchemaVersion:     routing.PlatformAuthoritySchemaVersionV1,
		PublisherIdentity: "signer-test", KeyID: "key-v1", Nonce: uuid.NewString(),
		IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		ExecutionTargetID: uuid.NewString(), ObservedAt: now.Format(time.RFC3339Nano),
		Health: &routing.PlatformAuthorityHealth{
			Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
			AllocatedCapacityUnits: 1, TTLSeconds: 60,
			ReservationAuthority: &routing.ReservationAuthorityObservation{
				Mode: routing.ReservationAuthorityExactActiveV1,
				Acknowledgements: []routing.ReservationIdentity{{
					ExecutionID: uuid.New(), Generation: 2,
				}},
			},
		},
	}
	encoded, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"--private-key-file", keyPath}, bytes.NewReader(encoded), &output); err != nil {
		t.Fatal(err)
	}
	var signed routing.PlatformAuthorityPublication
	if err := json.Unmarshal(output.Bytes(), &signed); err != nil {
		t.Fatal(err)
	}
	expected, err := routing.SignPlatformAuthorityPublication(privateKey, unsigned)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Signature == "" || signed.Signature != expected.Signature {
		t.Fatalf("signature = %q, want %q", signed.Signature, expected.Signature)
	}

	output.Reset()
	if err := run([]string{"--private-key-file", keyPath, "--public-key-only"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != base64.StdEncoding.EncodeToString(publicKey) {
		t.Fatalf("public key output = %q", output.String())
	}
}

func TestRunRejectsUnknownJSONSignatureAndWritableKeyFile(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secureKeyPath := writeTestPrivateKey(t, privateKey, 0o600)
	for _, input := range []string{
		`{"schemaVersion":1,"unknown":true}`,
		`{"schemaVersion":1,"signature":"already-signed"}`,
	} {
		if err := run([]string{"--private-key-file", secureKeyPath}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid input was accepted: %s", input)
		}
	}

	writableKeyPath := writeTestPrivateKey(t, privateKey, 0o622)
	if err := run([]string{"--private-key-file", writableKeyPath, "--public-key-only"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("group-writable private key was accepted")
	}

	symlinkPath := filepath.Join(t.TempDir(), "projected-key.pem")
	if err := os.Symlink(secureKeyPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--private-key-file", symlinkPath, "--public-key-only"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("private key symlink was accepted without explicit opt-in")
	}
	if err := run([]string{"--private-key-file", symlinkPath, "--allow-key-symlink", "--public-key-only"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("explicit projected-secret symlink opt-in failed: %v", err)
	}
}

func writeTestPrivateKey(t *testing.T, privateKey ed25519.PrivateKey, mode os.FileMode) string {
	t.Helper()
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "publisher-key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

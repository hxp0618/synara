package executiontargets

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

var processContainmentTestSeed = [32]byte{
	1, 2, 3, 4, 5, 6, 7, 8,
	9, 10, 11, 12, 13, 14, 15, 16,
	17, 18, 19, 20, 21, 22, 23, 24,
	25, 26, 27, 28, 29, 30, 31, 32,
}

var alternateProcessContainmentTestSeed = [32]byte{
	32, 31, 30, 29, 28, 27, 26, 25,
	24, 23, 22, 21, 20, 19, 18, 17,
	16, 15, 14, 13, 12, 11, 10, 9,
	8, 7, 6, 5, 4, 3, 2, 1,
}

func processContainmentTestPublicKey() ed25519.PublicKey {
	privateKey := ed25519.NewKeyFromSeed(processContainmentTestSeed[:])
	return ed25519.PublicKey(privateKey[32:])
}

func alternateProcessContainmentTestPublicKey() ed25519.PublicKey {
	privateKey := ed25519.NewKeyFromSeed(alternateProcessContainmentTestSeed[:])
	return ed25519.PublicKey(privateKey[32:])
}

func processContainmentTestPublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(processContainmentTestPublicKey())
}

func processContainmentTestPublicKeyBase64Raw() string {
	return base64.RawStdEncoding.EncodeToString(processContainmentTestPublicKey())
}

func alternateProcessContainmentTestPublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(alternateProcessContainmentTestPublicKey())
}

func TestParseProcessContainmentPolicyDefaultsDisabled(t *testing.T) {
	policy, err := ParseProcessContainmentPolicy(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if policy.TrustMode != ProcessContainmentTrustDisabled {
		t.Fatalf("default trust mode = %q", policy.TrustMode)
	}
}

func TestParseProcessContainmentPolicyParsesSignedV1(t *testing.T) {
	policy, err := ParseProcessContainmentPolicy(map[string]any{
		"processContainmentPolicy": map[string]any{
			"trustMode":        ProcessContainmentTrustSignedV1,
			"keyId":            "test-key",
			"ed25519PublicKey": processContainmentTestPublicKeyBase64(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.TrustMode != ProcessContainmentTrustSignedV1 {
		t.Fatalf("trust mode = %q", policy.TrustMode)
	}
	if policy.KeyID != "test-key" {
		t.Fatalf("key id = %q", policy.KeyID)
	}
	if !bytes.Equal(policy.PublicKey, processContainmentTestPublicKey()) {
		t.Fatalf("public key = %x", []byte(policy.PublicKey))
	}
	digest := sha256.Sum256(processContainmentTestPublicKey())
	if policy.PublicKeySHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("public key digest = %q", policy.PublicKeySHA256)
	}
}

func TestParseProcessContainmentPolicyRejectsInvalidShape(t *testing.T) {
	tests := []struct {
		name         string
		capabilities map[string]any
	}{
		{name: "policy-not-object", capabilities: map[string]any{"processContainmentPolicy": "disabled"}},
		{name: "unknown-field", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"mode": "disabled"}}},
		{name: "missing-trust-mode", capabilities: map[string]any{"processContainmentPolicy": map[string]any{}}},
		{name: "non-string-trust-mode", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": true}}},
		{name: "unsupported-trust-mode", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": "kernel-attested-v9"}}},
		{name: "disabled-extra-field", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustDisabled, "keyId": "extra"}}},
		{name: "signed-missing-key-id", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "ed25519PublicKey": processContainmentTestPublicKeyBase64()}}},
		{name: "signed-missing-public-key", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test-key"}}},
		{name: "key-id-empty", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "", "ed25519PublicKey": processContainmentTestPublicKeyBase64()}}},
		{name: "key-id-space", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test key", "ed25519PublicKey": processContainmentTestPublicKeyBase64()}}},
		{name: "key-id-nonascii", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "测试", "ed25519PublicKey": processContainmentTestPublicKeyBase64()}}},
		{name: "public-key-not-string", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test-key", "ed25519PublicKey": true}}},
		{name: "public-key-invalid-base64", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test-key", "ed25519PublicKey": "!!!"}}},
		{name: "public-key-wrong-size", capabilities: map[string]any{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test-key", "ed25519PublicKey": base64.StdEncoding.EncodeToString([]byte{1, 2, 3})}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseProcessContainmentPolicy(test.capabilities); err == nil {
				t.Fatalf("invalid policy was accepted: %#v", test.capabilities)
			}
		})
	}
}

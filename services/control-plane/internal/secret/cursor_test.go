package secret

import (
	"bytes"
	"testing"
)

func TestCursorCipherV2RoundTripAndBindingIsolation(t *testing.T) {
	cipher, err := NewCursorCipher(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var binding [32]byte
	copy(binding[:], bytes.Repeat([]byte{0x11}, len(binding)))
	envelope, err := cipher.SealV2([]byte("opaque-provider-cursor"), 1, binding)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, []byte("opaque-provider-cursor")) {
		t.Fatal("Cursor plaintext leaked into the v2 envelope")
	}
	plain, status, err := cipher.OpenV2(envelope, 1, binding)
	if err != nil {
		t.Fatal(err)
	}
	if status != CursorOpenValid || string(plain) != "opaque-provider-cursor" {
		t.Fatalf("OpenV2 = %q, %s", plain, status)
	}

	mismatched := binding
	mismatched[0] ^= 0xff
	plain, status, err = cipher.OpenV2(envelope, 1, mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if status != CursorOpenBindingMismatch || plain != nil {
		t.Fatalf("binding mismatch = %q, %s", plain, status)
	}
}

func TestCursorCipherV2WrongKeyAndTamperingRemainAuthenticationFailures(t *testing.T) {
	writer, err := NewCursorCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewCursorCipher(bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var binding [32]byte
	binding[0] = 1
	envelope, err := writer.SealV2([]byte("cursor"), 1, binding)
	if err != nil {
		t.Fatal(err)
	}
	if plain, status, err := reader.OpenV2(envelope, 1, binding); err != nil || status != CursorOpenAuthenticationFailed || plain != nil {
		t.Fatalf("wrong-key OpenV2 = %q, %s, %v", plain, status, err)
	}

	tampered := append([]byte(nil), envelope...)
	digestOffset := len(cursorEnvelopeMagic) + 2
	tampered[digestOffset] ^= 0xff
	tamperedBinding := binding
	tamperedBinding[0] ^= 0xff
	if plain, status, err := writer.OpenV2(tampered, 1, tamperedBinding); err != nil || status != CursorOpenAuthenticationFailed || plain != nil {
		t.Fatalf("tampered-header OpenV2 = %q, %s, %v", plain, status, err)
	}
}

func TestCursorCipherV2ClassifiesLegacyUnsupportedAndMalformedEnvelopes(t *testing.T) {
	cipher, err := NewCursorCipher(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := cipher.Encrypt("legacy-cursor")
	if err != nil {
		t.Fatal(err)
	}
	var binding [32]byte
	if plain, status, err := cipher.OpenV2(legacy, 1, binding); err != nil || status != CursorOpenLegacyUnbound || plain != nil {
		t.Fatalf("legacy OpenV2 = %q, %s, %v", plain, status, err)
	}

	envelope, err := cipher.SealV2([]byte("cursor"), 1, binding)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := append([]byte(nil), envelope...)
	unsupported[len(cursorEnvelopeMagic)] = 0x7f
	if plain, status, err := cipher.OpenV2(unsupported, 1, binding); err != nil || status != CursorOpenUnsupportedEnvelope || plain != nil {
		t.Fatalf("unsupported OpenV2 = %q, %s, %v", plain, status, err)
	}
	malformed := envelope[:len(cursorEnvelopeMagic)+2+cursorBindingDigestSize]
	if plain, status, err := cipher.OpenV2(malformed, 1, binding); err != nil || status != CursorOpenAuthenticationFailed || plain != nil {
		t.Fatalf("malformed OpenV2 = %q, %s, %v", plain, status, err)
	}
}

func TestCursorCipherKeyringReadsLegacyAndWritesKeyedPrimaryEnvelopes(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x51}, 32)
	newKey := bytes.Repeat([]byte{0x52}, 32)
	legacy, err := NewCursorCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := NewCursorCipherWithKeyring(
		CipherKey{ID: "runtime-v2", Key: newKey},
		CipherKey{ID: "runtime-v1", Key: oldKey},
	)
	if err != nil {
		t.Fatal(err)
	}

	legacyTarget, err := legacy.Encrypt("legacy-target-configuration")
	if err != nil {
		t.Fatal(err)
	}
	plain, metadata, err := keyring.DecryptWithMetadata(legacyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "legacy-target-configuration" || metadata.KeyID != "runtime-v1" || metadata.Primary || metadata.Keyed {
		t.Fatalf("legacy target metadata = %#v, plain = %q", metadata, plain)
	}

	keyedTarget, err := keyring.Encrypt("keyed-target-configuration")
	if err != nil {
		t.Fatal(err)
	}
	plain, metadata, err = keyring.DecryptWithMetadata(keyedTarget)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "keyed-target-configuration" || metadata.KeyID != "runtime-v2" || !metadata.Primary || !metadata.Keyed {
		t.Fatalf("keyed target metadata = %#v, plain = %q", metadata, plain)
	}
	if _, err := legacy.Decrypt(keyedTarget); err == nil {
		t.Fatal("legacy cipher accepted a keyed target envelope")
	}

	var binding [32]byte
	binding[0] = 0x22
	legacyCursor, err := legacy.SealV2([]byte("legacy-cursor"), 1, binding)
	if err != nil {
		t.Fatal(err)
	}
	opened, status, metadata, err := keyring.OpenV2WithMetadata(legacyCursor, 1, binding)
	if err != nil || status != CursorOpenValid || string(opened) != "legacy-cursor" ||
		metadata.KeyID != "runtime-v1" || metadata.Primary || metadata.Keyed {
		t.Fatalf("legacy cursor open = %q, %s, %#v, %v", opened, status, metadata, err)
	}
	keyedCursor, err := keyring.SealV2([]byte("keyed-cursor"), 1, binding)
	if err != nil {
		t.Fatal(err)
	}
	opened, status, metadata, err = keyring.OpenV2WithMetadata(keyedCursor, 1, binding)
	if err != nil || status != CursorOpenValid || string(opened) != "keyed-cursor" ||
		metadata.KeyID != "runtime-v2" || !metadata.Primary || !metadata.Keyed {
		t.Fatalf("keyed cursor open = %q, %s, %#v, %v", opened, status, metadata, err)
	}
	tampered := append([]byte(nil), keyedCursor...)
	keyIDOffset := len(cursorEnvelopeMagic) + 4
	tampered[keyIDOffset] ^= 1
	if opened, status, _, err := keyring.OpenV2WithMetadata(tampered, 1, binding); err != nil ||
		status != CursorOpenAuthenticationFailed || opened != nil {
		t.Fatalf("tampered keyed cursor = %q, %s, %v", opened, status, err)
	}
}

func TestCursorCipherKeyringRejectsDuplicateIdentitiesAndMaterial(t *testing.T) {
	primary := CipherKey{ID: "runtime-v2", Key: bytes.Repeat([]byte{0x61}, 32)}
	if _, err := NewCursorCipherWithKeyring(primary, CipherKey{
		ID: "runtime-v2", Key: bytes.Repeat([]byte{0x62}, 32),
	}); err == nil {
		t.Fatal("duplicate runtime key ID was accepted")
	}
	if _, err := NewCursorCipherWithKeyring(primary, CipherKey{
		ID: "runtime-v1", Key: append([]byte(nil), primary.Key...),
	}); err == nil {
		t.Fatal("duplicate runtime key material was accepted")
	}
}

func TestCursorCipherV2AllowsAuthoritativeFallbackWithoutConfiguredCipher(t *testing.T) {
	var cipher *CursorCipher
	var binding [32]byte
	if plain, status, err := cipher.OpenV2([]byte("ciphertext"), 1, binding); err != nil || status != CursorOpenCipherUnavailable || plain != nil {
		t.Fatalf("nil cipher OpenV2 = %q, %s, %v", plain, status, err)
	}
	if _, err := cipher.SealV2([]byte("cursor"), 1, binding); err == nil {
		t.Fatal("nil cipher unexpectedly sealed a Cursor")
	}
}

// Encrypt/Decrypt is the unversioned pair still used to protect Execution
// Target configuration in the SSH, Docker and Kubernetes reconcilers. The
// package only exercised the versioned SealV2 envelope, so this covers the
// round trip plus the two rejections a caller depends on: a wrong key and a
// tampered ciphertext must both fail rather than return partial plaintext.
func TestCursorCipherEncryptDecryptRoundTripAndRejections(t *testing.T) {
	cipher, err := NewCursorCipher(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const plain = "ssh://worker.example.internal:22?fingerprint=abc"

	encrypted, err := cipher.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(plain)) {
		t.Fatal("ciphertext contains the plaintext configuration")
	}
	decrypted, err := cipher.Decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != plain {
		t.Fatalf("round trip returned %q", decrypted)
	}

	// A distinct nonce per call, so two encryptions of one value do not match.
	repeat, err := cipher.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encrypted, repeat) {
		t.Fatal("Encrypt reused a nonce")
	}

	other, err := NewCursorCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt(encrypted); err == nil {
		t.Fatal("a wrong key decrypted the configuration")
	}

	tampered := append([]byte(nil), encrypted...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := cipher.Decrypt(tampered); err == nil {
		t.Fatal("a tampered ciphertext was accepted")
	}

	if _, err := cipher.Decrypt(encrypted[:4]); err == nil {
		t.Fatal("a truncated ciphertext was accepted")
	}
	var absent *CursorCipher
	if _, err := absent.Decrypt(encrypted); err == nil {
		t.Fatal("an unconfigured cipher reported success")
	}
}

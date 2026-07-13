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
	unsupported[len(cursorEnvelopeMagic)]++
	if plain, status, err := cipher.OpenV2(unsupported, 1, binding); err != nil || status != CursorOpenUnsupportedEnvelope || plain != nil {
		t.Fatalf("unsupported OpenV2 = %q, %s, %v", plain, status, err)
	}
	malformed := envelope[:len(cursorEnvelopeMagic)+2+cursorBindingDigestSize]
	if plain, status, err := cipher.OpenV2(malformed, 1, binding); err != nil || status != CursorOpenAuthenticationFailed || plain != nil {
		t.Fatalf("malformed OpenV2 = %q, %s, %v", plain, status, err)
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

package kms

import (
	"bytes"
	"context"
	"testing"
)

func TestLocalEnvelopeEncryptionBindsCiphertextToAAD(t *testing.T) {
	wrapper, err := NewLocalKeyWrapper("local-test-v1", bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cipher := NewEnvelopeCipher(wrapper)
	plaintext := []byte(`{"apiKey":"credential-secret"}`)
	aad := []byte("tenant/credential/provider/api_key")
	envelope, err := cipher.Encrypt(context.Background(), plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope.EncryptedPayload, []byte("credential-secret")) ||
		bytes.Contains(envelope.EncryptedDataKey, plaintext) {
		t.Fatal("credential plaintext leaked into the envelope")
	}
	decrypted, err := cipher.Decrypt(context.Background(), envelope, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("credential round trip mismatch: %q", decrypted)
	}
	if _, err := cipher.Decrypt(context.Background(), envelope, []byte("different-resource")); err == nil {
		t.Fatal("credential envelope decrypted with different AAD")
	}
}

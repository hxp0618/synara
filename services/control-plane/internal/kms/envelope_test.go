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

func TestEnvelopeKeyringWritesPrimaryReadsFallbackAndRewrapsWithoutPayloadChange(t *testing.T) {
	oldWrapper, err := NewLocalKeyWrapper("local-old", bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	newWrapper, err := NewLocalKeyWrapper("local-new", bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	oldCipher := NewEnvelopeCipher(oldWrapper)
	keyring, err := NewEnvelopeCipherWithDecryptors(newWrapper, oldWrapper)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"clientSecret":"do-not-log"}`)
	aad := []byte("tenant/identity/connection")
	oldEnvelope, err := oldCipher.Encrypt(context.Background(), plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := keyring.Decrypt(context.Background(), oldEnvelope, aad)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("fallback decrypt = %q, %v", decrypted, err)
	}
	rewrapped, changed, err := keyring.RewrapToPrimary(context.Background(), oldEnvelope, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || rewrapped.KMSKeyID != "local-new" || rewrapped.KMSProvider != "local" {
		t.Fatalf("unexpected rewrap result: changed=%v envelope=%#v", changed, rewrapped)
	}
	if !bytes.Equal(rewrapped.EncryptedPayload, oldEnvelope.EncryptedPayload) ||
		bytes.Equal(rewrapped.EncryptedDataKey, oldEnvelope.EncryptedDataKey) {
		t.Fatal("rewrap changed the payload or retained the old wrapped data key")
	}
	decrypted, err = keyring.Decrypt(context.Background(), rewrapped, aad)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("primary decrypt = %q, %v", decrypted, err)
	}
	if _, changed, err := keyring.RewrapToPrimary(context.Background(), rewrapped, aad); err != nil || changed {
		t.Fatalf("primary rewrap should be a no-op: changed=%v err=%v", changed, err)
	}
	newEnvelope, err := keyring.Encrypt(context.Background(), plaintext, aad)
	if err != nil || newEnvelope.KMSKeyID != "local-new" {
		t.Fatalf("new write did not use primary: %#v, %v", newEnvelope, err)
	}
}

func TestEnvelopeKeyringRejectsDuplicateAndUnknownKeys(t *testing.T) {
	wrapper, err := NewLocalKeyWrapper("local-one", bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnvelopeCipherWithDecryptors(wrapper, wrapper); err == nil {
		t.Fatal("duplicate keyring identity was accepted")
	}
	cipher := NewEnvelopeCipher(wrapper)
	_, err = cipher.Decrypt(context.Background(), Envelope{
		EncryptedPayload: []byte("payload"), EncryptedDataKey: []byte("key"),
		KMSProvider: "local", KMSKeyID: "unknown",
	}, []byte("aad"))
	if err == nil {
		t.Fatal("unknown KMS identity was accepted")
	}
}

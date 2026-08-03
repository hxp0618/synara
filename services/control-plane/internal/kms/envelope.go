package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

type KeyWrapper interface {
	Provider() string
	KeyID() string
	WrapKey(context.Context, []byte, []byte) ([]byte, error)
	UnwrapKey(context.Context, []byte, []byte) ([]byte, error)
}

type Envelope struct {
	EncryptedPayload []byte
	EncryptedDataKey []byte
	KMSProvider      string
	KMSKeyID         string
}

type EnvelopeCipher struct {
	primary  KeyWrapper
	wrappers map[string]KeyWrapper
}

func NewEnvelopeCipher(wrapper KeyWrapper) *EnvelopeCipher {
	if wrapper == nil {
		return nil
	}
	cipher, err := NewEnvelopeCipherWithDecryptors(wrapper)
	if err != nil {
		return nil
	}
	return cipher
}

func NewEnvelopeCipherWithDecryptors(primary KeyWrapper, decryptors ...KeyWrapper) (*EnvelopeCipher, error) {
	if primary == nil {
		return nil, errors.New("credential KMS primary key is not configured")
	}
	wrappers := make(map[string]KeyWrapper, len(decryptors)+1)
	for _, wrapper := range append([]KeyWrapper{primary}, decryptors...) {
		if wrapper == nil {
			return nil, errors.New("credential KMS decrypt key is not configured")
		}
		identity := wrapperIdentity(wrapper.Provider(), wrapper.KeyID())
		if _, exists := wrappers[identity]; exists {
			return nil, errors.New("credential KMS keyring contains a duplicate key")
		}
		wrappers[identity] = wrapper
	}
	return &EnvelopeCipher{primary: primary, wrappers: wrappers}, nil
}

func (c *EnvelopeCipher) Encrypt(ctx context.Context, plaintext, aad []byte) (Envelope, error) {
	if c == nil || c.primary == nil {
		return Envelope{}, errors.New("credential KMS is not configured")
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return Envelope{}, fmt.Errorf("generate credential data key: %w", err)
	}
	defer zero(dataKey)
	encryptedPayload, err := seal(dataKey, plaintext, aad)
	if err != nil {
		return Envelope{}, err
	}
	encryptedDataKey, err := c.primary.WrapKey(ctx, dataKey, aad)
	if err != nil {
		return Envelope{}, fmt.Errorf("wrap credential data key: %w", err)
	}
	return Envelope{
		EncryptedPayload: encryptedPayload, EncryptedDataKey: encryptedDataKey,
		KMSProvider: c.primary.Provider(), KMSKeyID: c.primary.KeyID(),
	}, nil
}

func (c *EnvelopeCipher) Decrypt(ctx context.Context, envelope Envelope, aad []byte) ([]byte, error) {
	if c == nil || c.primary == nil {
		return nil, errors.New("credential KMS is not configured")
	}
	wrapper, ok := c.wrappers[wrapperIdentity(envelope.KMSProvider, envelope.KMSKeyID)]
	if !ok {
		return nil, errors.New("credential envelope belongs to an unconfigured KMS key")
	}
	dataKey, err := wrapper.UnwrapKey(ctx, envelope.EncryptedDataKey, aad)
	if err != nil {
		return nil, fmt.Errorf("unwrap credential data key: %w", err)
	}
	defer zero(dataKey)
	if len(dataKey) != 32 {
		return nil, errors.New("credential data key is invalid")
	}
	plaintext, err := open(dataKey, envelope.EncryptedPayload, aad)
	if err != nil {
		return nil, errors.New("decrypt credential payload: authentication failed")
	}
	return plaintext, nil
}

func (c *EnvelopeCipher) PrimaryIdentity() (string, string) {
	if c == nil || c.primary == nil {
		return "", ""
	}
	return c.primary.Provider(), c.primary.KeyID()
}

func (c *EnvelopeCipher) CanDecrypt(provider, keyID string) bool {
	if c == nil {
		return false
	}
	_, ok := c.wrappers[wrapperIdentity(provider, keyID)]
	return ok
}

// RewrapToPrimary validates the payload with its current data key, then wraps
// that same data key with the configured primary KMS key. The business payload
// and its AAD stay byte-for-byte unchanged, so a KEK rotation does not advance
// Credential or identity-resource versions.
func (c *EnvelopeCipher) RewrapToPrimary(
	ctx context.Context,
	envelope Envelope,
	aad []byte,
) (Envelope, bool, error) {
	if c == nil || c.primary == nil {
		return Envelope{}, false, errors.New("credential KMS is not configured")
	}
	if envelope.KMSProvider == c.primary.Provider() && envelope.KMSKeyID == c.primary.KeyID() {
		return envelope, false, nil
	}
	wrapper, ok := c.wrappers[wrapperIdentity(envelope.KMSProvider, envelope.KMSKeyID)]
	if !ok {
		return Envelope{}, false, errors.New("credential envelope belongs to an unconfigured KMS key")
	}
	dataKey, err := wrapper.UnwrapKey(ctx, envelope.EncryptedDataKey, aad)
	if err != nil {
		return Envelope{}, false, fmt.Errorf("unwrap credential data key: %w", err)
	}
	defer zero(dataKey)
	if len(dataKey) != 32 {
		return Envelope{}, false, errors.New("credential data key is invalid")
	}
	plaintext, err := open(dataKey, envelope.EncryptedPayload, aad)
	if err != nil {
		return Envelope{}, false, errors.New("decrypt credential payload: authentication failed")
	}
	zero(plaintext)
	encryptedDataKey, err := c.primary.WrapKey(ctx, dataKey, aad)
	if err != nil {
		return Envelope{}, false, fmt.Errorf("wrap credential data key: %w", err)
	}
	return Envelope{
		EncryptedPayload: envelope.EncryptedPayload,
		EncryptedDataKey: encryptedDataKey,
		KMSProvider:      c.primary.Provider(),
		KMSKeyID:         c.primary.KeyID(),
	}, true, nil
}

func wrapperIdentity(provider, keyID string) string {
	return provider + "\x00" + keyID
}

func seal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func open(key, encrypted, aad []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(encrypted) < aead.NonceSize() {
		return nil, errors.New("ciphertext is truncated")
	}
	return aead.Open(nil, encrypted[:aead.NonceSize()], encrypted[aead.NonceSize():], aad)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

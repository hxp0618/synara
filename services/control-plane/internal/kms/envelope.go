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
	wrapper KeyWrapper
}

func NewEnvelopeCipher(wrapper KeyWrapper) *EnvelopeCipher {
	if wrapper == nil {
		return nil
	}
	return &EnvelopeCipher{wrapper: wrapper}
}

func (c *EnvelopeCipher) Encrypt(ctx context.Context, plaintext, aad []byte) (Envelope, error) {
	if c == nil || c.wrapper == nil {
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
	encryptedDataKey, err := c.wrapper.WrapKey(ctx, dataKey, aad)
	if err != nil {
		return Envelope{}, fmt.Errorf("wrap credential data key: %w", err)
	}
	return Envelope{
		EncryptedPayload: encryptedPayload, EncryptedDataKey: encryptedDataKey,
		KMSProvider: c.wrapper.Provider(), KMSKeyID: c.wrapper.KeyID(),
	}, nil
}

func (c *EnvelopeCipher) Decrypt(ctx context.Context, envelope Envelope, aad []byte) ([]byte, error) {
	if c == nil || c.wrapper == nil {
		return nil, errors.New("credential KMS is not configured")
	}
	if envelope.KMSProvider != c.wrapper.Provider() || envelope.KMSKeyID != c.wrapper.KeyID() {
		return nil, errors.New("credential envelope belongs to a different KMS key")
	}
	dataKey, err := c.wrapper.UnwrapKey(ctx, envelope.EncryptedDataKey, aad)
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

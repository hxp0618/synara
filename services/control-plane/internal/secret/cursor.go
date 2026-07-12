package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

type CursorCipher struct {
	aead cipher.AEAD
}

func NewCursorCipher(key []byte) (*CursorCipher, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, errors.New("provider cursor encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create provider cursor cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create provider cursor AEAD: %w", err)
	}
	return &CursorCipher{aead: aead}, nil
}

func (c *CursorCipher) Encrypt(plain string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("provider cursor encryption is not configured")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate provider cursor nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (c *CursorCipher) Decrypt(encrypted []byte) (string, error) {
	if c == nil {
		return "", errors.New("provider cursor encryption is not configured")
	}
	if len(encrypted) < c.aead.NonceSize() {
		return "", errors.New("provider cursor ciphertext is invalid")
	}
	nonce := encrypted[:c.aead.NonceSize()]
	plain, err := c.aead.Open(nil, nonce, encrypted[c.aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt provider cursor: %w", err)
	}
	return string(plain), nil
}

package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const (
	cursorEnvelopeV2Version = byte(2)
	cursorBindingDigestSize = 32
)

var cursorEnvelopeMagic = [8]byte{'S', 'Y', 'N', 'C', 'R', 'S', 'R', 0}

type CursorOpenStatus string

const (
	CursorOpenValid                CursorOpenStatus = "valid"
	CursorOpenBindingMismatch      CursorOpenStatus = "binding-mismatch"
	CursorOpenLegacyUnbound        CursorOpenStatus = "legacy-unbound"
	CursorOpenAuthenticationFailed CursorOpenStatus = "authentication-failed"
	CursorOpenUnsupportedEnvelope  CursorOpenStatus = "unsupported-envelope"
	CursorOpenCipherUnavailable    CursorOpenStatus = "cipher-unavailable"
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

// SealV2 creates a versioned envelope whose header is authenticated as AES-GCM
// additional data. The binding digest is deliberately visible so callers can
// distinguish an explicit resource mismatch from an authentication failure
// caused by a temporary wrong key. The digest contains no Cursor plaintext.
func (c *CursorCipher) SealV2(
	plain []byte,
	bindingVersion byte,
	bindingDigest [cursorBindingDigestSize]byte,
) ([]byte, error) {
	if c == nil {
		return nil, errors.New("provider cursor encryption is not configured")
	}
	if bindingVersion == 0 {
		return nil, errors.New("provider cursor binding version is required")
	}
	header := make([]byte, 0, len(cursorEnvelopeMagic)+2+len(bindingDigest))
	header = append(header, cursorEnvelopeMagic[:]...)
	header = append(header, cursorEnvelopeV2Version, bindingVersion)
	header = append(header, bindingDigest[:]...)
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate provider cursor nonce: %w", err)
	}
	envelope := append(append([]byte(nil), header...), nonce...)
	return c.aead.Seal(envelope, nonce, plain, header), nil
}

// OpenV2 authenticates a bound Cursor envelope without conflating binding
// drift with key/ciphertext failures. Callers may discard only an explicit
// CursorOpenBindingMismatch. Authentication failures and unknown envelopes
// must preserve the ciphertext and fall back to authoritative history.
func (c *CursorCipher) OpenV2(
	envelope []byte,
	expectedBindingVersion byte,
	expectedBindingDigest [cursorBindingDigestSize]byte,
) ([]byte, CursorOpenStatus, error) {
	if c == nil {
		return nil, CursorOpenCipherUnavailable, nil
	}
	if len(envelope) < len(cursorEnvelopeMagic) || !bytes.Equal(envelope[:len(cursorEnvelopeMagic)], cursorEnvelopeMagic[:]) {
		return nil, CursorOpenLegacyUnbound, nil
	}
	headerSize := len(cursorEnvelopeMagic) + 2 + cursorBindingDigestSize
	if len(envelope) < headerSize+c.aead.NonceSize()+c.aead.Overhead() {
		return nil, CursorOpenAuthenticationFailed, nil
	}
	if envelope[len(cursorEnvelopeMagic)] != cursorEnvelopeV2Version ||
		envelope[len(cursorEnvelopeMagic)+1] != expectedBindingVersion {
		return nil, CursorOpenUnsupportedEnvelope, nil
	}
	digestOffset := len(cursorEnvelopeMagic) + 2
	actualDigest := envelope[digestOffset : digestOffset+cursorBindingDigestSize]
	if !bytes.Equal(actualDigest, expectedBindingDigest[:]) {
		return nil, CursorOpenBindingMismatch, nil
	}
	header := envelope[:headerSize]
	nonce := envelope[headerSize : headerSize+c.aead.NonceSize()]
	ciphertext := envelope[headerSize+c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, header)
	if err != nil {
		return nil, CursorOpenAuthenticationFailed, nil
	}
	return plain, CursorOpenValid, nil
}

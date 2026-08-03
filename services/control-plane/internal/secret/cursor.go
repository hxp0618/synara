package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	cursorEnvelopeV2Version   = byte(2)
	cursorEnvelopeV3Version   = byte(3)
	runtimeSecretVersion      = byte(1)
	cursorBindingDigestSize   = 32
	maximumRuntimeSecretKeyID = 200
	maximumRuntimeDecryptKeys = 8
)

var (
	cursorEnvelopeMagic = [8]byte{'S', 'Y', 'N', 'C', 'R', 'S', 'R', 0}
	runtimeSecretMagic  = [8]byte{'S', 'Y', 'N', 'R', 'T', 'S', 'E', 'C'}
)

type CursorOpenStatus string

const (
	CursorOpenValid                CursorOpenStatus = "valid"
	CursorOpenBindingMismatch      CursorOpenStatus = "binding-mismatch"
	CursorOpenLegacyUnbound        CursorOpenStatus = "legacy-unbound"
	CursorOpenAuthenticationFailed CursorOpenStatus = "authentication-failed"
	CursorOpenUnsupportedEnvelope  CursorOpenStatus = "unsupported-envelope"
	CursorOpenCipherUnavailable    CursorOpenStatus = "cipher-unavailable"
)

type CipherKey struct {
	ID  string
	Key []byte
}

type DecryptionMetadata struct {
	KeyID   string
	Primary bool
	Keyed   bool
}

type namedAEAD struct {
	id   string
	aead cipher.AEAD
}

type CursorCipher struct {
	primary    namedAEAD
	decryptors []namedAEAD
	byID       map[string]namedAEAD
	keyed      bool
}

// NewCursorCipher preserves the pre-keyring wire format. It exists for
// compatibility and tests; production rotation first deploys this reader,
// then explicitly configures a key ID through NewCursorCipherWithKeyring.
func NewCursorCipher(key []byte) (*CursorCipher, error) {
	if len(key) == 0 {
		return nil, nil
	}
	aead, err := runtimeAEAD(key)
	if err != nil {
		return nil, err
	}
	primary := namedAEAD{aead: aead}
	return &CursorCipher{primary: primary, decryptors: []namedAEAD{primary}}, nil
}

func NewCursorCipherWithKeyring(primary CipherKey, decryptKeys ...CipherKey) (*CursorCipher, error) {
	primary.ID = strings.TrimSpace(primary.ID)
	if err := validateRuntimeKey(primary, "primary"); err != nil {
		return nil, err
	}
	if len(decryptKeys) > maximumRuntimeDecryptKeys {
		return nil, fmt.Errorf("provider cursor keyring supports at most %d decrypt-only keys", maximumRuntimeDecryptKeys)
	}
	keys := append([]CipherKey{primary}, decryptKeys...)
	decryptors := make([]namedAEAD, 0, len(keys))
	byID := make(map[string]namedAEAD, len(keys))
	seenKeys := make([][]byte, 0, len(keys))
	for index, key := range keys {
		key.ID = strings.TrimSpace(key.ID)
		label := fmt.Sprintf("decrypt-only key %d", index)
		if index == 0 {
			label = "primary"
		}
		if err := validateRuntimeKey(key, label); err != nil {
			return nil, err
		}
		if _, exists := byID[key.ID]; exists {
			return nil, errors.New("provider cursor keyring contains a duplicate key ID")
		}
		for _, seen := range seenKeys {
			if bytes.Equal(key.Key, seen) {
				return nil, errors.New("provider cursor keyring contains duplicate key material")
			}
		}
		aead, err := runtimeAEAD(key.Key)
		if err != nil {
			return nil, err
		}
		named := namedAEAD{id: key.ID, aead: aead}
		decryptors = append(decryptors, named)
		byID[key.ID] = named
		seenKeys = append(seenKeys, append([]byte(nil), key.Key...))
	}
	return &CursorCipher{
		primary: decryptors[0], decryptors: decryptors, byID: byID, keyed: true,
	}, nil
}

func (c *CursorCipher) PrimaryKeyID() string {
	if c == nil {
		return ""
	}
	return c.primary.id
}

func (c *CursorCipher) Encrypt(plain string) ([]byte, error) {
	return c.EncryptBytes([]byte(plain))
}

func (c *CursorCipher) EncryptBytes(plain []byte) ([]byte, error) {
	if c == nil {
		return nil, errors.New("provider cursor encryption is not configured")
	}
	if !c.keyed {
		return sealRuntimeSecret(c.primary.aead, nil, plain)
	}
	header := runtimeSecretHeader(c.primary.id)
	return sealRuntimeSecret(c.primary.aead, header, plain)
}

func (c *CursorCipher) Decrypt(encrypted []byte) (string, error) {
	plain, _, err := c.DecryptWithMetadata(encrypted)
	return plain, err
}

func (c *CursorCipher) DecryptWithMetadata(encrypted []byte) (string, DecryptionMetadata, error) {
	plain, metadata, err := c.DecryptBytesWithMetadata(encrypted)
	return string(plain), metadata, err
}

func (c *CursorCipher) DecryptBytesWithMetadata(encrypted []byte) ([]byte, DecryptionMetadata, error) {
	if c == nil {
		return nil, DecryptionMetadata{}, errors.New("provider cursor encryption is not configured")
	}
	if len(encrypted) >= len(runtimeSecretMagic) && bytes.Equal(encrypted[:len(runtimeSecretMagic)], runtimeSecretMagic[:]) {
		keyID, headerSize, ok := parseRuntimeSecretHeader(encrypted)
		if !ok {
			return nil, DecryptionMetadata{}, errors.New("execution target configuration envelope is invalid")
		}
		key, exists := c.byID[keyID]
		if !exists {
			return nil, DecryptionMetadata{}, errors.New("execution target configuration belongs to an unconfigured key")
		}
		plain, err := openRuntimeSecret(key.aead, encrypted, headerSize, encrypted[:headerSize])
		if err != nil {
			return nil, DecryptionMetadata{}, errors.New("decrypt execution target configuration: authentication failed")
		}
		return plain, c.metadata(key, true), nil
	}
	for _, key := range c.decryptors {
		plain, err := openRuntimeSecret(key.aead, encrypted, 0, nil)
		if err == nil {
			return plain, c.metadata(key, false), nil
		}
	}
	return nil, DecryptionMetadata{}, errors.New("decrypt provider cursor: authentication failed")
}

// SealV2 writes the historical v2 shape until an explicit primary key ID is
// configured. Keyring mode writes v3, which authenticates that key ID together
// with the existing resource binding.
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
	header := cursorHeaderV2(bindingVersion, bindingDigest)
	if c.keyed {
		header = cursorHeaderV3(bindingVersion, bindingDigest, c.primary.id)
	}
	return sealRuntimeSecret(c.primary.aead, header, plain)
}

func (c *CursorCipher) OpenV2(
	envelope []byte,
	expectedBindingVersion byte,
	expectedBindingDigest [cursorBindingDigestSize]byte,
) ([]byte, CursorOpenStatus, error) {
	plain, status, _, err := c.OpenV2WithMetadata(envelope, expectedBindingVersion, expectedBindingDigest)
	return plain, status, err
}

func (c *CursorCipher) OpenV2WithMetadata(
	envelope []byte,
	expectedBindingVersion byte,
	expectedBindingDigest [cursorBindingDigestSize]byte,
) ([]byte, CursorOpenStatus, DecryptionMetadata, error) {
	if c == nil {
		return nil, CursorOpenCipherUnavailable, DecryptionMetadata{}, nil
	}
	if len(envelope) < len(cursorEnvelopeMagic) || !bytes.Equal(envelope[:len(cursorEnvelopeMagic)], cursorEnvelopeMagic[:]) {
		return nil, CursorOpenLegacyUnbound, DecryptionMetadata{}, nil
	}
	if len(envelope) <= len(cursorEnvelopeMagic) {
		return nil, CursorOpenAuthenticationFailed, DecryptionMetadata{}, nil
	}
	switch envelope[len(cursorEnvelopeMagic)] {
	case cursorEnvelopeV2Version:
		headerSize := len(cursorEnvelopeMagic) + 2 + cursorBindingDigestSize
		if len(envelope) < headerSize+c.primary.aead.NonceSize()+c.primary.aead.Overhead() {
			return nil, CursorOpenAuthenticationFailed, DecryptionMetadata{}, nil
		}
		if envelope[len(cursorEnvelopeMagic)+1] != expectedBindingVersion {
			return nil, CursorOpenUnsupportedEnvelope, DecryptionMetadata{}, nil
		}
		digestOffset := len(cursorEnvelopeMagic) + 2
		if !bytes.Equal(envelope[digestOffset:digestOffset+cursorBindingDigestSize], expectedBindingDigest[:]) {
			return nil, CursorOpenBindingMismatch, DecryptionMetadata{}, nil
		}
		for _, key := range c.decryptors {
			plain, err := openRuntimeSecret(key.aead, envelope, headerSize, envelope[:headerSize])
			if err == nil {
				return plain, CursorOpenValid, c.metadata(key, false), nil
			}
		}
		return nil, CursorOpenAuthenticationFailed, DecryptionMetadata{}, nil
	case cursorEnvelopeV3Version:
		bindingVersion, digest, keyID, headerSize, ok := parseCursorHeaderV3(envelope)
		if !ok {
			return nil, CursorOpenAuthenticationFailed, DecryptionMetadata{}, nil
		}
		if bindingVersion != expectedBindingVersion {
			return nil, CursorOpenUnsupportedEnvelope, DecryptionMetadata{}, nil
		}
		if !bytes.Equal(digest, expectedBindingDigest[:]) {
			return nil, CursorOpenBindingMismatch, DecryptionMetadata{}, nil
		}
		key, exists := c.byID[keyID]
		if !exists {
			return nil, CursorOpenAuthenticationFailed, DecryptionMetadata{}, nil
		}
		plain, err := openRuntimeSecret(key.aead, envelope, headerSize, envelope[:headerSize])
		if err != nil {
			return nil, CursorOpenAuthenticationFailed, DecryptionMetadata{}, nil
		}
		return plain, CursorOpenValid, c.metadata(key, true), nil
	default:
		return nil, CursorOpenUnsupportedEnvelope, DecryptionMetadata{}, nil
	}
}

func (c *CursorCipher) metadata(key namedAEAD, keyed bool) DecryptionMetadata {
	return DecryptionMetadata{KeyID: key.id, Primary: key.id == c.primary.id, Keyed: keyed}
}

func validateRuntimeKey(key CipherKey, label string) error {
	if key.ID == "" || len(key.ID) > maximumRuntimeSecretKeyID || strings.ContainsAny(key.ID, "\r\n\t") {
		return fmt.Errorf("provider cursor %s key ID is invalid", label)
	}
	if len(key.Key) != 32 {
		return fmt.Errorf("provider cursor %s key must be 32 bytes", label)
	}
	return nil
}

func runtimeAEAD(key []byte) (cipher.AEAD, error) {
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
	return aead, nil
}

func sealRuntimeSecret(aead cipher.AEAD, header, plain []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate provider cursor nonce: %w", err)
	}
	envelope := append(append([]byte(nil), header...), nonce...)
	return aead.Seal(envelope, nonce, plain, header), nil
}

func openRuntimeSecret(aead cipher.AEAD, envelope []byte, headerSize int, aad []byte) ([]byte, error) {
	if len(envelope) < headerSize+aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("provider cursor ciphertext is invalid")
	}
	nonce := envelope[headerSize : headerSize+aead.NonceSize()]
	ciphertext := envelope[headerSize+aead.NonceSize():]
	return aead.Open(nil, nonce, ciphertext, aad)
}

func runtimeSecretHeader(keyID string) []byte {
	header := make([]byte, 0, len(runtimeSecretMagic)+3+len(keyID))
	header = append(header, runtimeSecretMagic[:]...)
	header = append(header, runtimeSecretVersion, 0, 0)
	binary.BigEndian.PutUint16(header[len(runtimeSecretMagic)+1:], uint16(len(keyID)))
	header = append(header, keyID...)
	return header
}

func parseRuntimeSecretHeader(envelope []byte) (string, int, bool) {
	minimum := len(runtimeSecretMagic) + 3
	if len(envelope) < minimum || envelope[len(runtimeSecretMagic)] != runtimeSecretVersion {
		return "", 0, false
	}
	keyLength := int(binary.BigEndian.Uint16(envelope[len(runtimeSecretMagic)+1 : minimum]))
	if keyLength < 1 || keyLength > maximumRuntimeSecretKeyID || len(envelope) < minimum+keyLength {
		return "", 0, false
	}
	keyID := string(envelope[minimum : minimum+keyLength])
	if strings.TrimSpace(keyID) != keyID || strings.ContainsAny(keyID, "\r\n\t") {
		return "", 0, false
	}
	return keyID, minimum + keyLength, true
}

func cursorHeaderV2(bindingVersion byte, digest [cursorBindingDigestSize]byte) []byte {
	header := make([]byte, 0, len(cursorEnvelopeMagic)+2+len(digest))
	header = append(header, cursorEnvelopeMagic[:]...)
	header = append(header, cursorEnvelopeV2Version, bindingVersion)
	header = append(header, digest[:]...)
	return header
}

func cursorHeaderV3(bindingVersion byte, digest [cursorBindingDigestSize]byte, keyID string) []byte {
	header := make([]byte, 0, len(cursorEnvelopeMagic)+4+len(keyID)+len(digest))
	header = append(header, cursorEnvelopeMagic[:]...)
	header = append(header, cursorEnvelopeV3Version, bindingVersion, 0, 0)
	binary.BigEndian.PutUint16(header[len(cursorEnvelopeMagic)+2:], uint16(len(keyID)))
	header = append(header, keyID...)
	header = append(header, digest[:]...)
	return header
}

func parseCursorHeaderV3(envelope []byte) (byte, []byte, string, int, bool) {
	minimum := len(cursorEnvelopeMagic) + 4
	if len(envelope) < minimum {
		return 0, nil, "", 0, false
	}
	bindingVersion := envelope[len(cursorEnvelopeMagic)+1]
	keyLength := int(binary.BigEndian.Uint16(envelope[len(cursorEnvelopeMagic)+2 : minimum]))
	if keyLength < 1 || keyLength > maximumRuntimeSecretKeyID || len(envelope) < minimum+keyLength+cursorBindingDigestSize {
		return 0, nil, "", 0, false
	}
	keyID := string(envelope[minimum : minimum+keyLength])
	if strings.TrimSpace(keyID) != keyID || strings.ContainsAny(keyID, "\r\n\t") {
		return 0, nil, "", 0, false
	}
	digestOffset := minimum + keyLength
	return bindingVersion, envelope[digestOffset : digestOffset+cursorBindingDigestSize], keyID,
		digestOffset + cursorBindingDigestSize, true
}

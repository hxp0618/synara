package kmsworker

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

var wrappedKeyMagic = []byte{'S', 'K', 'W', '1'}

type SealKeyring struct {
	primaryID string
	primary   []byte
	keys      map[string][]byte
}

func NewSealKeyring(primary []byte, fallbacks ...[]byte) (*SealKeyring, error) {
	if len(primary) != 32 {
		return nil, errors.New("KMS sealing key must be 32 bytes")
	}
	result := &SealKeyring{primaryID: sealKeyID(primary), primary: append([]byte(nil), primary...), keys: map[string][]byte{}}
	result.keys[result.primaryID] = result.primary
	for _, fallback := range fallbacks {
		if len(fallback) != 32 {
			return nil, errors.New("KMS fallback sealing key must be 32 bytes")
		}
		id := sealKeyID(fallback)
		if _, exists := result.keys[id]; exists {
			return nil, errors.New("KMS sealing keyring contains duplicate key material")
		}
		result.keys[id] = append([]byte(nil), fallback...)
	}
	return result, nil
}

func (k *SealKeyring) PrimaryID() string { return k.primaryID }

func (k *SealKeyring) Seal(plaintext, aad []byte) ([]byte, string, error) {
	sealed, err := sealAESGCM(k.primary, plaintext, aad)
	return sealed, k.primaryID, err
}

func (k *SealKeyring) Open(keyID string, sealed, aad []byte) ([]byte, error) {
	key, exists := k.keys[keyID]
	if !exists {
		return nil, errors.New("managed key belongs to an unconfigured sealing key")
	}
	return openAESGCM(key, sealed, aad)
}

func sealKeyID(key []byte) string {
	digest := sha256.Sum256(append([]byte("synara-kms-seal-key-v1\x00"), key...))
	return hex.EncodeToString(digest[:16])
}

func managedKeyAAD(keyID string, version int64) []byte {
	return []byte(fmt.Sprintf("synara-kms-managed-key-v1\x00%s\x00%d\x00%s", keyID, version, AlgorithmAES256GCM))
}

func wrapAAD(keyID string, version int64, aad []byte) []byte {
	digest := sha256.Sum256(aad)
	return []byte(fmt.Sprintf("synara-kms-wrapped-data-key-v1\x00%s\x00%d\x00%s\x00%s", keyID, version, AlgorithmAES256GCM, hex.EncodeToString(digest[:])))
}

func wrapDataKey(managedKey []byte, keyID string, version int64, dataKey, aad []byte) ([]byte, error) {
	if len(dataKey) != 32 {
		return nil, errors.New("data key must be 32 bytes")
	}
	sealed, err := sealAESGCM(managedKey, dataKey, wrapAAD(keyID, version, aad))
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(wrappedKeyMagic)+len(sealed))
	result = append(result, wrappedKeyMagic...)
	return append(result, sealed...), nil
}

func unwrapDataKey(managedKey []byte, keyID string, version int64, wrapped, aad []byte) ([]byte, error) {
	if len(wrapped) <= len(wrappedKeyMagic) || string(wrapped[:len(wrappedKeyMagic)]) != string(wrappedKeyMagic) {
		return nil, errors.New("wrapped data key format is invalid")
	}
	plaintext, err := openAESGCM(managedKey, wrapped[len(wrappedKeyMagic):], wrapAAD(keyID, version, aad))
	if err != nil || len(plaintext) != 32 {
		zeroBytes(plaintext)
		return nil, errors.New("wrapped data key authentication failed")
	}
	return plaintext, nil
}

func sealAESGCM(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := aesGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func openAESGCM(key, sealed, aad []byte) ([]byte, error) {
	aead, err := aesGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("ciphertext is truncated")
	}
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("ciphertext authentication failed")
	}
	return plaintext, nil
}

func aesGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("AES-256-GCM key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

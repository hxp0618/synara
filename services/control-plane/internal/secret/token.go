package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

func NewToken() (plain string, hash []byte, err error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", nil, fmt.Errorf("read secure random bytes: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(bytes)
	digest := sha256.Sum256([]byte(plain))
	return plain, digest[:], nil
}

func HashToken(plain string) []byte {
	digest := sha256.Sum256([]byte(plain))
	return digest[:]
}

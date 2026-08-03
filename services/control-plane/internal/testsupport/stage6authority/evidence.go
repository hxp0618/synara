package stage6authority

import (
	"crypto/sha256"
	"encoding/hex"
)

func Digest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func DigestPointer(value string) *string {
	digest := Digest(value)
	return &digest
}

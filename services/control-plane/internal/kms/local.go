package kms

import (
	"context"
	"errors"
	"strings"
)

type LocalKeyWrapper struct {
	keyID string
	key   []byte
}

func NewLocalKeyWrapper(keyID string, key []byte) (*LocalKeyWrapper, error) {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || len(keyID) > 1024 {
		return nil, errors.New("local credential KMS key id is invalid")
	}
	if len(key) != 32 {
		return nil, errors.New("local credential KMS key must be 32 bytes")
	}
	return &LocalKeyWrapper{keyID: keyID, key: append([]byte(nil), key...)}, nil
}

func (w *LocalKeyWrapper) Provider() string { return "local" }
func (w *LocalKeyWrapper) KeyID() string    { return w.keyID }

func (w *LocalKeyWrapper) WrapKey(_ context.Context, dataKey, aad []byte) ([]byte, error) {
	return seal(w.key, dataKey, aad)
}

func (w *LocalKeyWrapper) UnwrapKey(_ context.Context, encrypted, aad []byte) ([]byte, error) {
	return open(w.key, encrypted, aad)
}

package runtimekeys

import (
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

// NewProviderCursorCipher is shared by live API replicas and offline rotation
// commands so both interpret the runtime keyring identically.
func NewProviderCursorCipher(cfg config.Config) (*secret.CursorCipher, error) {
	if cfg.ProviderCursorKeyID == "" {
		return secret.NewCursorCipher(cfg.ProviderCursorKey)
	}
	decryptKeys := make([]secret.CipherKey, 0, len(cfg.ProviderCursorDecryptKeys))
	for _, value := range cfg.ProviderCursorDecryptKeys {
		decryptKeys = append(decryptKeys, secret.CipherKey{ID: value.KeyID, Key: value.Key})
	}
	return secret.NewCursorCipherWithKeyring(
		secret.CipherKey{ID: cfg.ProviderCursorKeyID, Key: cfg.ProviderCursorKey}, decryptKeys...,
	)
}

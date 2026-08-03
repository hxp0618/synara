package kms

import (
	"context"
	"fmt"
	"strings"
)

type Config struct {
	Provider    string
	KeyID       string
	LocalKey    []byte
	Region      string
	Synara      SynaraKMSConfig
	DecryptKeys []DecryptKeyConfig
}

type DecryptKeyConfig struct {
	Provider string
	KeyID    string
	LocalKey []byte
	Region   string
	Synara   SynaraKMSConfig
}

func New(ctx context.Context, config Config) (*EnvelopeCipher, error) {
	provider := strings.ToLower(strings.TrimSpace(config.Provider))
	if provider == "" {
		if len(config.DecryptKeys) > 0 {
			return nil, fmt.Errorf("credential KMS decrypt keys require a primary key")
		}
		return nil, nil
	}
	primary, err := newKeyWrapper(ctx, DecryptKeyConfig{
		Provider: provider, KeyID: config.KeyID, LocalKey: config.LocalKey, Region: config.Region, Synara: config.Synara,
	}, true)
	if err != nil {
		return nil, err
	}
	decryptors := make([]KeyWrapper, 0, len(config.DecryptKeys))
	for index, decryptKey := range config.DecryptKeys {
		wrapper, err := newKeyWrapper(ctx, decryptKey, false)
		if err != nil {
			return nil, fmt.Errorf("configure credential KMS decrypt key %d: %w", index, err)
		}
		decryptors = append(decryptors, wrapper)
	}
	return NewEnvelopeCipherWithDecryptors(primary, decryptors...)
}

func newKeyWrapper(ctx context.Context, config DecryptKeyConfig, primary bool) (KeyWrapper, error) {
	switch strings.ToLower(strings.TrimSpace(config.Provider)) {
	case "":
		return nil, fmt.Errorf("credential KMS provider is required")
	case "local":
		wrapper, err := NewLocalKeyWrapper(config.KeyID, config.LocalKey)
		if err != nil {
			return nil, err
		}
		return wrapper, nil
	case "aws-kms":
		wrapper, err := NewAWSKeyWrapper(ctx, config.KeyID, config.Region)
		if err != nil {
			return nil, fmt.Errorf("configure AWS credential KMS: %w", err)
		}
		return wrapper, nil
	case "synara-kms":
		wrapper, err := NewSynaraKeyWrapper(ctx, config.KeyID, config.Synara, primary)
		if err != nil {
			return nil, fmt.Errorf("configure Synara credential KMS: %w", err)
		}
		return wrapper, nil
	default:
		return nil, fmt.Errorf("unsupported credential KMS provider %q", config.Provider)
	}
}

package kms

import (
	"context"
	"fmt"
	"strings"
)

type Config struct {
	Provider string
	KeyID    string
	LocalKey []byte
	Region   string
}

func New(ctx context.Context, config Config) (*EnvelopeCipher, error) {
	switch strings.ToLower(strings.TrimSpace(config.Provider)) {
	case "":
		return nil, nil
	case "local":
		wrapper, err := NewLocalKeyWrapper(config.KeyID, config.LocalKey)
		if err != nil {
			return nil, err
		}
		return NewEnvelopeCipher(wrapper), nil
	case "aws-kms":
		wrapper, err := NewAWSKeyWrapper(ctx, config.KeyID, config.Region)
		if err != nil {
			return nil, fmt.Errorf("configure AWS credential KMS: %w", err)
		}
		return NewEnvelopeCipher(wrapper), nil
	default:
		return nil, fmt.Errorf("unsupported credential KMS provider %q", config.Provider)
	}
}

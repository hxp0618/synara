package kms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	awstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

type awsKMSClient interface {
	Encrypt(context.Context, *awskms.EncryptInput, ...func(*awskms.Options)) (*awskms.EncryptOutput, error)
	Decrypt(context.Context, *awskms.DecryptInput, ...func(*awskms.Options)) (*awskms.DecryptOutput, error)
}

type AWSKeyWrapper struct {
	keyID  string
	client awsKMSClient
}

func NewAWSKeyWrapper(ctx context.Context, keyID, region string) (*AWSKeyWrapper, error) {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || len(keyID) > 1024 {
		return nil, errors.New("AWS credential KMS key id is invalid")
	}
	options := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if region = strings.TrimSpace(region); region != "" {
		options = append(options, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, err
	}
	return &AWSKeyWrapper{keyID: keyID, client: awskms.NewFromConfig(cfg)}, nil
}

func (w *AWSKeyWrapper) Provider() string { return "aws-kms" }
func (w *AWSKeyWrapper) KeyID() string    { return w.keyID }

func (w *AWSKeyWrapper) WrapKey(ctx context.Context, dataKey, aad []byte) ([]byte, error) {
	output, err := w.client.Encrypt(ctx, &awskms.EncryptInput{
		KeyId: &w.keyID, Plaintext: dataKey, EncryptionAlgorithm: awstypes.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext: encryptionContext(aad),
	})
	if err != nil {
		return nil, err
	}
	return output.CiphertextBlob, nil
}

func (w *AWSKeyWrapper) UnwrapKey(ctx context.Context, encrypted, aad []byte) ([]byte, error) {
	output, err := w.client.Decrypt(ctx, &awskms.DecryptInput{
		KeyId: &w.keyID, CiphertextBlob: encrypted,
		EncryptionAlgorithm: awstypes.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   encryptionContext(aad),
	})
	if err != nil {
		return nil, err
	}
	return output.Plaintext, nil
}

func encryptionContext(aad []byte) map[string]string {
	digest := sha256.Sum256(aad)
	return map[string]string{"synara_context_sha256": hex.EncodeToString(digest[:])}
}

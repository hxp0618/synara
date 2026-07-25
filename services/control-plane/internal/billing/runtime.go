package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SourceKindDisabled = "disabled"
	SourceKindLocal    = "local"
	SourceKindS3       = "s3"
	SourceKindGCS      = "gcs"
	SourceKindAzure    = "azure"
)

type RuntimeConfig struct {
	MaxObjectBytes         int64
	TariffOperatorTenantID uuid.UUID
	Source                 SourceConfig
	Imports                []ConfiguredImport
}

type SourceConfig struct {
	Kind string

	LocalBaseDir string

	S3Bucket              string
	S3Region              string
	S3Endpoint            string
	S3UsePathStyle        bool
	S3AllowCustomEndpoint bool
	S3AllowHTTP           bool

	GCSBucket string

	AzureContainerURL string
	AzureAllowHTTP    bool

	Prefix string
}

type ConfiguredImport struct {
	TenantID            uuid.UUID
	Provider            string
	ExternalImportID    string
	Format              ExportObjectFormat
	ObjectKey           string
	ObjectVersion       string
	ExecutionTargetIDs  []uuid.UUID
	ScheduleInterval    time.Duration
	Reconcile           bool
	EstimateAfterImport bool
}

func (config RuntimeConfig) Normalize() (RuntimeConfig, error) {
	normalizedSource, err := config.Source.Normalize()
	if err != nil {
		return RuntimeConfig{}, err
	}
	normalized := RuntimeConfig{
		MaxObjectBytes:         config.MaxObjectBytes,
		TariffOperatorTenantID: config.TariffOperatorTenantID,
		Source:                 normalizedSource,
		Imports:                make([]ConfiguredImport, 0, len(config.Imports)),
	}
	if normalized.MaxObjectBytes < 0 {
		return RuntimeConfig{}, errors.New("billing max object bytes must not be negative")
	}
	if normalized.MaxObjectBytes == 0 {
		normalized.MaxObjectBytes = defaultBlobInvoiceMaxBytes
	}
	importKeys := make(map[string]struct{}, len(config.Imports))
	targetProviders := make(map[string]string)
	for _, rawImport := range config.Imports {
		normalizedImport, normalizeErr := rawImport.Normalize()
		if normalizeErr != nil {
			return RuntimeConfig{}, normalizeErr
		}
		switch normalized.Source.Kind {
		case SourceKindS3, SourceKindGCS, SourceKindAzure:
			if normalizedImport.ObjectVersion == "" {
				return RuntimeConfig{}, fmt.Errorf(
					"billing import mapping for tenant %s %s/%s requires objectVersion when using %s",
					normalizedImport.TenantID, normalizedImport.Provider, normalizedImport.ExternalImportID, normalized.Source.Kind,
				)
			}
		}
		key := configuredImportKey(normalizedImport.TenantID, normalizedImport.Provider, normalizedImport.ExternalImportID)
		if _, exists := importKeys[key]; exists {
			return RuntimeConfig{}, fmt.Errorf("billing import mapping for tenant %s %s/%s is duplicated",
				normalizedImport.TenantID, normalizedImport.Provider, normalizedImport.ExternalImportID)
		}
		importKeys[key] = struct{}{}
		for _, targetID := range normalizedImport.ExecutionTargetIDs {
			targetKey := normalizedImport.TenantID.String() + "\x00" + targetID.String()
			if provider, exists := targetProviders[targetKey]; exists && provider != normalizedImport.Provider {
				return RuntimeConfig{}, fmt.Errorf(
					"billing execution target %s for tenant %s is assigned to both %s and %s estimate providers",
					targetID, normalizedImport.TenantID, provider, normalizedImport.Provider,
				)
			}
			targetProviders[targetKey] = normalizedImport.Provider
		}
		normalized.Imports = append(normalized.Imports, normalizedImport)
	}
	slices.SortFunc(normalized.Imports, func(left, right ConfiguredImport) int {
		switch {
		case left.TenantID != right.TenantID:
			return strings.Compare(left.TenantID.String(), right.TenantID.String())
		case left.Provider != right.Provider:
			return strings.Compare(left.Provider, right.Provider)
		default:
			return strings.Compare(left.ExternalImportID, right.ExternalImportID)
		}
	})
	if normalized.Source.Kind == SourceKindDisabled {
		if len(normalized.Imports) > 0 {
			return RuntimeConfig{}, errors.New("billing import mappings require a configured billing blob source")
		}
		return normalized, nil
	}
	return normalized, nil
}

func (config RuntimeConfig) MinimumScheduleInterval() time.Duration {
	var minimum time.Duration
	for _, configuredImport := range config.Imports {
		if configuredImport.ScheduleInterval <= 0 {
			continue
		}
		if minimum == 0 || configuredImport.ScheduleInterval < minimum {
			minimum = configuredImport.ScheduleInterval
		}
	}
	return minimum
}

func (config RuntimeConfig) HasScheduledImports() bool {
	return config.MinimumScheduleInterval() > 0
}

func (config RuntimeConfig) RequiresEstimateSweeper() bool {
	for _, configuredImport := range config.Imports {
		if configuredImport.EstimateAfterImport {
			return true
		}
	}
	return false
}

func (config SourceConfig) Normalize() (SourceConfig, error) {
	normalized := config
	normalized.Kind = strings.ToLower(strings.TrimSpace(normalized.Kind))
	if normalized.Kind == "" {
		normalized.Kind = SourceKindDisabled
	}
	var err error
	normalized.Prefix, err = normalizeBlobPrefix(normalized.Prefix)
	if err != nil {
		return SourceConfig{}, err
	}
	switch normalized.Kind {
	case SourceKindDisabled:
		return normalized, nil
	case SourceKindLocal:
		normalized.LocalBaseDir = strings.TrimSpace(normalized.LocalBaseDir)
		if normalized.LocalBaseDir == "" {
			return SourceConfig{}, errors.New("billing local blob source requires a base directory")
		}
		return normalized, nil
	case SourceKindS3:
		normalized.S3Bucket = strings.TrimSpace(normalized.S3Bucket)
		normalized.S3Region = strings.TrimSpace(normalized.S3Region)
		normalized.S3Endpoint = strings.TrimSpace(normalized.S3Endpoint)
		if normalized.S3Bucket == "" || normalized.S3Region == "" {
			return SourceConfig{}, errors.New("billing s3 blob source requires bucket and region")
		}
		if normalized.S3Endpoint != "" {
			if !normalized.S3AllowCustomEndpoint {
				return SourceConfig{}, errors.New("billing s3 blob source custom endpoint requires explicit enablement")
			}
			parsed, err := url.Parse(normalized.S3Endpoint)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				return SourceConfig{}, errors.New("billing s3 blob source endpoint must be an HTTP(S) origin")
			}
			if parsed.Path != "" && parsed.Path != "/" {
				return SourceConfig{}, errors.New("billing s3 blob source endpoint must be an HTTP(S) origin")
			}
			switch parsed.Scheme {
			case "https":
			case "http":
				if !normalized.S3AllowHTTP {
					return SourceConfig{}, errors.New("billing s3 blob source endpoint must use HTTPS unless HTTP is explicitly allowed")
				}
			default:
				return SourceConfig{}, errors.New("billing s3 blob source endpoint must be an HTTP(S) origin")
			}
			normalized.S3Endpoint = (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
		}
		return normalized, nil
	case SourceKindGCS:
		normalized.GCSBucket = strings.TrimSpace(normalized.GCSBucket)
		if normalized.GCSBucket == "" {
			return SourceConfig{}, errors.New("billing gcs blob source requires a bucket")
		}
		return normalized, nil
	case SourceKindAzure:
		normalized.AzureContainerURL = strings.TrimRight(strings.TrimSpace(normalized.AzureContainerURL), "/")
		if normalized.AzureContainerURL == "" {
			return SourceConfig{}, errors.New("billing azure blob source requires a container URL")
		}
		parsed, err := url.Parse(normalized.AzureContainerURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return SourceConfig{}, errors.New("billing azure blob source container URL must be absolute")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return SourceConfig{}, errors.New("billing azure blob source container URL must be an HTTP(S) container root without user info, query, or fragment")
		}
		switch parsed.Scheme {
		case "https":
		case "http":
			if !normalized.AzureAllowHTTP {
				return SourceConfig{}, errors.New("billing azure blob source container URL must use HTTPS unless HTTP is explicitly allowed")
			}
		default:
			return SourceConfig{}, errors.New("billing azure blob source container URL must use HTTPS unless HTTP is explicitly allowed")
		}
		cleanPath := path.Clean(parsed.Path)
		if cleanPath == "." || cleanPath == "/" || strings.HasSuffix(parsed.Path, "/.") || strings.HasSuffix(parsed.Path, "/..") {
			return SourceConfig{}, errors.New("billing azure blob source container URL must reference a container root path")
		}
		normalized.AzureContainerURL = (&url.URL{
			Scheme: parsed.Scheme,
			Host:   parsed.Host,
			Path:   cleanPath,
		}).String()
		return normalized, nil
	default:
		return SourceConfig{}, fmt.Errorf("unsupported billing blob source kind %q", normalized.Kind)
	}
}

func (configuredImport ConfiguredImport) Normalize() (ConfiguredImport, error) {
	normalized := configuredImport
	if normalized.TenantID == uuid.Nil {
		return ConfiguredImport{}, errors.New("billing import mapping tenant id is required")
	}
	provider, err := normalizeProvider(normalized.Provider)
	if err != nil {
		return ConfiguredImport{}, err
	}
	normalized.Provider = provider
	normalized.ExternalImportID = strings.TrimSpace(normalized.ExternalImportID)
	if normalized.ExternalImportID == "" {
		return ConfiguredImport{}, fmt.Errorf("billing import mapping for tenant %s provider %s is missing external import id",
			normalized.TenantID, normalized.Provider)
	}
	if err := validateExportObjectFormat(normalized.Format); err != nil {
		return ConfiguredImport{}, err
	}
	normalized.ObjectKey, err = normalizeBlobRelativeKey(normalized.ObjectKey)
	if err != nil {
		return ConfiguredImport{}, fmt.Errorf("billing import mapping for tenant %s %s/%s has an invalid object key: %w",
			normalized.TenantID, normalized.Provider, normalized.ExternalImportID, err)
	}
	normalized.ObjectVersion = strings.TrimSpace(normalized.ObjectVersion)
	normalized.ExecutionTargetIDs = make([]uuid.UUID, 0, len(configuredImport.ExecutionTargetIDs))
	seenTargetIDs := make(map[uuid.UUID]struct{}, len(configuredImport.ExecutionTargetIDs))
	for _, targetID := range configuredImport.ExecutionTargetIDs {
		if targetID == uuid.Nil {
			return ConfiguredImport{}, fmt.Errorf(
				"billing import mapping for tenant %s %s/%s contains an empty execution target id",
				normalized.TenantID, normalized.Provider, normalized.ExternalImportID,
			)
		}
		if _, duplicate := seenTargetIDs[targetID]; duplicate {
			return ConfiguredImport{}, fmt.Errorf(
				"billing import mapping for tenant %s %s/%s repeats execution target %s",
				normalized.TenantID, normalized.Provider, normalized.ExternalImportID, targetID,
			)
		}
		seenTargetIDs[targetID] = struct{}{}
		normalized.ExecutionTargetIDs = append(normalized.ExecutionTargetIDs, targetID)
	}
	slices.SortFunc(normalized.ExecutionTargetIDs, func(left, right uuid.UUID) int {
		return strings.Compare(left.String(), right.String())
	})
	if normalized.EstimateAfterImport && len(normalized.ExecutionTargetIDs) == 0 {
		return ConfiguredImport{}, fmt.Errorf(
			"billing import mapping for tenant %s %s/%s requires executionTargetIds when estimateAfterImport is enabled",
			normalized.TenantID, normalized.Provider, normalized.ExternalImportID,
		)
	}
	if !normalized.EstimateAfterImport && len(normalized.ExecutionTargetIDs) > 0 {
		return ConfiguredImport{}, fmt.Errorf(
			"billing import mapping for tenant %s %s/%s cannot configure executionTargetIds unless estimateAfterImport is enabled",
			normalized.TenantID, normalized.Provider, normalized.ExternalImportID,
		)
	}
	if normalized.ScheduleInterval < 0 {
		return ConfiguredImport{}, fmt.Errorf("billing import mapping for tenant %s %s/%s has a negative schedule interval",
			normalized.TenantID, normalized.Provider, normalized.ExternalImportID)
	}
	return normalized, nil
}

func OpenBlobSource(ctx context.Context, config SourceConfig) (BlobSource, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, err
	}
	switch normalized.Kind {
	case SourceKindDisabled:
		return nil, nil
	case SourceKindLocal:
		return NewLocalReadOnlyBlobSource(normalized.LocalBaseDir)
	case SourceKindS3:
		return NewS3ReadOnlyBlobSource(ctx, normalized)
	case SourceKindGCS:
		return NewGCSReadOnlyBlobSource(ctx, normalized)
	case SourceKindAzure:
		return NewAzureReadOnlyBlobSource(ctx, normalized)
	default:
		return nil, fmt.Errorf("unsupported billing blob source kind %q", normalized.Kind)
	}
}

func NewAdapterFromRuntime(ctx context.Context, config RuntimeConfig) (Adapter, []ConfiguredImport, io.Closer, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, nil, nil, err
	}
	if normalized.Source.Kind == SourceKindDisabled {
		return nil, nil, nil, nil
	}
	source, err := OpenBlobSource(ctx, normalized.Source)
	if err != nil {
		return nil, nil, nil, err
	}
	imports := make([]BlobInvoiceImportObject, 0, len(normalized.Imports))
	for _, configuredImport := range normalized.Imports {
		imports = append(imports, BlobInvoiceImportObject{
			TenantID:         configuredImport.TenantID,
			Provider:         configuredImport.Provider,
			ExternalImportID: configuredImport.ExternalImportID,
			Format:           configuredImport.Format,
			Object: BlobObjectRef{
				Key:     configuredImport.ObjectKey,
				Version: configuredImport.ObjectVersion,
			},
		})
	}
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source:         source,
		MaxObjectBytes: normalized.MaxObjectBytes,
		Imports:        imports,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	var closer io.Closer
	if closable, ok := source.(io.Closer); ok {
		closer = closable
	}
	return adapter, normalized.Imports, closer, nil
}

func configuredImportKey(tenantID uuid.UUID, provider, externalImportID string) string {
	return fixtureKey(tenantID, provider, externalImportID)
}

func normalizeBlobPrefix(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.HasPrefix(value, "/") {
		return "", errors.New("billing blob prefix must be relative")
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("billing blob prefix must not escape its root")
	}
	return strings.Trim(cleaned, "/"), nil
}

func normalizeBlobRelativeKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("object key is required")
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.HasPrefix(value, "/") {
		return "", errors.New("object key must be relative")
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("object key must not escape the configured prefix")
	}
	return cleaned, nil
}

func resolveBlobObjectKey(prefix, key string) (string, error) {
	normalizedKey, err := normalizeBlobRelativeKey(key)
	if err != nil {
		return "", err
	}
	normalizedPrefix, err := normalizeBlobPrefix(prefix)
	if err != nil {
		return "", err
	}
	if normalizedPrefix == "" {
		return normalizedKey, nil
	}
	return path.Join(normalizedPrefix, normalizedKey), nil
}

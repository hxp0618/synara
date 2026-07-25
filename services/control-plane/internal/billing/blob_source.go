package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const defaultBlobInvoiceMaxBytes int64 = 16 << 20

type ExportObjectFormat string

const (
	ExportObjectFormatAWSCURCSV      ExportObjectFormat = "aws-cur-csv"
	ExportObjectFormatGCPBillingJSON ExportObjectFormat = "gcp-cloud-billing-json"
	ExportObjectFormatAzureCostCSV   ExportObjectFormat = "azure-cost-normalized-csv"
	ExportObjectFormatAzureCostJSON  ExportObjectFormat = "azure-cost-normalized-json"
)

var ErrBlobInvoiceTooLarge = errors.New("billing invoice blob exceeds size limit")

type BlobObjectRef struct {
	Key     string
	Version string
}

type BlobObjectMetadata struct {
	SizeBytes int64
}

type BlobSource interface {
	Open(context.Context, BlobObjectRef) (io.ReadCloser, BlobObjectMetadata, error)
}

type BlobInvoiceImportObject struct {
	TenantID         uuid.UUID
	Provider         string
	ExternalImportID string
	Format           ExportObjectFormat
	Object           BlobObjectRef
}

type BlobInvoiceAdapterConfig struct {
	Source         BlobSource
	MaxObjectBytes int64
	Imports        []BlobInvoiceImportObject
}

type BlobInvoiceAdapter struct {
	source         BlobSource
	maxObjectBytes int64
	imports        map[string]BlobInvoiceImportObject
}

func NewBlobInvoiceAdapter(config BlobInvoiceAdapterConfig) (*BlobInvoiceAdapter, error) {
	if config.Source == nil {
		return nil, errors.New("billing blob source is required")
	}
	maxObjectBytes := config.MaxObjectBytes
	if maxObjectBytes <= 0 {
		maxObjectBytes = defaultBlobInvoiceMaxBytes
	}

	imports := make(map[string]BlobInvoiceImportObject, len(config.Imports))
	for _, rawImport := range config.Imports {
		if rawImport.TenantID == uuid.Nil {
			return nil, fmt.Errorf("billing import mapping for provider %q is missing tenant id", rawImport.Provider)
		}
		provider, err := normalizeProvider(rawImport.Provider)
		if err != nil {
			return nil, err
		}
		externalImportID := strings.TrimSpace(rawImport.ExternalImportID)
		if externalImportID == "" {
			return nil, fmt.Errorf("billing import mapping for provider %q is missing external import id", provider)
		}
		if err := validateExportObjectFormat(rawImport.Format); err != nil {
			return nil, err
		}
		if strings.TrimSpace(rawImport.Object.Key) == "" {
			return nil, fmt.Errorf("billing import mapping for %s/%s is missing an object key", provider, externalImportID)
		}
		rawImport.Provider = provider
		rawImport.ExternalImportID = externalImportID
		key := fixtureKey(rawImport.TenantID, provider, externalImportID)
		if _, exists := imports[key]; exists {
			return nil, fmt.Errorf("billing import mapping for tenant %s %s/%s is duplicated", rawImport.TenantID, provider, externalImportID)
		}
		imports[key] = rawImport
	}

	return &BlobInvoiceAdapter{
		source:         config.Source,
		maxObjectBytes: maxObjectBytes,
		imports:        imports,
	}, nil
}

func (a *BlobInvoiceAdapter) FetchActualInvoice(
	ctx context.Context,
	request ImportActualInvoiceRequest,
) (ImportedActualInvoice, error) {
	if a == nil {
		return ImportedActualInvoice{}, ErrInvoiceNotFound
	}
	provider, err := normalizeProvider(request.Provider)
	if err != nil {
		return ImportedActualInvoice{}, err
	}
	externalImportID := strings.TrimSpace(request.ExternalImportID)
	if externalImportID == "" {
		return ImportedActualInvoice{}, fmt.Errorf("billing external import id is required")
	}

	mapping, found := a.imports[fixtureKey(request.TenantID, provider, externalImportID)]
	if !found {
		return ImportedActualInvoice{}, ErrInvoiceNotFound
	}

	blob, err := readBlobWithLimit(ctx, a.source, mapping.Object, a.maxObjectBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ImportedActualInvoice{}, ErrInvoiceNotFound
		}
		return ImportedActualInvoice{}, err
	}
	return parseImportedInvoiceBlob(provider, externalImportID, mapping.Format, blob)
}

func readBlobWithLimit(
	ctx context.Context,
	source BlobSource,
	object BlobObjectRef,
	maxBytes int64,
) ([]byte, error) {
	if source == nil {
		return nil, errors.New("billing blob source is required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultBlobInvoiceMaxBytes
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reader, metadata, err := source.Open(ctx, object)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	if metadata.SizeBytes > maxBytes {
		return nil, fmt.Errorf("%w: %d > %d bytes", ErrBlobInvoiceTooLarge, metadata.SizeBytes, maxBytes)
	}

	limited := io.LimitReader(reader, maxBytes+1)
	blob, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(blob)) > maxBytes {
		return nil, fmt.Errorf("%w: read more than %d bytes", ErrBlobInvoiceTooLarge, maxBytes)
	}
	return blob, nil
}

func validateExportObjectFormat(format ExportObjectFormat) error {
	switch format {
	case ExportObjectFormatAWSCURCSV,
		ExportObjectFormatGCPBillingJSON,
		ExportObjectFormatAzureCostCSV,
		ExportObjectFormatAzureCostJSON:
		return nil
	default:
		return fmt.Errorf("unsupported billing export object format %q", format)
	}
}

type LocalReadOnlyBlobSource struct {
	baseDir string
	root    *os.Root
}

func NewLocalReadOnlyBlobSource(baseDir string) (*LocalReadOnlyBlobSource, error) {
	resolvedBaseDir, root, err := openVerifiedLocalBlobRoot(baseDir)
	if err != nil {
		return nil, err
	}
	return &LocalReadOnlyBlobSource{baseDir: resolvedBaseDir, root: root}, nil
}

func (s *LocalReadOnlyBlobSource) Open(
	_ context.Context,
	object BlobObjectRef,
) (io.ReadCloser, BlobObjectMetadata, error) {
	if s == nil {
		return nil, BlobObjectMetadata{}, errors.New("billing local blob source is nil")
	}
	if s.root == nil {
		return nil, BlobObjectMetadata{}, errors.New("billing local blob source root is unavailable")
	}
	key, segments, err := cleanLocalBlobRelativePath(object.Key)
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	parentRoot := s.root
	cleanup := func() {}
	if len(segments) > 1 {
		parentRelative := filepath.Join(segments[:len(segments)-1]...)
		parentRoot, cleanup, err = openVerifiedLocalBlobDirectory(s.root, parentRelative)
		if err != nil {
			return nil, BlobObjectMetadata{}, err
		}
		defer cleanup()
	}
	fileName := segments[len(segments)-1]
	before, err := parentRoot.Lstat(fileName)
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, BlobObjectMetadata{}, fmt.Errorf("billing blob object %q must not be a symlink", key)
	}
	if !before.Mode().IsRegular() {
		return nil, BlobObjectMetadata{}, fmt.Errorf("billing blob object %q must be a regular file", key)
	}
	file, err := parentRoot.Open(fileName)
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	opened, openedErr := file.Stat()
	after, afterErr := parentRoot.Lstat(fileName)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = file.Close()
		return nil, BlobObjectMetadata{}, fmt.Errorf("billing blob object %q changed while it was opened", key)
	}
	return file, BlobObjectMetadata{SizeBytes: opened.Size()}, nil
}

func (s *LocalReadOnlyBlobSource) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

func openVerifiedLocalBlobRoot(baseDir string) (string, *os.Root, error) {
	resolvedBaseDir, err := filepath.Abs(strings.TrimSpace(baseDir))
	if err != nil {
		return "", nil, err
	}
	before, err := os.Lstat(resolvedBaseDir)
	if err != nil {
		return "", nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return "", nil, fmt.Errorf("billing blob base directory %q is not a real directory", resolvedBaseDir)
	}
	root, err := os.OpenRoot(resolvedBaseDir)
	if err != nil {
		return "", nil, err
	}
	opened, openedErr := root.Stat(".")
	after, afterErr := os.Lstat(resolvedBaseDir)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = root.Close()
		return "", nil, errors.New("billing blob base directory changed while it was opened")
	}
	return resolvedBaseDir, root, nil
}

func cleanLocalBlobRelativePath(raw string) (string, []string, error) {
	key := filepath.FromSlash(strings.TrimSpace(raw))
	if key == "" {
		return "", nil, fmt.Errorf("billing blob object key is required")
	}
	if filepath.IsAbs(key) {
		return "", nil, fmt.Errorf("billing blob object key %q must be relative", raw)
	}
	cleaned := filepath.Clean(key)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", nil, fmt.Errorf("billing blob object key %q escapes the source root", raw)
	}
	segments := strings.Split(cleaned, string(os.PathSeparator))
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", nil, fmt.Errorf("billing blob object key %q escapes the source root", raw)
		}
	}
	return cleaned, segments, nil
}

func openVerifiedLocalBlobDirectory(root *os.Root, relative string) (*os.Root, func(), error) {
	segments := strings.Split(relative, string(os.PathSeparator))
	current := root
	opened := make([]*os.Root, 0, len(segments))
	cleanup := func() {
		for index := len(opened) - 1; index >= 0; index-- {
			_ = opened[index].Close()
		}
	}
	for _, segment := range segments {
		before, err := current.Lstat(segment)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			cleanup()
			return nil, nil, errors.New("billing blob path contains a symlink or non-directory component")
		}
		next, err := current.OpenRoot(segment)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		opened = append(opened, next)
		openedInfo, openedErr := next.Stat(".")
		after, afterErr := current.Lstat(segment)
		if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
			!os.SameFile(before, after) || !os.SameFile(after, openedInfo) {
			cleanup()
			return nil, nil, errors.New("billing blob directory changed while it was opened")
		}
		current = next
	}
	return current, cleanup, nil
}

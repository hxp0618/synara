package billing

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultBlobInvoiceMaxBytes  int64 = 16 << 20
	maxAWSCUR2ManifestChunks          = 128
	maxAWSCUR2CompressedBytes   int64 = 64 << 20
	maxAWSCUR2DecompressedBytes int64 = 256 << 20
	maxAWSCUR2Rows                    = 1_000_000
)

type ExportObjectFormat string

const (
	ExportObjectFormatAWSCURCSV       ExportObjectFormat = "aws-cur-csv"
	ExportObjectFormatAWSCUR2Manifest ExportObjectFormat = "aws-cur-2-manifest"
	ExportObjectFormatGCPBillingJSON  ExportObjectFormat = "gcp-cloud-billing-json"
	ExportObjectFormatAzureCostCSV    ExportObjectFormat = "azure-cost-normalized-csv"
	ExportObjectFormatAzureCostJSON   ExportObjectFormat = "azure-cost-normalized-json"
)

var ErrBlobInvoiceTooLarge = errors.New("provider cost invoice blob exceeds size limit")

type BlobObjectRef struct {
	Key     string
	Version string
}

type BlobObjectMetadata struct {
	SizeBytes    int64
	Version      string
	ETag         string
	LastModified time.Time
}

type BlobSource interface {
	Open(context.Context, BlobObjectRef) (io.ReadCloser, BlobObjectMetadata, error)
}

// BlobManifestObjectResolver captures the current immutable version of an
// object named by a manifest. Implementations must return a reference that can
// subsequently be passed to Open without consulting the mutable latest object.
type ResolvedBlobObject struct {
	Ref      BlobObjectRef
	Metadata BlobObjectMetadata
}

type BlobManifestObjectResolver interface {
	ResolveManifestObject(context.Context, string) (ResolvedBlobObject, error)
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
	CUR2Budget     CUR2ImportBudget
	Imports        []BlobInvoiceImportObject
}

type CUR2ImportBudget struct {
	MaxChunks            int
	MaxCompressedBytes   int64
	MaxDecompressedBytes int64
	MaxRows              int
}

type BlobInvoiceAdapter struct {
	source         BlobSource
	maxObjectBytes int64
	cur2Budget     CUR2ImportBudget
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
	cur2Budget, err := normalizeCUR2ImportBudget(config.CUR2Budget)
	if err != nil {
		return nil, err
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
		cur2Budget:     cur2Budget,
		imports:        imports,
	}, nil
}

func normalizeCUR2ImportBudget(raw CUR2ImportBudget) (CUR2ImportBudget, error) {
	defaults := CUR2ImportBudget{
		MaxChunks: maxAWSCUR2ManifestChunks, MaxCompressedBytes: maxAWSCUR2CompressedBytes,
		MaxDecompressedBytes: maxAWSCUR2DecompressedBytes, MaxRows: maxAWSCUR2Rows,
	}
	normalized := raw
	if normalized.MaxChunks == 0 {
		normalized.MaxChunks = defaults.MaxChunks
	}
	if normalized.MaxCompressedBytes == 0 {
		normalized.MaxCompressedBytes = defaults.MaxCompressedBytes
	}
	if normalized.MaxDecompressedBytes == 0 {
		normalized.MaxDecompressedBytes = defaults.MaxDecompressedBytes
	}
	if normalized.MaxRows == 0 {
		normalized.MaxRows = defaults.MaxRows
	}
	if normalized.MaxChunks < 1 || normalized.MaxChunks > defaults.MaxChunks ||
		normalized.MaxCompressedBytes < 1 || normalized.MaxCompressedBytes > defaults.MaxCompressedBytes ||
		normalized.MaxDecompressedBytes < 1 || normalized.MaxDecompressedBytes > defaults.MaxDecompressedBytes ||
		normalized.MaxRows < 1 || normalized.MaxRows > defaults.MaxRows {
		return CUR2ImportBudget{}, errors.New("aws cur 2.0 import budget must be positive and no greater than the safety defaults")
	}
	return normalized, nil
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

	blob, metadata, err := readBlobWithLimitAndMetadata(ctx, a.source, mapping.Object, a.maxObjectBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ImportedActualInvoice{}, ErrInvoiceNotFound
		}
		return ImportedActualInvoice{}, err
	}
	if mapping.Format == ExportObjectFormatAWSCUR2Manifest {
		return a.fetchAWSCUR2ManifestInvoice(ctx, provider, externalImportID, mapping.Object, metadata, blob)
	}
	return parseImportedInvoiceBlob(provider, externalImportID, mapping.Format, blob)
}

type awsCUR2Manifest struct {
	DataFiles []json.RawMessage `json:"dataFiles"`
}

type awsCUR2ManifestDataFile struct {
	FilePath string `json:"filePath"`
	Path     string `json:"path"`
}

func (a *BlobInvoiceAdapter) fetchAWSCUR2ManifestInvoice(
	ctx context.Context,
	provider string,
	externalImportID string,
	manifestRef BlobObjectRef,
	manifestMetadata BlobObjectMetadata,
	manifestBlob []byte,
) (ImportedActualInvoice, error) {
	if provider != "aws" {
		return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest requires provider aws, got %q", provider)
	}
	resolver, ok := a.source.(BlobManifestObjectResolver)
	if !ok {
		return ImportedActualInvoice{}, errors.New("aws cur 2.0 manifest source cannot capture immutable chunk versions")
	}
	chunkKeys, err := parseAWSCUR2Manifest(manifestBlob, a.cur2Budget.MaxChunks)
	if err != nil {
		return ImportedActualInvoice{}, err
	}

	if strings.TrimSpace(manifestRef.Version) == "" || manifestMetadata.LastModified.IsZero() {
		return ImportedActualInvoice{}, errors.New("aws cur 2.0 manifest immutable version metadata is incomplete")
	}
	chunkObjects := make([]ResolvedBlobObject, 0, len(chunkKeys))
	var resolvedCompressedBytes int64
	chunkDirectory := ""
	if err := addImportBudget(&resolvedCompressedBytes, manifestMetadata.SizeBytes, a.cur2Budget.MaxCompressedBytes, "compressed bytes"); err != nil {
		return ImportedActualInvoice{}, err
	}
	for _, key := range chunkKeys {
		object, resolveErr := resolver.ResolveManifestObject(ctx, key)
		if resolveErr != nil {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q: %w", key, resolveErr)
		}
		if strings.TrimSpace(object.Ref.Version) == "" || object.Metadata.LastModified.IsZero() || strings.TrimSpace(object.Metadata.ETag) == "" {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q resolved without an immutable version", key)
		}
		if object.Metadata.LastModified.After(manifestMetadata.LastModified) {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q was replaced after the pinned manifest", key)
		}
		if !sameAWSCUR2ExecutionBoundary(manifestRef.Key, object.Ref.Key) {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q is outside the manifest execution boundary", key)
		}
		if chunkDirectory == "" {
			chunkDirectory = path.Dir(object.Ref.Key)
		} else if path.Dir(object.Ref.Key) != chunkDirectory {
			return ImportedActualInvoice{}, errors.New("aws cur 2.0 manifest chunks span multiple execution directories")
		}
		if err := addImportBudget(&resolvedCompressedBytes, object.Metadata.SizeBytes, a.cur2Budget.MaxCompressedBytes, "compressed bytes"); err != nil {
			return ImportedActualInvoice{}, err
		}
		chunkObjects = append(chunkObjects, object)
	}

	var combinedRecords []map[string]string
	compressedBytes := int64(len(manifestBlob))
	decompressedBytes := int64(len(manifestBlob))
	if compressedBytes > a.cur2Budget.MaxCompressedBytes || decompressedBytes > a.cur2Budget.MaxDecompressedBytes {
		return ImportedActualInvoice{}, errors.New("aws cur 2.0 manifest exceeds total import byte budget")
	}
	provenanceChunks := make([]InvoiceSourceObject, 0, len(chunkObjects))
	for index, object := range chunkObjects {
		compressed, openedMetadata, readErr := readBlobWithLimitAndMetadata(ctx, a.source, object.Ref, a.maxObjectBytes)
		if readErr != nil {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q: %w", chunkKeys[index], readErr)
		}
		if !sameBlobObjectMetadata(object.Metadata, openedMetadata) {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q metadata changed before versioned read", chunkKeys[index])
		}
		if err := addImportBudget(&compressedBytes, int64(len(compressed)), a.cur2Budget.MaxCompressedBytes, "compressed bytes"); err != nil {
			return ImportedActualInvoice{}, err
		}
		csvBlob, decompressErr := gunzipBlobWithLimit(compressed, a.maxObjectBytes)
		if decompressErr != nil {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q: %w", chunkKeys[index], decompressErr)
		}
		if err := addImportBudget(&decompressedBytes, int64(len(csvBlob)), a.cur2Budget.MaxDecompressedBytes, "decompressed bytes"); err != nil {
			return ImportedActualInvoice{}, err
		}
		chunkRecords, parseErr := parseCSVRecords(csvBlob)
		if parseErr != nil {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 manifest chunk %q: %w", chunkKeys[index], parseErr)
		}
		if len(chunkRecords) > a.cur2Budget.MaxRows-len(combinedRecords) {
			return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 import exceeds row limit %d", a.cur2Budget.MaxRows)
		}
		combinedRecords = append(combinedRecords, chunkRecords...)
		provenanceChunks = append(provenanceChunks, invoiceSourceObject(object.Ref, openedMetadata, compressed))
	}
	combined, err := parseAWSCURRecords(combinedRecords, true)
	if err != nil {
		return ImportedActualInvoice{}, err
	}
	invoice, err := buildImportedInvoice(provider, externalImportID, combined)
	if err != nil {
		return ImportedActualInvoice{}, err
	}
	provenance := InvoiceSourceProvenance{
		Format:   ExportObjectFormatAWSCUR2Manifest,
		Manifest: invoiceSourceObject(manifestRef, manifestMetadata, manifestBlob),
		Chunks:   provenanceChunks, CompressedBytes: compressedBytes, DecompressedBytes: decompressedBytes,
		RowCount: len(combinedRecords), FilteredAdjustmentCount: combined.FilteredAdjustmentCount,
	}
	provenance.FilteredAdjustmentAmountMicros, err = decimalRatToRoundedMicros(combined.FilteredAdjustmentAmount)
	if err != nil {
		return ImportedActualInvoice{}, fmt.Errorf("aws cur 2.0 filtered adjustment total: %w", err)
	}
	provenance.BundleChecksum = checksumInvoiceSourceBundle(provenance)
	invoice.SourceProvenance = &provenance
	return invoice, nil
}

func sameAWSCUR2ExecutionBoundary(manifestKey, chunkKey string) bool {
	manifestRoot, manifestPartition, manifestExecution, manifestOK := awsCUR2DeliveryBoundary(manifestKey, "metadata")
	chunkRoot, chunkPartition, chunkExecution, chunkOK := awsCUR2DeliveryBoundary(chunkKey, "data")
	return manifestOK && chunkOK && manifestRoot == chunkRoot && manifestPartition == chunkPartition &&
		manifestExecution == chunkExecution
}

func awsCUR2DeliveryBoundary(key, directoryKind string) (root, partition, execution string, ok bool) {
	cleaned, err := normalizeBlobRelativeKey(key)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(path.Clean(cleaned), "/")
	// Exactly root/{metadata|data}/partition/execution/file. Root may contain
	// multiple path segments but may not be empty. Parse from the fixed tail so
	// a root segment named "data" or "metadata" cannot shift the boundary.
	if len(parts) < 5 {
		return "", "", "", false
	}
	directoryIndex := len(parts) - 4
	if directoryIndex < 1 || parts[directoryIndex] != directoryKind {
		return "", "", "", false
	}
	partition = parts[directoryIndex+1]
	execution = parts[directoryIndex+2]
	if !strings.HasPrefix(partition, "BILLING_PERIOD=") || strings.TrimPrefix(partition, "BILLING_PERIOD=") == "" ||
		execution == "" || execution == "." || execution == ".." {
		return "", "", "", false
	}
	return strings.Join(parts[:directoryIndex], "/"), partition, execution, true
}

func addImportBudget(total *int64, addition, limit int64, name string) error {
	if addition < 0 || *total > limit-addition {
		return fmt.Errorf("aws cur 2.0 import exceeds %s limit %d", name, limit)
	}
	*total += addition
	return nil
}

func sameBlobObjectMetadata(left, right BlobObjectMetadata) bool {
	return left.SizeBytes == right.SizeBytes && left.Version == right.Version && left.ETag == right.ETag &&
		left.LastModified.Equal(right.LastModified)
}

func invoiceSourceObject(ref BlobObjectRef, metadata BlobObjectMetadata, blob []byte) InvoiceSourceObject {
	digest := sha256.Sum256(blob)
	return InvoiceSourceObject{
		Key: ref.Key, Version: ref.Version, ETag: metadata.ETag, LastModified: metadata.LastModified.UTC(),
		SizeBytes: metadata.SizeBytes, SHA256: hex.EncodeToString(digest[:]),
	}
}

func checksumInvoiceSourceBundle(provenance InvoiceSourceProvenance) string {
	hasher := sha256.New()
	writeSourceObject := func(object InvoiceSourceObject) {
		writeChecksumPart(hasher, object.Key)
		writeChecksumPart(hasher, object.Version)
		writeChecksumPart(hasher, object.ETag)
		writeChecksumPart(hasher, object.LastModified.UTC().Format(time.RFC3339Nano))
		writeChecksumPart(hasher, fmt.Sprintf("%d", object.SizeBytes))
		writeChecksumPart(hasher, object.SHA256)
	}
	writeSourceObject(provenance.Manifest)
	for _, chunk := range provenance.Chunks {
		writeSourceObject(chunk)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func parseAWSCUR2Manifest(blob []byte, maxChunks int) ([]string, error) {
	var manifest awsCUR2Manifest
	decoder := json.NewDecoder(bytes.NewReader(blob))
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("aws cur 2.0 manifest: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("contains multiple json values")
		}
		return nil, fmt.Errorf("aws cur 2.0 manifest: %w", err)
	}
	keys := make([]string, 0, len(manifest.DataFiles))
	for _, rawFile := range manifest.DataFiles {
		var key string
		if err := json.Unmarshal(rawFile, &key); err != nil {
			var file awsCUR2ManifestDataFile
			if objectErr := json.Unmarshal(rawFile, &file); objectErr != nil {
				return nil, fmt.Errorf("aws cur 2.0 manifest dataFiles entry must be a path string or object")
			}
			key = firstNonEmpty(file.FilePath, file.Path)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("aws cur 2.0 manifest contains no data files")
	}
	if len(keys) > maxChunks {
		return nil, fmt.Errorf("aws cur 2.0 manifest contains %d data files, limit is %d", len(keys), maxChunks)
	}
	for index := range keys {
		keys[index] = strings.TrimSpace(keys[index])
		if keys[index] == "" {
			return nil, errors.New("aws cur 2.0 manifest contains an empty data file path")
		}
		lower := strings.ToLower(keys[index])
		switch {
		case strings.HasSuffix(lower, ".csv.gz"):
		case strings.HasSuffix(lower, ".parquet") || strings.HasSuffix(lower, ".snappy.parquet"):
			return nil, fmt.Errorf("aws cur 2.0 parquet chunk %q is not supported", keys[index])
		default:
			return nil, fmt.Errorf("aws cur 2.0 chunk %q must be gzip csv", keys[index])
		}
	}
	slices.Sort(keys)
	for index := 1; index < len(keys); index++ {
		if keys[index] == keys[index-1] {
			return nil, fmt.Errorf("aws cur 2.0 manifest repeats data file %q", keys[index])
		}
	}
	return keys, nil
}

func gunzipBlobWithLimit(blob []byte, maxBytes int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("invalid gzip csv: %w", err)
	}
	defer reader.Close()
	limited := io.LimitReader(reader, maxBytes+1)
	decompressed, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read gzip csv: %w", err)
	}
	if int64(len(decompressed)) > maxBytes {
		return nil, fmt.Errorf("%w: decompressed chunk exceeds %d bytes", ErrBlobInvoiceTooLarge, maxBytes)
	}
	return decompressed, nil
}

func readBlobWithLimitAndMetadata(
	ctx context.Context,
	source BlobSource,
	object BlobObjectRef,
	maxBytes int64,
) ([]byte, BlobObjectMetadata, error) {
	if source == nil {
		return nil, BlobObjectMetadata{}, errors.New("billing blob source is required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultBlobInvoiceMaxBytes
	}
	if err := ctx.Err(); err != nil {
		return nil, BlobObjectMetadata{}, err
	}

	reader, metadata, err := source.Open(ctx, object)
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	defer reader.Close()

	if metadata.SizeBytes > maxBytes {
		return nil, BlobObjectMetadata{}, fmt.Errorf("%w: %d > %d bytes", ErrBlobInvoiceTooLarge, metadata.SizeBytes, maxBytes)
	}

	limited := io.LimitReader(reader, maxBytes+1)
	blob, err := io.ReadAll(limited)
	if err != nil {
		return nil, BlobObjectMetadata{}, err
	}
	if int64(len(blob)) > maxBytes {
		return nil, BlobObjectMetadata{}, fmt.Errorf("%w: read more than %d bytes", ErrBlobInvoiceTooLarge, maxBytes)
	}
	return blob, metadata, nil
}

func validateExportObjectFormat(format ExportObjectFormat) error {
	switch format {
	case ExportObjectFormatAWSCURCSV,
		ExportObjectFormatAWSCUR2Manifest,
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
	if len(segments) > 1 {
		parentRelative := filepath.Join(segments[:len(segments)-1]...)
		var cleanup func()
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

package billing

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSourceConfigNormalizeRejectsUnsafeS3CustomEndpoints(t *testing.T) {
	tests := []struct {
		name string
		cfg  SourceConfig
		want string
	}{
		{
			name: "custom endpoint requires explicit allow",
			cfg:  SourceConfig{Kind: SourceKindS3, S3Bucket: "billing", S3Region: "us-east-1", S3Endpoint: "https://billing.example.com"},
			want: "explicit enablement",
		},
		{
			name: "http requires explicit allow",
			cfg: SourceConfig{
				Kind: SourceKindS3, S3Bucket: "billing", S3Region: "us-east-1",
				S3Endpoint: "http://billing.example.com", S3AllowCustomEndpoint: true,
			},
			want: "must use HTTPS",
		},
		{
			name: "rejects user info",
			cfg: SourceConfig{
				Kind: SourceKindS3, S3Bucket: "billing", S3Region: "us-east-1",
				S3Endpoint: "https://user@example.com", S3AllowCustomEndpoint: true,
			},
			want: "HTTP(S) origin",
		},
		{
			name: "rejects query",
			cfg: SourceConfig{
				Kind: SourceKindS3, S3Bucket: "billing", S3Region: "us-east-1",
				S3Endpoint: "https://billing.example.com?x=1", S3AllowCustomEndpoint: true,
			},
			want: "HTTP(S) origin",
		},
		{
			name: "rejects path",
			cfg: SourceConfig{
				Kind: SourceKindS3, S3Bucket: "billing", S3Region: "us-east-1",
				S3Endpoint: "https://billing.example.com/minio", S3AllowCustomEndpoint: true,
			},
			want: "HTTP(S) origin",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.cfg.Normalize()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestSourceConfigNormalizeAllowsExplicitSafeS3CustomEndpoint(t *testing.T) {
	normalized, err := (SourceConfig{
		Kind: SourceKindS3, S3Bucket: "billing", S3Region: "us-east-1",
		S3Endpoint: "https://billing.example.com/", S3AllowCustomEndpoint: true,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.S3Endpoint != "https://billing.example.com" {
		t.Fatalf("normalized endpoint = %q, want https://billing.example.com", normalized.S3Endpoint)
	}
}

func TestSourceConfigNormalizeRejectsUnsafeAzureContainerURLs(t *testing.T) {
	tests := []struct {
		name string
		cfg  SourceConfig
		want string
	}{
		{
			name: "http requires explicit allow",
			cfg:  SourceConfig{Kind: SourceKindAzure, AzureContainerURL: "http://127.0.0.1:10000/devstoreaccount1/invoices"},
			want: "must use HTTPS",
		},
		{
			name: "rejects non-http scheme even when allow is enabled",
			cfg:  SourceConfig{Kind: SourceKindAzure, AzureContainerURL: "ftp://example.com/container", AzureAllowHTTP: true},
			want: "must use HTTPS",
		},
		{
			name: "rejects user info",
			cfg:  SourceConfig{Kind: SourceKindAzure, AzureContainerURL: "https://user@example.blob.core.windows.net/container"},
			want: "without user info, query, or fragment",
		},
		{
			name: "rejects query",
			cfg:  SourceConfig{Kind: SourceKindAzure, AzureContainerURL: "https://example.blob.core.windows.net/container?sig=1"},
			want: "without user info, query, or fragment",
		},
		{
			name: "rejects fragment",
			cfg:  SourceConfig{Kind: SourceKindAzure, AzureContainerURL: "https://example.blob.core.windows.net/container#frag"},
			want: "without user info, query, or fragment",
		},
		{
			name: "rejects missing container path",
			cfg:  SourceConfig{Kind: SourceKindAzure, AzureContainerURL: "https://example.blob.core.windows.net"},
			want: "container root path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.cfg.Normalize()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestSourceConfigNormalizeAllowsSafeAzureContainerRoot(t *testing.T) {
	normalized, err := (SourceConfig{
		Kind:              SourceKindAzure,
		AzureContainerURL: "https://example.blob.core.windows.net/devstoreaccount1/invoices/",
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.AzureContainerURL != "https://example.blob.core.windows.net/devstoreaccount1/invoices" {
		t.Fatalf("normalized Azure container URL = %q", normalized.AzureContainerURL)
	}

	normalized, err = (SourceConfig{
		Kind:              SourceKindAzure,
		AzureContainerURL: "http://127.0.0.1:10000/devstoreaccount1/invoices",
		AzureAllowHTTP:    true,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.AzureContainerURL != "http://127.0.0.1:10000/devstoreaccount1/invoices" {
		t.Fatalf("normalized Azure emulator URL = %q", normalized.AzureContainerURL)
	}
}

func TestRuntimeConfigRequiresEstimateSweeper(t *testing.T) {
	if !(RuntimeConfig{
		Imports: []ConfiguredImport{{ExternalImportID: "hourly", EstimateAfterImport: true}},
	}).RequiresEstimateSweeper() {
		t.Fatal("expected estimate sweeper requirement")
	}
	if (RuntimeConfig{
		Imports: []ConfiguredImport{{ExternalImportID: "manual"}},
	}).RequiresEstimateSweeper() {
		t.Fatal("did not expect estimate sweeper requirement")
	}
}

func TestRuntimeConfigRequiresExplicitNonConflictingEstimateTargets(t *testing.T) {
	tenantID := uuid.New()
	targetA := uuid.New()
	targetB := uuid.New()
	base := RuntimeConfig{
		Source: SourceConfig{Kind: SourceKindLocal, LocalBaseDir: t.TempDir()},
		Imports: []ConfiguredImport{{
			TenantID: tenantID, Provider: "aws", ExternalImportID: "july",
			Format: ExportObjectFormatAWSCURCSV, ObjectKey: "july.csv",
			EstimateAfterImport: true, ExecutionTargetIDs: []uuid.UUID{targetB, targetA},
		}},
	}
	normalized, err := base.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Imports[0].ExecutionTargetIDs) != 2 ||
		normalized.Imports[0].ExecutionTargetIDs[0].String() > normalized.Imports[0].ExecutionTargetIDs[1].String() {
		t.Fatalf("estimate targets were not normalized: %#v", normalized.Imports[0].ExecutionTargetIDs)
	}

	missingTargets := base
	missingTargets.Imports = append([]ConfiguredImport(nil), base.Imports...)
	missingTargets.Imports[0].ExecutionTargetIDs = nil
	if _, err := missingTargets.Normalize(); err == nil || !strings.Contains(err.Error(), "requires executionTargetIds") {
		t.Fatalf("missing estimate targets error = %v", err)
	}

	unusedTargets := base
	unusedTargets.Imports = append([]ConfiguredImport(nil), base.Imports...)
	unusedTargets.Imports[0].EstimateAfterImport = false
	if _, err := unusedTargets.Normalize(); err == nil || !strings.Contains(err.Error(), "cannot configure executionTargetIds") {
		t.Fatalf("unused estimate targets error = %v", err)
	}

	conflictingProviders := base
	conflictingProviders.Imports = append(append([]ConfiguredImport(nil), base.Imports...), ConfiguredImport{
		TenantID: tenantID, Provider: "gcp", ExternalImportID: "july-gcp",
		Format: ExportObjectFormatGCPBillingJSON, ObjectKey: "july-gcp.json",
		EstimateAfterImport: true, ExecutionTargetIDs: []uuid.UUID{targetA},
	})
	if _, err := conflictingProviders.Normalize(); err == nil || !strings.Contains(err.Error(), "assigned to both aws and gcp") {
		t.Fatalf("conflicting estimate provider error = %v", err)
	}
}

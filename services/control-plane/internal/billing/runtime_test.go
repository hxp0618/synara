package billing

import (
	"strings"
	"testing"
	"time"

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

func TestRuntimeConfigNormalizesExplicitSharedAllocationPeriods(t *testing.T) {
	targetID := uuid.New()
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	config := RuntimeConfig{SharedAllocations: []ConfiguredSharedAllocation{
		{
			ExecutionTargetID: targetID, Provider: " AWS ", CurrencyCode: " usd ",
			BillingPeriodStartAt: base.Add(24 * time.Hour), BillingPeriodEndAt: base.Add(48 * time.Hour),
			SettlementDelay: 2 * time.Hour, ScheduleInterval: 30 * time.Minute,
		},
		{
			ExecutionTargetID: targetID, Provider: "aws", CurrencyCode: "USD",
			BillingPeriodStartAt: base, BillingPeriodEndAt: base.Add(24 * time.Hour),
			SettlementDelay: time.Hour, ScheduleInterval: 15 * time.Minute,
		},
	}}
	normalized, err := config.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.SharedAllocations) != 2 ||
		!normalized.SharedAllocations[0].BillingPeriodStartAt.Equal(base) ||
		normalized.SharedAllocations[0].Provider != "aws" || normalized.SharedAllocations[0].CurrencyCode != "USD" {
		t.Fatalf("unexpected normalized shared allocations: %#v", normalized.SharedAllocations)
	}
	if normalized.MinimumSharedAllocationScheduleInterval() != 15*time.Minute ||
		!normalized.HasScheduledSharedAllocations() {
		t.Fatalf("unexpected shared allocation scheduler interval: %s", normalized.MinimumSharedAllocationScheduleInterval())
	}

	duplicate := config
	duplicate.SharedAllocations = append(
		append([]ConfiguredSharedAllocation(nil), config.SharedAllocations...),
		config.SharedAllocations[0],
	)
	if _, err := duplicate.Normalize(); err == nil || !strings.Contains(err.Error(), "is duplicated") {
		t.Fatalf("duplicate shared allocation error = %v", err)
	}

	overlap := config
	overlap.SharedAllocations = append([]ConfiguredSharedAllocation(nil), config.SharedAllocations...)
	overlap.SharedAllocations[1].BillingPeriodEndAt = base.Add(25 * time.Hour)
	if _, err := overlap.Normalize(); err == nil || !strings.Contains(err.Error(), "overlapping periods") {
		t.Fatalf("overlapping shared allocation error = %v", err)
	}
}

func TestConfiguredSharedAllocationRejectsUnsafeScheduleBounds(t *testing.T) {
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	valid := ConfiguredSharedAllocation{
		ExecutionTargetID: uuid.New(), Provider: "aws", CurrencyCode: "USD",
		BillingPeriodStartAt: base, BillingPeriodEndAt: base.Add(24 * time.Hour),
		SettlementDelay: time.Hour, ScheduleInterval: time.Hour,
	}
	for _, test := range []struct {
		name   string
		mutate func(*ConfiguredSharedAllocation)
		want   string
	}{
		{name: "missing target", mutate: func(item *ConfiguredSharedAllocation) { item.ExecutionTargetID = uuid.Nil }, want: "execution Target id is required"},
		{name: "open period", mutate: func(item *ConfiguredSharedAllocation) { item.BillingPeriodEndAt = item.BillingPeriodStartAt }, want: "billingPeriodEndAt must be after billingPeriodStartAt"},
		{name: "period too large", mutate: func(item *ConfiguredSharedAllocation) {
			item.BillingPeriodEndAt = item.BillingPeriodStartAt.Add(367 * 24 * time.Hour)
		}, want: "must not exceed 366 days"},
		{name: "missing settlement", mutate: func(item *ConfiguredSharedAllocation) { item.SettlementDelay = 0 }, want: "settlement delay must be between"},
		{name: "excessive settlement", mutate: func(item *ConfiguredSharedAllocation) { item.SettlementDelay = 91 * 24 * time.Hour }, want: "settlement delay must be between"},
		{name: "tight retry loop", mutate: func(item *ConfiguredSharedAllocation) { item.ScheduleInterval = time.Second }, want: "schedule interval must be at least 1m"},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := valid
			test.mutate(&item)
			_, err := item.Normalize()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestConfiguredImportNormalizeEnforcesProviderFormatBinding(t *testing.T) {
	tenantID := uuid.New()
	for _, test := range []struct {
		name     string
		provider string
		format   ExportObjectFormat
	}{
		{name: "aws cur csv", provider: "aws", format: ExportObjectFormatAWSCURCSV},
		{name: "aws cur2 manifest", provider: "aws", format: ExportObjectFormatAWSCUR2Manifest},
		{name: "gcp normalized json", provider: "gcp", format: ExportObjectFormatGCPBillingJSON},
		{name: "azure normalized csv", provider: "azure", format: ExportObjectFormatAzureCostCSV},
		{name: "azure normalized json", provider: "azure", format: ExportObjectFormatAzureCostJSON},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (ConfiguredImport{
				TenantID: tenantID, Provider: test.provider, ExternalImportID: "july",
				Format: test.format, ObjectKey: "billing/export",
			}).Normalize()
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, test := range []struct {
		name     string
		provider string
		format   ExportObjectFormat
		want     string
	}{
		{name: "gcp label on aws cur", provider: "gcp", format: ExportObjectFormatAWSCURCSV, want: "requires provider aws, not gcp"},
		{name: "azure label on cur2", provider: "azure", format: ExportObjectFormatAWSCUR2Manifest, want: "requires provider aws, not azure"},
		{name: "aws label on gcp", provider: "aws", format: ExportObjectFormatGCPBillingJSON, want: "requires provider gcp, not aws"},
		{name: "gcp label on azure csv", provider: "gcp", format: ExportObjectFormatAzureCostCSV, want: "requires provider azure, not gcp"},
		{name: "aws label on azure json", provider: "aws", format: ExportObjectFormatAzureCostJSON, want: "requires provider azure, not aws"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (ConfiguredImport{
				TenantID: tenantID, Provider: test.provider, ExternalImportID: "july",
				Format: test.format, ObjectKey: "billing/export",
			}).Normalize()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("provider/format mismatch error = %v, want %q", err, test.want)
			}
		})
	}
}

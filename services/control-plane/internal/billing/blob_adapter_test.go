package billing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBlobInvoiceAdapterFetchActualInvoiceFixtures(t *testing.T) {
	t.Parallel()

	source, err := NewLocalReadOnlyBlobSource(filepath.Join("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	tenantID := uuid.New()
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source: source,
		Imports: []BlobInvoiceImportObject{
			{
				TenantID:         tenantID,
				Provider:         "aws",
				ExternalImportID: "aws-july-2026",
				Format:           ExportObjectFormatAWSCURCSV,
				Object:           BlobObjectRef{Key: "aws_cur_fixture.csv"},
			},
			{
				TenantID:         tenantID,
				Provider:         "gcp",
				ExternalImportID: "gcp-july-2026",
				Format:           ExportObjectFormatGCPBillingJSON,
				Object:           BlobObjectRef{Key: "gcp_billing_fixture.json"},
			},
			{
				TenantID:         tenantID,
				Provider:         "azure",
				ExternalImportID: "azure-csv-july-2026",
				Format:           ExportObjectFormatAzureCostCSV,
				Object:           BlobObjectRef{Key: "azure_cost_fixture.csv"},
			},
			{
				TenantID:         tenantID,
				Provider:         "azure",
				ExternalImportID: "azure-json-july-2026",
				Format:           ExportObjectFormatAzureCostJSON,
				Object:           BlobObjectRef{Key: "azure_cost_fixture.json"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	type expectation struct {
		provider         string
		externalImportID string
		startAt          time.Time
		endAt            time.Time
		currencyCode     string
		amountByKind     map[string]int64
		resourceByKind   map[string]string
		aggregatedKinds  map[string]bool
	}
	for _, tc := range []expectation{
		{
			provider:         "aws",
			externalImportID: "aws-july-2026",
			startAt:          time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			endAt:            time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
			currencyCode:     "USD",
			amountByKind: map[string]int64{
				ChargeKindCPU:    500_001,
				ChargeKindMemory: 1_500_000,
				ChargeKindPod:    125_000,
			},
			resourceByKind: map[string]string{
				ChargeKindCPU:    "kubernetes:cluster-a:us-east-1:default:worker-a:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				ChargeKindMemory: "kubernetes:cluster-a:us-east-1:default:worker-a:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				ChargeKindPod:    "aws:123456789012:us-east-1:arn:aws:eks:us-east-1:123456789012:pod/cluster-a/default/worker-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			},
			aggregatedKinds: map[string]bool{
				ChargeKindCPU: true,
			},
		},
		{
			provider:         "gcp",
			externalImportID: "gcp-july-2026",
			startAt:          time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			endAt:            time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
			currencyCode:     "USD",
			amountByKind: map[string]int64{
				ChargeKindEphemeralStorage: 666_667,
				ChargeKindRequest:          2_000_000,
			},
			resourceByKind: map[string]string{
				ChargeKindEphemeralStorage: "kubernetes:cluster-b:us-central1:default:worker-b:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				ChargeKindRequest:          "gcp:project-b:us-central1://run.googleapis.com/projects/project-b/locations/us-central1/services/gateway",
			},
			aggregatedKinds: map[string]bool{
				ChargeKindEphemeralStorage: true,
			},
		},
		{
			provider:         "azure",
			externalImportID: "azure-csv-july-2026",
			startAt:          time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			endAt:            time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
			currencyCode:     "USD",
			amountByKind: map[string]int64{
				ChargeKindCPU:    750_000,
				ChargeKindMemory: 500_000,
			},
			resourceByKind: map[string]string{
				ChargeKindCPU:    "kubernetes:cluster-c:eastus:prod:worker-c:cccccccc-cccc-4ccc-8ccc-cccccccccccc",
				ChargeKindMemory: "kubernetes:cluster-c:eastus:prod:worker-c:cccccccc-cccc-4ccc-8ccc-cccccccccccc",
			},
		},
		{
			provider:         "azure",
			externalImportID: "azure-json-july-2026",
			startAt:          time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			endAt:            time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
			currencyCode:     "USD",
			amountByKind: map[string]int64{
				ChargeKindRequest:          3_000_000,
				ChargeKindEphemeralStorage: 1_125_000,
			},
			resourceByKind: map[string]string{
				ChargeKindRequest:          "azure:sub-456:westus3:/subscriptions/sub-456/resourcegroups/rg/providers/microsoft.apimanagement/service/api-gateway",
				ChargeKindEphemeralStorage: "azure:sub-456:westus3:/subscriptions/sub-456/resourcegroups/rg/providers/microsoft.compute/disks/cache-disk",
			},
		},
	} {
		tc := tc
		t.Run(tc.provider+"-"+tc.externalImportID, func(t *testing.T) {
			t.Parallel()

			invoice, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
				TenantID:         tenantID,
				Provider:         tc.provider,
				ExternalImportID: tc.externalImportID,
			})
			if err != nil {
				t.Fatal(err)
			}

			if invoice.ExternalImportID != tc.externalImportID {
				t.Fatalf("external import id = %q, want %q", invoice.ExternalImportID, tc.externalImportID)
			}
			if !invoice.BillingPeriodStartAt.Equal(tc.startAt) || !invoice.BillingPeriodEndAt.Equal(tc.endAt) {
				t.Fatalf("billing period = [%s,%s), want [%s,%s)",
					invoice.BillingPeriodStartAt, invoice.BillingPeriodEndAt, tc.startAt, tc.endAt)
			}
			if invoice.CurrencyCode != tc.currencyCode {
				t.Fatalf("currency = %q, want %q", invoice.CurrencyCode, tc.currencyCode)
			}
			if len(invoice.Lines) != len(tc.amountByKind) {
				t.Fatalf("line count = %d, want %d", len(invoice.Lines), len(tc.amountByKind))
			}

			linesByKind := make(map[string]ImportedActualInvoiceLine, len(invoice.Lines))
			for _, line := range invoice.Lines {
				linesByKind[line.ChargeKind] = line
			}

			for chargeKind, wantAmount := range tc.amountByKind {
				line, ok := linesByKind[chargeKind]
				if !ok {
					t.Fatalf("missing charge kind %q in %#v", chargeKind, invoice.Lines)
				}
				if line.AmountMicros != wantAmount {
					t.Fatalf("%s amount micros = %d, want %d", chargeKind, line.AmountMicros, wantAmount)
				}
				if line.ResourceCorrelationKey != tc.resourceByKind[chargeKind] {
					t.Fatalf("%s resource key = %q, want %q", chargeKind, line.ResourceCorrelationKey, tc.resourceByKind[chargeKind])
				}
				if tc.aggregatedKinds[chargeKind] && !strings.HasPrefix(line.ExternalLineID, "agg-") {
					t.Fatalf("%s external line id = %q, want aggregated id", chargeKind, line.ExternalLineID)
				}
				if !tc.aggregatedKinds[chargeKind] && strings.HasPrefix(line.ExternalLineID, "agg-") {
					t.Fatalf("%s external line id = %q, want provider row id", chargeKind, line.ExternalLineID)
				}
			}
		})
	}
}

func TestBlobInvoiceAdapterRejectsStrictInputFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "mixed-currency",
			content: "ExternalLineId,BillingPeriodStartAt,BillingPeriodEndAt,CurrencyCode,CostInBillingCurrency,ChargeKind,ResourceId,Region\n" +
				"line-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,cpu,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/a,eastus\n" +
				"line-2,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,EUR,1,memory,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/b,eastus\n",
			wantErr: "mixes currencies",
		},
		{
			name: "mixed-period",
			content: "ExternalLineId,BillingPeriodStartAt,BillingPeriodEndAt,CurrencyCode,CostInBillingCurrency,ChargeKind,ResourceId,Region\n" +
				"line-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,cpu,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/a,eastus\n" +
				"line-2,2026-07-15T00:00:00Z,2026-08-15T00:00:00Z,USD,1,memory,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/b,eastus\n",
			wantErr: "mixes billing periods",
		},
		{
			name: "duplicate-line-id",
			content: "ExternalLineId,BillingPeriodStartAt,BillingPeriodEndAt,CurrencyCode,CostInBillingCurrency,ChargeKind,ResourceId,Region\n" +
				"line-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,cpu,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/a,eastus\n" +
				"line-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,memory,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/b,eastus\n",
			wantErr: "repeats external line id",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tenantID := uuid.New()
			source, adapter := newTempBlobAdapter(
				t, tenantID, "azure", "fixture", tc.content, "fixture.csv", 0, ExportObjectFormatAzureCostCSV,
			)
			_ = source
			_, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
				TenantID:         tenantID,
				Provider:         "azure",
				ExternalImportID: "fixture",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestBlobInvoiceAdapterRejectsOversizedBlob(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	_, adapter := newTempBlobAdapter(t,
		tenantID,
		"azure",
		"fixture",
		"ExternalLineId,BillingPeriodStartAt,BillingPeriodEndAt,CurrencyCode,CostInBillingCurrency,ChargeKind,ResourceId,Region\n"+
			"line-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,cpu,/subscriptions/SUB-123/resourceGroups/rg/providers/Microsoft.Compute/disks/a,eastus\n",
		"fixture.csv",
		32,
		ExportObjectFormatAzureCostCSV,
	)

	_, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID:         tenantID,
		Provider:         "azure",
		ExternalImportID: "fixture",
	})
	if !errors.Is(err, ErrBlobInvoiceTooLarge) {
		t.Fatalf("error = %v, want ErrBlobInvoiceTooLarge", err)
	}
}

func TestLocalReadOnlyBlobSourceRejectsTraversal(t *testing.T) {
	t.Parallel()

	source, err := NewLocalReadOnlyBlobSource(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = source.Open(context.Background(), BlobObjectRef{Key: "../escape.csv"})
	if err == nil || !strings.Contains(err.Error(), "escapes the source root") {
		t.Fatalf("error = %v, want traversal rejection", err)
	}
}

func TestLocalReadOnlyBlobSourceRejectsIntermediateSymlink(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	targetDir := filepath.Join(baseDir, "target")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "invoice.csv"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, filepath.Join(baseDir, "linked")); err != nil {
		t.Fatal(err)
	}

	source, err := NewLocalReadOnlyBlobSource(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	_, _, err = source.Open(context.Background(), BlobObjectRef{Key: "linked/invoice.csv"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

func TestLocalReadOnlyBlobSourceRejectsSymlinkBaseDir(t *testing.T) {
	t.Parallel()

	realDir := t.TempDir()
	linkDir := filepath.Join(filepath.Dir(realDir), "billing-base-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	_, err := NewLocalReadOnlyBlobSource(linkDir)
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("error = %v, want real-directory rejection", err)
	}
}

func TestServiceImportActualInvoiceWithBlobInvoiceAdapter(t *testing.T) {
	fixture := newBillingFixture(t, nil)

	source, err := NewLocalReadOnlyBlobSource(filepath.Join("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source: source,
		Imports: []BlobInvoiceImportObject{{
			TenantID:         fixture.tenantID,
			Provider:         "azure",
			ExternalImportID: "azure-json-july-2026",
			Format:           ExportObjectFormatAzureCostJSON,
			Object:           BlobObjectRef{Key: "azure_cost_fixture.json"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	fixture.service.adapter = adapter
	imported, err := fixture.service.ImportActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID:         fixture.tenantID,
		Provider:         "azure",
		ExternalImportID: "azure-json-july-2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Import.Provider != "azure" || imported.Import.CurrencyCode != "USD" {
		t.Fatalf("import metadata = %#v", imported.Import)
	}
	if len(imported.Lines) != 2 {
		t.Fatalf("imported line count = %d, want 2", len(imported.Lines))
	}
}

func TestBlobInvoiceAdapterScopesMappingsByTenant(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "tenant-a.json"), []byte(`[
{"externalLineId":"cpu-tenant-a","billingPeriodStartAt":"2026-07-01T00:00:00Z","billingPeriodEndAt":"2026-08-01T00:00:00Z","currencyCode":"USD","costInBillingCurrency":1,"chargeKind":"cpu","resourceId":"/subscriptions/sub-a/resourceGroups/rg/providers/Microsoft.Compute/disks/a","region":"eastus"}
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "tenant-b.json"), []byte(`[
{"externalLineId":"cpu-tenant-b","billingPeriodStartAt":"2026-07-01T00:00:00Z","billingPeriodEndAt":"2026-08-01T00:00:00Z","currencyCode":"USD","costInBillingCurrency":1,"chargeKind":"cpu","resourceId":"/subscriptions/sub-b/resourceGroups/rg/providers/Microsoft.Compute/disks/b","region":"eastus"}
]`), 0o600); err != nil {
		t.Fatal(err)
	}

	source, err := NewLocalReadOnlyBlobSource(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	tenantA := uuid.New()
	tenantB := uuid.New()
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source: source,
		Imports: []BlobInvoiceImportObject{
			{
				TenantID:         tenantA,
				Provider:         "azure",
				ExternalImportID: "shared-import",
				Format:           ExportObjectFormatAzureCostJSON,
				Object:           BlobObjectRef{Key: "tenant-a.json"},
			},
			{
				TenantID:         tenantB,
				Provider:         "azure",
				ExternalImportID: "shared-import",
				Format:           ExportObjectFormatAzureCostJSON,
				Object:           BlobObjectRef{Key: "tenant-b.json"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	invoiceA, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID:         tenantA,
		Provider:         "azure",
		ExternalImportID: "shared-import",
	})
	if err != nil {
		t.Fatal(err)
	}
	invoiceB, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID:         tenantB,
		Provider:         "azure",
		ExternalImportID: "shared-import",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(invoiceA.Lines) != 1 || invoiceA.Lines[0].ExternalLineID != "cpu-tenant-a" {
		t.Fatalf("tenant A invoice = %#v, want tenant-scoped mapping", invoiceA)
	}
	if len(invoiceB.Lines) != 1 || invoiceB.Lines[0].ExternalLineID != "cpu-tenant-b" {
		t.Fatalf("tenant B invoice = %#v, want tenant-scoped mapping", invoiceB)
	}
}

func TestBlobInvoiceAdapterPreservesBareJSONNumberPrecision(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		provider         string
		format           ExportObjectFormat
		content          string
		wantChargeKind   string
		wantAmountMicros int64
	}{
		{
			name:     "gcp-json-half-micro-rounds-down",
			provider: "gcp",
			format:   ExportObjectFormatGCPBillingJSON,
			content: `[
{"lineItemId":"gcp-half-down","invoice":{"month":"202607"},"currency":"USD","cost":2.0000004999999,"chargeKind":"request","project":{"id":"project-a"},"location":{"region":"us-central1"},"resource":{"globalName":"//run.googleapis.com/projects/project-a/locations/us-central1/services/gateway"}}
]`,
			wantChargeKind:   ChargeKindRequest,
			wantAmountMicros: 2_000_000,
		},
		{
			name:     "azure-json-half-micro-rounds-down",
			provider: "azure",
			format:   ExportObjectFormatAzureCostJSON,
			content: `[
{"externalLineId":"azure-half-down","billingPeriodStartAt":"2026-07-01T00:00:00Z","billingPeriodEndAt":"2026-08-01T00:00:00Z","currencyCode":"USD","costInBillingCurrency":2.0000004999999,"chargeKind":"cpu","resourceId":"/subscriptions/sub-123/resourceGroups/rg/providers/Microsoft.Compute/disks/a","region":"eastus"}
]`,
			wantChargeKind:   ChargeKindCPU,
			wantAmountMicros: 2_000_000,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tenantID := uuid.New()
			_, adapter := newTempBlobAdapter(
				t, tenantID, tc.provider, "precision", tc.content, "fixture.json", 0, tc.format,
			)
			invoice, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
				TenantID:         tenantID,
				Provider:         tc.provider,
				ExternalImportID: "precision",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(invoice.Lines) != 1 {
				t.Fatalf("line count = %d, want 1", len(invoice.Lines))
			}
			if invoice.Lines[0].ChargeKind != tc.wantChargeKind {
				t.Fatalf("charge kind = %q, want %q", invoice.Lines[0].ChargeKind, tc.wantChargeKind)
			}
			if invoice.Lines[0].AmountMicros != tc.wantAmountMicros {
				t.Fatalf("amount micros = %d, want %d", invoice.Lines[0].AmountMicros, tc.wantAmountMicros)
			}
		})
	}
}

func newTempBlobAdapter(
	t *testing.T,
	tenantID uuid.UUID,
	provider string,
	externalImportID string,
	content string,
	filename string,
	maxObjectBytes int64,
	format ExportObjectFormat,
) (*LocalReadOnlyBlobSource, *BlobInvoiceAdapter) {
	t.Helper()

	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, filename), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewLocalReadOnlyBlobSource(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source:         source,
		MaxObjectBytes: maxObjectBytes,
		Imports: []BlobInvoiceImportObject{{
			TenantID:         tenantID,
			Provider:         provider,
			ExternalImportID: externalImportID,
			Format:           format,
			Object:           BlobObjectRef{Key: filename},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, adapter
}

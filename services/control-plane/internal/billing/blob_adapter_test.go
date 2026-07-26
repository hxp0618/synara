package billing

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBlobInvoiceAdapterCUR2ManifestPinsManifestAndChunkVersions(t *testing.T) {
	t.Parallel()

	const manifestKey = "exports/demo/metadata/BILLING_PERIOD=2026-07/20260726T000000Z-exec/Manifest.json"
	const chunkAKey = "exports/demo/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/chunk-a.csv.gz"
	const chunkBKey = "exports/demo/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/chunk-b.csv.gz"
	const header = "identity_line_item_id,bill_billing_period_start_date,bill_billing_period_end_date,line_item_currency_code,line_item_unblended_cost,line_item_net_unblended_cost,line_item_line_item_type,line_item_usage_type,line_item_resource_id,line_item_usage_account_id,product_region,line_item_usage_start_date,line_item_usage_end_date,line_item_operation,line_item_product_code,resource_tags,split_line_item_parent_resource_id,split_line_item_split_cost,split_line_item_unused_cost,split_line_item_net_split_cost,split_line_item_net_unused_cost\n"
	chunkA := gzipTestBlob(t, header+
		"parent-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,9,5,Usage,CPU,i-parent,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},,,,,\n"+
		"parent-partial,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,2.5,,Usage,Memory,i-parent-2,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,CreateVolume,AmazonEC2,{},,,,,\n"+
		"parent-other-usage,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,4,4,Usage,Memory,i-parent,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,CreateVolume,AmazonEC2,{},,,,,\n"+
		"refund-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,-1,,Refund,CPU,,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,Refund,AmazonEC2,{},,,,,\n")
	chunkB := gzipTestBlob(t, header+
		"child-1,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,10,8,Usage,CPU,arn:aws:eks:us-east-1:123456789012:pod/cluster-a/default/worker-a/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},i-parent,7,2,3,0\n"+
		"child-memory,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,10,8,Usage,Memory,arn:aws:eks:us-east-1:123456789012:pod/cluster-a/default/worker-b/cccccccc-cccc-4ccc-8ccc-cccccccccccc,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},i-parent,7,2,2,0\n"+
		"child-partial,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,3,3,Usage,Memory,pod-without-uid,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,CreateVolume,AmazonEC2,{user:synara:cluster-id=cluster-a},i-parent-2,2,0.5,,\n")
	manifestV1 := []byte(`{"dataFiles":[{"filePath":"` + chunkBKey + `"},{"filePath":"` + chunkAKey + `"}],"billingPeriod":{"start":"2026-07-01","end":"2026-08-01"},"columns":[]}`)
	manifestPoison := []byte(`{"dataFiles":[{"filePath":"exports/demo/data/BILLING_PERIOD=2026-07/poison/poison.snappy.parquet"}]}`)
	source := &fakeVersionedManifestBlobSource{
		latest: map[string]string{
			manifestKey: "manifest-v2",
			chunkAKey:   "chunk-a-v1",
			chunkBKey:   "chunk-b-v1",
		},
		objects: map[string][]byte{
			versionedBlobKey(manifestKey, "manifest-v1"): manifestV1,
			versionedBlobKey(manifestKey, "manifest-v2"): manifestPoison,
			versionedBlobKey(chunkAKey, "chunk-a-v1"):    chunkA,
			versionedBlobKey(chunkBKey, "chunk-b-v1"):    chunkB,
		},
		lastModified: map[string]time.Time{
			versionedBlobKey(manifestKey, "manifest-v1"): time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
			versionedBlobKey(manifestKey, "manifest-v2"): time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
			versionedBlobKey(chunkAKey, "chunk-a-v1"):    time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
			versionedBlobKey(chunkBKey, "chunk-b-v1"):    time.Date(2026, 7, 1, 0, 0, 1, 0, time.UTC),
		},
	}
	tenantID := uuid.New()
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source: source,
		Imports: []BlobInvoiceImportObject{{
			TenantID: tenantID, Provider: "aws", ExternalImportID: "cur2-july",
			Format: ExportObjectFormatAWSCUR2Manifest,
			Object: BlobObjectRef{Key: manifestKey, Version: "manifest-v1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	invoice, err := adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID: tenantID, Provider: "aws", ExternalImportID: "cur2-july",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(source.resolved, ",") != chunkAKey+","+chunkBKey {
		t.Fatalf("resolved chunks = %#v, want stable sorted order", source.resolved)
	}
	if len(source.opened) != 3 || source.opened[0] != versionedBlobKey(manifestKey, "manifest-v1") {
		t.Fatalf("opened objects = %#v, want pinned manifest followed by two pinned chunks", source.opened)
	}
	if len(invoice.Lines) != 4 {
		t.Fatalf("invoice lines = %#v, want CPU+Memory children, partial allocation, and unmatched parent usage", invoice.Lines)
	}
	amounts := make(map[string]int64)
	for _, line := range invoice.Lines {
		amounts[line.ResourceCorrelationKey] = line.AmountMicros
	}
	if got := amounts["kubernetes:cluster-a:us-east-1:default:worker-a:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"]; got != 3_000_000 {
		t.Fatalf("CPU child amount = %d, want 3000000", got)
	}
	if got := amounts["kubernetes:cluster-a:us-east-1:default:worker-b:cccccccc-cccc-4ccc-8ccc-cccccccccccc"]; got != 2_000_000 {
		t.Fatalf("Memory child amount = %d, want 2000000", got)
	}
	partialFound := false
	for key, amount := range amounts {
		if strings.HasPrefix(key, "aws-allocation-split-missing-pod-uid:") {
			partialFound = true
			if amount != 2_500_000 {
				t.Fatalf("partial allocation amount = %d, want 2500000", amount)
			}
		}
	}
	if !partialFound {
		t.Fatalf("resource keys = %#v, want explicit non-exact allocation quality", amounts)
	}
	unmatchedParentFound := false
	for key, amount := range amounts {
		if strings.HasPrefix(key, "aws-allocation-split-parent-not-replaced:") {
			unmatchedParentFound = true
			if amount != 4_000_000 {
				t.Fatalf("unmatched parent usage amount = %d, want 4000000", amount)
			}
		}
	}
	if !unmatchedParentFound {
		t.Fatalf("resource keys = %#v, want same-instance unrelated usage preserved", amounts)
	}
	if invoice.SourceProvenance == nil || invoice.SourceProvenance.FilteredAdjustmentCount != 1 ||
		invoice.SourceProvenance.FilteredAdjustmentAmountMicros != -1_000_000 || len(invoice.SourceProvenance.BundleChecksum) != 64 {
		t.Fatalf("source provenance = %#v, want explicit filtered refund and bundle checksum", invoice.SourceProvenance)
	}
}

func TestBlobInvoiceAdapterCUR2ManifestFailsClosedForParquet(t *testing.T) {
	t.Parallel()

	const manifestKey = "exports/demo/metadata/BILLING_PERIOD=2026-07/20260726T000000Z-exec/Manifest.json"
	tenantID := uuid.New()
	source := &fakeVersionedManifestBlobSource{
		objects: map[string][]byte{
			versionedBlobKey(manifestKey, "v1"): []byte(`{"dataFiles":["exports/demo/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/chunk-00001.snappy.parquet"]}`),
		},
		lastModified: map[string]time.Time{
			versionedBlobKey(manifestKey, "v1"): time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
		},
	}
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{Source: source, Imports: []BlobInvoiceImportObject{{
		TenantID: tenantID, Provider: "aws", ExternalImportID: "parquet", Format: ExportObjectFormatAWSCUR2Manifest,
		Object: BlobObjectRef{Key: manifestKey, Version: "v1"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID: tenantID, Provider: "aws", ExternalImportID: "parquet",
	})
	if err == nil || !strings.Contains(err.Error(), "parquet") || len(source.resolved) != 0 {
		t.Fatalf("error = %v resolved=%#v, want fail-closed parquet before chunk resolution", err, source.resolved)
	}
}

func TestAWSCUR2ExecutionBoundaryUsesFullMetadataDataPath(t *testing.T) {
	t.Parallel()
	manifest := "root/export/metadata/BILLING_PERIOD=2026-07/20260726T000000Z-exec/Manifest.json"
	validChunk := "root/export/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/export-00001.csv.gz"
	if !sameAWSCUR2ExecutionBoundary(manifest, validChunk) {
		t.Fatal("realistic AWS metadata/data execution boundary was rejected")
	}
	for _, invalid := range []string{
		"other/export/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/export-00001.csv.gz",
		"root/export/data/BILLING_PERIOD=2026-08/20260726T000000Z-exec/export-00001.csv.gz",
		"root/export/data/BILLING_PERIOD=2026-07/other-exec/export-00001.csv.gz",
		"root/export/other/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/export-00001.csv.gz",
	} {
		if sameAWSCUR2ExecutionBoundary(manifest, invalid) {
			t.Fatalf("invalid delivery boundary accepted: %q", invalid)
		}
	}
}

func TestAWSCURCostUsesNetAmortizedCostAndPreservesAdjustmentSigns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		record     map[string]string
		wantMicros int64
	}{
		{
			name: "net-amortized-reservation",
			record: map[string]string{
				"reservationneteffectivecost": "6", "reservationeffectivecost": "7",
				"lineitemnetunblendedcost": "8", "lineitemunblendedcost": "10",
			},
			wantMicros: 6_000_000,
		},
		{name: "positive-credit-preserves-source-sign", record: map[string]string{
			"lineitemlineitemtype": "Credit", "lineitemunblendedcost": "2",
		}, wantMicros: 2_000_000},
		{name: "private-rate-discount", record: map[string]string{
			"lineitemlineitemtype": "PrivateRateDiscount", "lineitemunblendedcost": "-1.5",
		}, wantMicros: -1_500_000},
		{name: "negative-refund-stays-negative", record: map[string]string{
			"lineitemlineitemtype": "Refund", "lineitemunblendedcost": "-1",
		}, wantMicros: -1_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			amount, err := awsCURCost(tc.record)
			if err != nil {
				t.Fatal(err)
			}
			micros, err := decimalRatToRoundedMicros(amount)
			if err != nil {
				t.Fatal(err)
			}
			if micros != tc.wantMicros {
				t.Fatalf("amount micros = %d, want %d", micros, tc.wantMicros)
			}
		})
	}
}

func TestAWSCURSplitCostKeepsNetAndNonNetLayersPaired(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		record     map[string]string
		wantMicros int64
		wantErr    string
	}{
		{name: "positive-unused", record: map[string]string{
			"splitlineitemparentresourceid": "i-parent", "splitlineitemsplitcost": "7", "splitlineitemunusedcost": "2",
		}, wantMicros: 9_000_000},
		{name: "zero-unused", record: map[string]string{
			"splitlineitemparentresourceid": "i-parent", "splitlineitemsplitcost": "7", "splitlineitemunusedcost": "0",
		}, wantMicros: 7_000_000},
		{name: "net-discount", record: map[string]string{
			"splitlineitemparentresourceid": "i-parent", "splitlineitemnetsplitcost": "6", "splitlineitemnetunusedcost": "-1",
		}, wantMicros: 5_000_000},
		{name: "reject-cross-layer-unused", record: map[string]string{
			"splitlineitemparentresourceid": "i-parent", "splitlineitemnetsplitcost": "6", "splitlineitemunusedcost": "2",
		}, wantErr: "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			amount, err := awsCURCost(tc.record)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			micros, err := decimalRatToRoundedMicros(amount)
			if err != nil {
				t.Fatal(err)
			}
			if micros != tc.wantMicros {
				t.Fatalf("amount micros = %d, want %d", micros, tc.wantMicros)
			}
		})
	}
}

func TestAWSCURSplitReplacementAggregatesCPUAndMemoryAndRejectsMismatch(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"billbillingperiodstartdate": "2026-07-01T00:00:00Z",
		"billbillingperiodenddate":   "2026-08-01T00:00:00Z",
		"lineitemcurrencycode":       "USD", "lineitemlineitemtype": "Usage",
		"lineitemusageaccountid": "123456789012", "productregion": "us-east-1", "resourcetags": "{}",
		"lineitemusagestartdate": "2026-07-01T00:00:00Z", "lineitemusageenddate": "2026-07-01T01:00:00Z",
		"lineitemoperation": "RunInstances", "lineitemproductcode": "AmazonEC2",
	}
	parent := mapsClone(base)
	parent["identitylineitemid"], parent["lineitemresourceid"], parent["lineitemusagetype"] = "parent", "i-parent", "CPU"
	parent["lineitemnetunblendedcost"] = "5"
	childCPU := mapsClone(base)
	childCPU["identitylineitemid"], childCPU["lineitemresourceid"], childCPU["lineitemusagetype"] = "cpu", "pod-cpu", "CPU"
	childCPU["splitlineitemparentresourceid"], childCPU["splitlineitemnetsplitcost"], childCPU["splitlineitemnetunusedcost"] = "i-parent", "3", "0"
	childMemory := mapsClone(base)
	childMemory["identitylineitemid"], childMemory["lineitemresourceid"], childMemory["lineitemusagetype"] = "memory", "pod-memory", "Memory"
	childMemory["splitlineitemparentresourceid"], childMemory["splitlineitemnetsplitcost"], childMemory["splitlineitemnetunusedcost"] = "  i-parent  ", "2", "0"

	parsed, err := parseAWSCURRecords([]map[string]string{parent, childCPU, childMemory}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Rows) != 2 {
		t.Fatalf("rows = %#v, want CPU and Memory children with parent suppressed", parsed.Rows)
	}

	childMemory["splitlineitemnetsplitcost"] = "1"
	_, err = parseAWSCURRecords([]map[string]string{parent, childCPU, childMemory}, true)
	if err == nil || !strings.Contains(err.Error(), "do not exactly replace parent cost") {
		t.Fatalf("mismatch error = %v", err)
	}

	orphan := mapsClone(childCPU)
	orphan["identitylineitemid"] = "orphan"
	_, err = parseAWSCURRecords([]map[string]string{orphan}, true)
	if err == nil || !strings.Contains(err.Error(), "0 parent rows") {
		t.Fatalf("child-only error = %v", err)
	}

	grossOrphan := mapsClone(childCPU)
	grossOrphan["identitylineitemid"] = "gross-orphan"
	delete(grossOrphan, "splitlineitemnetsplitcost")
	delete(grossOrphan, "splitlineitemnetunusedcost")
	grossOrphan["splitlineitemsplitcost"], grossOrphan["splitlineitemunusedcost"] = "5", "0"
	_, err = parseAWSCURRecords([]map[string]string{parent, grossOrphan}, true)
	if err == nil || !strings.Contains(err.Error(), "0 parent rows") {
		t.Fatalf("net-parent/gross-child error = %v", err)
	}
}

func TestBlobInvoiceAdapterCUR2RejectsMissingParentAcrossChunks(t *testing.T) {
	t.Parallel()
	const manifestKey = "missing/demo/metadata/BILLING_PERIOD=2026-07/20260726T000000Z-exec/Manifest.json"
	const chunkAKey = "missing/demo/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/chunk-a.csv.gz"
	const chunkBKey = "missing/demo/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/chunk-b.csv.gz"
	const header = "identity_line_item_id,bill_billing_period_start_date,bill_billing_period_end_date,line_item_currency_code,line_item_unblended_cost,line_item_line_item_type,line_item_usage_type,line_item_resource_id,line_item_usage_account_id,product_region,line_item_usage_start_date,line_item_usage_end_date,line_item_operation,line_item_product_code,resource_tags,split_line_item_parent_resource_id,split_line_item_split_cost,split_line_item_unused_cost\n"
	chunkA := gzipTestBlob(t, header+
		"unrelated,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,Usage,CPU,i-unrelated,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},,,\n")
	chunkB := gzipTestBlob(t, header+
		"orphan-child,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,Usage,Memory,pod-orphan,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},i-missing,1,0\n")
	manifest := []byte(`{"dataFiles":["` + chunkAKey + `","` + chunkBKey + `"]}`)
	modified := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	source := &fakeVersionedManifestBlobSource{
		latest: map[string]string{chunkAKey: "a-v1", chunkBKey: "b-v1"},
		objects: map[string][]byte{
			versionedBlobKey(manifestKey, "manifest-v1"): manifest,
			versionedBlobKey(chunkAKey, "a-v1"):          chunkA,
			versionedBlobKey(chunkBKey, "b-v1"):          chunkB,
		},
		lastModified: map[string]time.Time{
			versionedBlobKey(manifestKey, "manifest-v1"): modified,
			versionedBlobKey(chunkAKey, "a-v1"):          modified,
			versionedBlobKey(chunkBKey, "b-v1"):          modified,
		},
	}
	tenantID := uuid.New()
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{Source: source, Imports: []BlobInvoiceImportObject{{
		TenantID: tenantID, Provider: "aws", ExternalImportID: "missing-parent",
		Format: ExportObjectFormatAWSCUR2Manifest, Object: BlobObjectRef{Key: manifestKey, Version: "manifest-v1"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
		TenantID: tenantID, Provider: "aws", ExternalImportID: "missing-parent",
	})
	if err == nil || !strings.Contains(err.Error(), "0 parent rows") {
		t.Fatalf("missing parent chunk error = %v", err)
	}
}

func TestAWSCUR2RejectsPositiveResourceLessAdjustment(t *testing.T) {
	t.Parallel()
	record := map[string]string{
		"identitylineitemid": "positive-refund", "billbillingperiodstartdate": "2026-07-01T00:00:00Z",
		"billbillingperiodenddate": "2026-08-01T00:00:00Z", "lineitemcurrencycode": "USD",
		"lineitemlineitemtype": "Refund", "lineitemunblendedcost": "1", "lineitemusageaccountid": "123456789012",
	}
	_, err := parseAWSCURRecords([]map[string]string{record}, true)
	if err == nil || !strings.Contains(err.Error(), "must not be positive") {
		t.Fatalf("positive refund error = %v", err)
	}
}

func mapsClone(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func TestBlobInvoiceAdapterCUR2ImportBudgets(t *testing.T) {
	t.Parallel()

	const manifestKey = "budget/demo/metadata/BILLING_PERIOD=2026-07/20260726T000000Z-exec/Manifest.json"
	const chunkKey = "budget/demo/data/BILLING_PERIOD=2026-07/20260726T000000Z-exec/chunk.csv.gz"
	manifest := []byte(`{"dataFiles":["` + chunkKey + `"]}`)
	csvBlob := []byte("identity_line_item_id,bill_billing_period_start_date,bill_billing_period_end_date,line_item_currency_code,line_item_unblended_cost,line_item_line_item_type,line_item_usage_type,line_item_resource_id,line_item_usage_account_id,product_region,resource_tags\n" +
		"one,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,Usage,CPU,i-one,123456789012,us-east-1,{}\n" +
		"two,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,Usage,CPU,i-two,123456789012,us-east-1,{}\n")
	compressed := gzipTestBlob(t, string(csvBlob))
	for _, tc := range []struct {
		name    string
		budget  CUR2ImportBudget
		wantErr string
	}{
		{
			name:    "cumulative-compressed",
			budget:  CUR2ImportBudget{MaxCompressedBytes: int64(len(manifest) + len(compressed) - 1)},
			wantErr: "compressed bytes limit",
		},
		{
			name:    "cumulative-decompressed",
			budget:  CUR2ImportBudget{MaxDecompressedBytes: int64(len(manifest) + len(csvBlob) - 1)},
			wantErr: "decompressed bytes limit",
		},
		{name: "row-count", budget: CUR2ImportBudget{MaxRows: 1}, wantErr: "row limit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := &fakeVersionedManifestBlobSource{
				latest: map[string]string{chunkKey: "chunk-v1"},
				objects: map[string][]byte{
					versionedBlobKey(manifestKey, "manifest-v1"): manifest,
					versionedBlobKey(chunkKey, "chunk-v1"):       compressed,
				},
				lastModified: map[string]time.Time{
					versionedBlobKey(manifestKey, "manifest-v1"): time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
					versionedBlobKey(chunkKey, "chunk-v1"):       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
				},
			}
			tenantID := uuid.New()
			adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
				Source: source, CUR2Budget: tc.budget,
				Imports: []BlobInvoiceImportObject{{
					TenantID: tenantID, Provider: "aws", ExternalImportID: "budget",
					Format: ExportObjectFormatAWSCUR2Manifest,
					Object: BlobObjectRef{Key: manifestKey, Version: "manifest-v1"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.FetchActualInvoice(context.Background(), ImportActualInvoiceRequest{
				TenantID: tenantID, Provider: "aws", ExternalImportID: "budget",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAWSResourceKeyRequiresTrustedExactKubernetesIdentity(t *testing.T) {
	t.Parallel()

	genericAliases := map[string]string{
		"resourcetags":           `{cluster-id=cluster-a,namespace=default,pod=worker-a,pod-uid=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb}`,
		"lineitemresourceid":     "i-untrusted",
		"lineitemusageaccountid": "123456789012",
	}
	key, err := buildAWSResourceKey(genericAliases, "us-east-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(key, "kubernetes:") || !strings.HasPrefix(key, "aws-allocation-provider-resource:") {
		t.Fatalf("generic aliases produced resource key %q", key)
	}

	trustedInvalidUID := map[string]string{
		"resourcetags":       `{user:synara:cluster-id=cluster-a,user:synara:namespace=default,user:synara:pod=worker-a,user:synara:instance-uid=not-a-uuid}`,
		"lineitemresourceid": "i-tagged",
	}
	if _, err := buildAWSResourceKey(trustedInvalidUID, "us-east-1", true); err == nil || !strings.Contains(err.Error(), "not a UUID") {
		t.Fatalf("trusted invalid UID error = %v", err)
	}

	arnRegionConflict := map[string]string{
		"resourcetags":           "{}",
		"lineitemresourceid":     "arn:aws:eks:us-west-2:123456789012:pod/cluster-a/default/worker-a/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"lineitemusageaccountid": "123456789012",
	}
	if _, err := buildAWSResourceKey(arnRegionConflict, "us-east-1", true); err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("ARN region conflict error = %v", err)
	}
}

type fakeVersionedManifestBlobSource struct {
	latest       map[string]string
	objects      map[string][]byte
	lastModified map[string]time.Time
	resolved     []string
	opened       []string
}

func (s *fakeVersionedManifestBlobSource) ResolveManifestObject(_ context.Context, key string) (ResolvedBlobObject, error) {
	s.resolved = append(s.resolved, key)
	version := s.latest[key]
	if version == "" {
		return ResolvedBlobObject{}, os.ErrNotExist
	}
	objectKey := versionedBlobKey(key, version)
	blob := s.objects[objectKey]
	return ResolvedBlobObject{
		Ref: BlobObjectRef{Key: key, Version: version},
		Metadata: BlobObjectMetadata{
			SizeBytes: int64(len(blob)), Version: version, ETag: "etag-" + version, LastModified: s.lastModified[objectKey],
		},
	}, nil
}

func (s *fakeVersionedManifestBlobSource) Open(_ context.Context, object BlobObjectRef) (io.ReadCloser, BlobObjectMetadata, error) {
	key := versionedBlobKey(object.Key, object.Version)
	s.opened = append(s.opened, key)
	blob, ok := s.objects[key]
	if !ok {
		return nil, BlobObjectMetadata{}, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(blob)), BlobObjectMetadata{
		SizeBytes: int64(len(blob)), Version: object.Version, ETag: "etag-" + object.Version,
		LastModified: s.lastModified[key],
	}, nil
}

func versionedBlobKey(key, version string) string {
	return key + "@" + version
}

func gzipTestBlob(t *testing.T, content string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

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

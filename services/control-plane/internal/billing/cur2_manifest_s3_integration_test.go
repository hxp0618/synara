package billing

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

func TestBillingCUR2ManifestVersionedS3Acceptance(t *testing.T) {
	requiredEnvironment := []string{
		"SYNARA_COST_ACCOUNTING_RUNTIME_ACCEPTANCE_S3_ENDPOINT",
		"SYNARA_COST_ACCOUNTING_RUNTIME_ACCEPTANCE_S3_BUCKET",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
	}
	var missing []string
	for _, name := range requiredEnvironment {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		t.Skipf("billing CUR 2.0 versioned S3 acceptance is opt-in; missing %s", strings.Join(missing, ", "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	endpoint := strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_RUNTIME_ACCEPTANCE_S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("SYNARA_COST_ACCOUNTING_RUNTIME_ACCEPTANCE_S3_BUCKET"))
	awsRuntimeConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(billingRuntimeAcceptanceRegion()))
	if err != nil {
		t.Fatal(err)
	}
	client := awss3.NewFromConfig(awsRuntimeConfig, func(options *awss3.Options) {
		options.UsePathStyle = true
		options.BaseEndpoint = aws.String(endpoint)
	})

	const deliveryRoot = "runtime/aws-data-exports/cur2-final17"
	const partition = "BILLING_PERIOD=2026-07"
	const executionID = "20260726T000000Z-11111111-2222-4333-8444-555555555555"
	const header = "identity_line_item_id,bill_billing_period_start_date,bill_billing_period_end_date,line_item_currency_code,line_item_unblended_cost,line_item_net_unblended_cost,line_item_line_item_type,line_item_usage_type,line_item_resource_id,line_item_usage_account_id,product_region,line_item_usage_start_date,line_item_usage_end_date,line_item_operation,line_item_product_code,resource_tags,split_line_item_parent_resource_id,split_line_item_split_cost,split_line_item_unused_cost,split_line_item_net_split_cost,split_line_item_net_unused_cost\n"
	chunkDirectory := deliveryRoot + "/data/" + partition + "/" + executionID
	manifestDirectory := deliveryRoot + "/metadata/" + partition + "/" + executionID
	chunkAKey := chunkDirectory + "/cur2-final17-00001.csv.gz"
	chunkBKey := chunkDirectory + "/cur2-final17-00002.csv.gz"
	putVersionedS3AcceptanceObject(t, ctx, client, bucket, chunkAKey, gzipTestBlob(t, header+
		"parent,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,9,5,Usage,CPU,i-parent,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},,,,,\n"+
		"parent-other-usage,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,4,4,Usage,Memory,i-parent,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,CreateVolume,AmazonEC2,{},,,,,\n"+
		"refund,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,-1,,Refund,CPU,,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,Refund,AmazonEC2,{},,,,,\n"))
	putVersionedS3AcceptanceObject(t, ctx, client, bucket, chunkBKey, gzipTestBlob(t, header+
		"child-cpu,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,10,8,Usage,CPU,arn:aws:eks:us-east-1:123456789012:pod/cluster-a/default/worker-a/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},i-parent,7,2,3,0\n"+
		"child-memory,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,10,8,Usage,Memory,arn:aws:eks:us-east-1:123456789012:pod/cluster-a/default/worker-b/cccccccc-cccc-4ccc-8ccc-cccccccccccc,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},i-parent,7,2,2,0\n"))
	manifestKey := manifestDirectory + "/cur2-final17-Manifest.json"
	manifestVersion := putVersionedS3AcceptanceObject(t, ctx, client, bucket, manifestKey, []byte(fmt.Sprintf(
		`{"dataFiles":["s3://%s/%s","s3://%s/%s"]}`,
		bucket, chunkBKey, bucket, chunkAKey,
	)))
	poisonVersion := putVersionedS3AcceptanceObject(t, ctx, client, bucket, manifestKey, []byte(
		`{"dataFiles":["runtime/aws-data-exports/cur2-final17/data/BILLING_PERIOD=2026-07/poison/poison.snappy.parquet"]}`,
	))
	if manifestVersion == poisonVersion {
		t.Fatal("versioned S3 manifest replacement reused its VersionId")
	}

	s3Source, err := NewS3ReadOnlyBlobSource(ctx, SourceConfig{
		Kind: SourceKindS3, S3Bucket: bucket, S3Region: billingRuntimeAcceptanceRegion(),
		S3Endpoint: endpoint, S3UsePathStyle: true, S3AllowCustomEndpoint: true, S3AllowHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	poisoningSource := &poisonAfterResolveS3Source{
		S3ReadOnlyBlobSource: s3Source, client: client, bucket: bucket,
		poison: map[string][]byte{
			chunkAKey: gzipTestBlob(t, "poison-a\n"), chunkBKey: gzipTestBlob(t, "poison-b\n"),
		},
	}
	tenantID := uuid.New()
	adapter := newCUR2MinIOAdapter(t, poisoningSource, tenantID, "cur2-minio", manifestKey, manifestVersion, CUR2ImportBudget{})
	invoice, err := adapter.FetchActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: tenantID, Provider: "aws", ExternalImportID: "cur2-minio",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invoice.Lines) != 3 {
		t.Fatalf("CUR 2.0 MinIO invoice lines = %#v, want CPU+Memory children and unrelated parent usage", invoice.Lines)
	}
	amounts := make(map[string]int64, len(invoice.Lines))
	for _, line := range invoice.Lines {
		amounts[line.ResourceCorrelationKey] = line.AmountMicros
	}
	if amounts["kubernetes:cluster-a:us-east-1:default:worker-a:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"] != 3_000_000 ||
		amounts["kubernetes:cluster-a:us-east-1:default:worker-b:cccccccc-cccc-4ccc-8ccc-cccccccccccc"] != 2_000_000 {
		t.Fatalf("CUR 2.0 MinIO amounts = %#v", amounts)
	}
	if invoice.SourceProvenance == nil || invoice.SourceProvenance.FilteredAdjustmentCount != 1 ||
		invoice.SourceProvenance.FilteredAdjustmentAmountMicros != -1_000_000 || len(poisoningSource.poisoned) != 2 {
		t.Fatalf("CUR 2.0 MinIO provenance/poisoning = %#v/%#v", invoice.SourceProvenance, poisoningSource.poisoned)
	}
	otherUsage := false
	for key, amount := range amounts {
		if strings.HasPrefix(key, "aws-allocation-split-parent-not-replaced:") && amount == 4_000_000 {
			otherUsage = true
		}
	}
	if !otherUsage {
		t.Fatalf("CUR 2.0 MinIO did not preserve unrelated same-instance usage: %#v", amounts)
	}

	staleTenant := uuid.New()
	staleAdapter := newCUR2MinIOAdapter(t, s3Source, staleTenant, "cur2-stale", manifestKey, manifestVersion, CUR2ImportBudget{})
	_, err = staleAdapter.FetchActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: staleTenant, Provider: "aws", ExternalImportID: "cur2-stale",
	})
	if err == nil {
		t.Fatal("stale manifest unexpectedly imported poisoned latest chunks")
	}

	t.Run("child_only_rejected", func(t *testing.T) {
		child := cur2AcceptanceReplacementRecord("child-only", "pod-only", " i-orphan ", "CPU")
		child["splitlineitemnetsplitcost"], child["splitlineitemnetunusedcost"] = "1", "0"
		_, childErr := parseAWSCURRecords([]map[string]string{child}, true)
		if childErr == nil || !strings.Contains(childErr.Error(), "0 parent rows") {
			t.Fatalf("child-only error = %v", childErr)
		}
	})

	t.Run("net_parent_gross_child_rejected", func(t *testing.T) {
		parent := cur2AcceptanceReplacementRecord("net-parent", "i-parent-family", "", "CPU")
		parent["lineitemnetunblendedcost"] = "5"
		child := cur2AcceptanceReplacementRecord("gross-child", "pod-gross", "i-parent-family", "Memory")
		child["splitlineitemsplitcost"], child["splitlineitemunusedcost"] = "5", "0"
		_, familyErr := parseAWSCURRecords([]map[string]string{parent, child}, true)
		if familyErr == nil || !strings.Contains(familyErr.Error(), "0 parent rows") {
			t.Fatalf("net-parent/gross-child error = %v", familyErr)
		}
	})

	t.Run("cross_chunk_missing_parent_rejected", func(t *testing.T) {
		orphanRoot := deliveryRoot + "-orphan"
		orphanExecution := executionID + "-orphan"
		orphanData := orphanRoot + "/data/" + partition + "/" + orphanExecution
		orphanMetadata := orphanRoot + "/metadata/" + partition + "/" + orphanExecution
		unrelatedKey := orphanData + "/unrelated.csv.gz"
		orphanKey := orphanData + "/orphan.csv.gz"
		putVersionedS3AcceptanceObject(t, ctx, client, bucket, unrelatedKey, gzipTestBlob(t, header+
			"unrelated,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,,Usage,CPU,i-unrelated,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},,,,,\n"))
		putVersionedS3AcceptanceObject(t, ctx, client, bucket, orphanKey, gzipTestBlob(t, header+
			"orphan,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,,Usage,Memory,pod-orphan,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,{},i-missing,1,0,,\n"))
		orphanManifestKey := orphanMetadata + "/Manifest.json"
		orphanManifestVersion := putVersionedS3AcceptanceObject(t, ctx, client, bucket, orphanManifestKey, []byte(fmt.Sprintf(
			`{"dataFiles":["s3://%s/%s","s3://%s/%s"]}`, bucket, unrelatedKey, bucket, orphanKey,
		)))
		orphanTenant := uuid.New()
		orphanAdapter := newCUR2MinIOAdapter(
			t, s3Source, orphanTenant, "cur2-cross-chunk-orphan", orphanManifestKey, orphanManifestVersion, CUR2ImportBudget{},
		)
		_, orphanErr := orphanAdapter.FetchActualInvoice(ctx, ImportActualInvoiceRequest{
			TenantID: orphanTenant, Provider: "aws", ExternalImportID: "cur2-cross-chunk-orphan",
		})
		if orphanErr == nil || !strings.Contains(orphanErr.Error(), "0 parent rows") {
			t.Fatalf("cross-chunk orphan error = %v", orphanErr)
		}
	})

	t.Run("total-budget", func(t *testing.T) {
		budgetRoot := deliveryRoot + "-budget"
		budgetExecution := executionID + "-budget"
		budgetData := budgetRoot + "/data/" + partition + "/" + budgetExecution
		budgetManifestKey := budgetRoot + "/metadata/" + partition + "/" + budgetExecution + "/Manifest.json"
		budgetManifestVersion := putVersionedS3AcceptanceObject(t, ctx, client, bucket, budgetManifestKey, []byte(fmt.Sprintf(
			`{"dataFiles":["s3://%s/%s","s3://%s/%s"]}`,
			bucket, budgetData+"/one.csv.gz", bucket, budgetData+"/two.csv.gz",
		)))
		budgetTenant := uuid.New()
		budgetAdapter := newCUR2MinIOAdapter(t, s3Source, budgetTenant, "cur2-budget", budgetManifestKey, budgetManifestVersion, CUR2ImportBudget{MaxChunks: 1})
		_, budgetErr := budgetAdapter.FetchActualInvoice(ctx, ImportActualInvoiceRequest{
			TenantID: budgetTenant, Provider: "aws", ExternalImportID: "cur2-budget",
		})
		if budgetErr == nil || !strings.Contains(budgetErr.Error(), "limit is 1") {
			t.Fatalf("budget error = %v", budgetErr)
		}
	})

	t.Run("trusted-tag-arn-conflict", func(t *testing.T) {
		conflictRoot := deliveryRoot + "-conflict"
		conflictExecution := executionID + "-conflict"
		conflictChunk := conflictRoot + "/data/" + partition + "/" + conflictExecution + "/chunk.csv.gz"
		putVersionedS3AcceptanceObject(t, ctx, client, bucket, conflictChunk, gzipTestBlob(t, header+
			`conflict,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,USD,1,1,Usage,CPU,arn:aws:eks:us-east-1:123456789012:pod/cluster-a/default/worker-a/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb,123456789012,us-east-1,2026-07-01T00:00:00Z,2026-07-01T01:00:00Z,RunInstances,AmazonEC2,"{user:synara:cluster-id=other,user:synara:namespace=default,user:synara:pod=worker-a,user:synara:instance-uid=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb}",,,,,`+"\n"))
		conflictManifest := conflictRoot + "/metadata/" + partition + "/" + conflictExecution + "/Manifest.json"
		conflictVersion := putVersionedS3AcceptanceObject(t, ctx, client, bucket, conflictManifest, []byte(fmt.Sprintf(
			`{"dataFiles":["s3://%s/%s"]}`, bucket, conflictChunk,
		)))
		conflictTenant := uuid.New()
		conflictAdapter := newCUR2MinIOAdapter(t, s3Source, conflictTenant, "cur2-conflict", conflictManifest, conflictVersion, CUR2ImportBudget{})
		_, conflictErr := conflictAdapter.FetchActualInvoice(ctx, ImportActualInvoiceRequest{
			TenantID: conflictTenant, Provider: "aws", ExternalImportID: "cur2-conflict",
		})
		if conflictErr == nil || !strings.Contains(conflictErr.Error(), "conflict") {
			t.Fatalf("tag/ARN conflict error = %v", conflictErr)
		}
	})
}

func cur2AcceptanceReplacementRecord(id, resourceID, parentResourceID, usageType string) map[string]string {
	return map[string]string{
		"identitylineitemid": id, "billbillingperiodstartdate": "2026-07-01T00:00:00Z",
		"billbillingperiodenddate": "2026-08-01T00:00:00Z", "lineitemcurrencycode": "USD",
		"lineitemlineitemtype": "Usage", "lineitemusagetype": usageType, "lineitemresourceid": resourceID,
		"lineitemusageaccountid": "123456789012", "productregion": "us-east-1", "resourcetags": "{}",
		"lineitemusagestartdate": "2026-07-01T00:00:00Z", "lineitemusageenddate": "2026-07-01T01:00:00Z",
		"lineitemoperation": "RunInstances", "lineitemproductcode": "AmazonEC2",
		"splitlineitemparentresourceid": parentResourceID,
	}
}

type poisonAfterResolveS3Source struct {
	*S3ReadOnlyBlobSource
	client   *awss3.Client
	bucket   string
	poison   map[string][]byte
	poisoned []string
}

func (s *poisonAfterResolveS3Source) ResolveManifestObject(ctx context.Context, key string) (ResolvedBlobObject, error) {
	object, err := s.S3ReadOnlyBlobSource.ResolveManifestObject(ctx, key)
	if err != nil {
		return ResolvedBlobObject{}, err
	}
	if payload, ok := s.poison[object.Ref.Key]; ok {
		if _, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(s.bucket), Key: aws.String(object.Ref.Key), Body: bytes.NewReader(payload),
		}); err != nil {
			return ResolvedBlobObject{}, err
		}
		s.poisoned = append(s.poisoned, object.Ref.Key)
		delete(s.poison, object.Ref.Key)
	}
	return object, nil
}

func newCUR2MinIOAdapter(
	t *testing.T,
	source BlobSource,
	tenantID uuid.UUID,
	externalImportID, manifestKey, manifestVersion string,
	budget CUR2ImportBudget,
) *BlobInvoiceAdapter {
	t.Helper()
	adapter, err := NewBlobInvoiceAdapter(BlobInvoiceAdapterConfig{
		Source: source, MaxObjectBytes: 1 << 20, CUR2Budget: budget,
		Imports: []BlobInvoiceImportObject{{
			TenantID: tenantID, Provider: "aws", ExternalImportID: externalImportID,
			Format: ExportObjectFormatAWSCUR2Manifest, Object: BlobObjectRef{Key: manifestKey, Version: manifestVersion},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func putVersionedS3AcceptanceObject(
	t *testing.T,
	ctx context.Context,
	client *awss3.Client,
	bucket string,
	key string,
	payload []byte,
) string {
	t.Helper()
	output, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(aws.ToString(output.VersionId))
	if version == "" || strings.EqualFold(version, "null") {
		t.Fatalf("versioned S3 object %q returned no immutable VersionId", key)
	}
	return version
}

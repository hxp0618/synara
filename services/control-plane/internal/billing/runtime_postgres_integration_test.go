package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const billingRuntimeAcceptanceEvidenceDetailEnv = "SYNARA_BILLING_RUNTIME_ACCEPTANCE_EVIDENCE_DETAIL_FILE"

func TestBillingRuntimePostgresVersionedS3ImportEstimateReconcileReplay(t *testing.T) {
	requiredEnvironment := []string{
		"SYNARA_TEST_DATABASE_URL",
		"SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_ENDPOINT",
		"SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_BUCKET",
		"SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_OBJECT_KEY",
		"SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_OBJECT_VERSION",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
	}
	missing := make([]string, 0, len(requiredEnvironment))
	for _, name := range requiredEnvironment {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		t.Skipf("billing runtime PostgreSQL/S3 acceptance is opt-in; missing %s", strings.Join(missing, ", "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openBillingPostgresIntegrationDB(t)
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	var appliedBillingMigrations int64
	if err := db.WithContext(ctx).Table("control_plane_schema_migrations").
		Where("version IN ?", []int{64, 68}).Count(&appliedBillingMigrations).Error; err != nil {
		t.Fatal(err)
	}
	if appliedBillingMigrations != 2 {
		t.Fatalf("billing migrations applied = %d, want 2", appliedBillingMigrations)
	}

	externalImportID := "runtime-s3-" + uuid.NewString()
	targetID := uuid.New()
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	if err := db.WithContext(ctx).Create(&persistence.ExecutionTarget{
		ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "billing-runtime-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: base, UpdatedAt: base,
	}).Error; err != nil {
		t.Fatal(err)
	}
	configuredImport := ConfiguredImport{
		TenantID:            domain.TenantID,
		Provider:            "aws",
		ExternalImportID:    externalImportID,
		Format:              ExportObjectFormatAWSCURCSV,
		ObjectKey:           strings.TrimSpace(os.Getenv("SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_OBJECT_KEY")),
		ObjectVersion:       strings.TrimSpace(os.Getenv("SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_OBJECT_VERSION")),
		ScheduleInterval:    time.Hour,
		Reconcile:           true,
		EstimateAfterImport: true,
		ExecutionTargetIDs:  []uuid.UUID{targetID},
	}
	runtimeConfig := RuntimeConfig{
		MaxObjectBytes: 1 << 20,
		Source: SourceConfig{
			Kind:                  SourceKindS3,
			S3Bucket:              strings.TrimSpace(os.Getenv("SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_BUCKET")),
			S3Region:              billingRuntimeAcceptanceRegion(),
			S3Endpoint:            strings.TrimSpace(os.Getenv("SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_ENDPOINT")),
			S3UsePathStyle:        true,
			S3AllowCustomEndpoint: true,
			S3AllowHTTP:           true,
		},
		Imports: []ConfiguredImport{configuredImport},
	}

	adapter, imports, closer, err := NewAdapterFromRuntime(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Cleanup(func() { _ = closer.Close() })
	}
	preview, err := adapter.FetchActualInvoice(ctx, ImportActualInvoiceRequest{
		TenantID: domain.TenantID, Provider: "aws", ExternalImportID: externalImportID,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerIdentity := requireBillingRuntimeAcceptanceInvoice(t, preview)

	insertBillingRuntimePostgresWorker(
		t, db, base, domain.TenantID, targetID,
		workerIdentity.clusterID, workerIdentity.region, workerIdentity.namespace,
		workerIdentity.podName, workerIdentity.instanceUID,
	)
	tariffService := NewService(db, nil)
	for _, input := range billingRuntimeAcceptanceTariffs(base, workerIdentity.region) {
		if _, err := tariffService.CreateTariff(ctx, input); err != nil {
			t.Fatalf("create acceptance tariff version %d: %v", input.Version, err)
		}
	}

	service := NewService(db, adapter, WithConfiguredImports(imports), WithBuiltInEstimateSweeper())
	service.now = func() time.Time { return preview.BillingPeriodEndAt.Add(time.Hour) }
	first, err := service.RunImportSchedulerOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 || first.Imported != 1 || first.Reconciled != 1 || first.EstimateWorkers != 1 ||
		first.EstimateSweeps != 8 || first.EstimateWorkerFailures != 0 || first.Failed != 0 {
		t.Fatalf("first billing runtime scheduler summary = %#v", first)
	}

	importModel, lines, estimates, auditCount := loadBillingRuntimeAcceptanceState(
		t, ctx, db, domain.TenantID, externalImportID, targetID,
	)
	if len(lines) != 3 || len(estimates) != 8 || auditCount != 2 {
		t.Fatalf("billing runtime persisted lines/estimates/audits = %d/%d/%d, want 3/8/2", len(lines), len(estimates), auditCount)
	}
	if len(importModel.SourceChecksum) != 64 {
		t.Fatalf("billing runtime import checksum length = %d, want 64", len(importModel.SourceChecksum))
	}
	assertBillingRuntimeAcceptanceReconciliation(t, lines, estimates, workerIdentity.resourceKey)
	estimateIDs := billingRuntimeEstimateIDs(estimates)

	restartAdapter, restartImports, restartCloser, err := NewAdapterFromRuntime(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if restartCloser != nil {
		t.Cleanup(func() { _ = restartCloser.Close() })
	}
	restartService := NewService(db, restartAdapter, WithConfiguredImports(restartImports), WithBuiltInEstimateSweeper())
	restartService.now = service.now
	replayed, err := restartService.RunImportSchedulerOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Checked != 1 || replayed.Imported != 0 || replayed.Reconciled != 0 || replayed.EstimateWorkers != 1 ||
		replayed.EstimateSweeps != 8 || replayed.EstimateWorkerFailures != 0 || replayed.Failed != 0 {
		t.Fatalf("restarted billing runtime scheduler summary = %#v", replayed)
	}

	replayedImport, replayedLines, replayedEstimates, replayedAuditCount := loadBillingRuntimeAcceptanceState(
		t, ctx, db, domain.TenantID, externalImportID, targetID,
	)
	if replayedImport.ID != importModel.ID || replayedImport.SourceChecksum != importModel.SourceChecksum ||
		len(replayedLines) != len(lines) || replayedAuditCount != auditCount ||
		!equalBillingRuntimeEstimateIDs(estimateIDs, billingRuntimeEstimateIDs(replayedEstimates)) {
		t.Fatalf("billing runtime restart replay changed immutable import, estimate, line, or audit identity")
	}

	if evidencePath := strings.TrimSpace(os.Getenv(billingRuntimeAcceptanceEvidenceDetailEnv)); evidencePath != "" {
		detail := map[string]any{
			"schemaVersion": "synara.billing-runtime-postgres-versioned-s3-acceptance.v1",
			"observedAt":    time.Now().UTC(),
			"boundary": map[string]any{
				"metadataStore": "postgresql", "objectStore": "versioned-s3-compatible",
				"providerFormat": "aws-cur-csv", "cloudWorkloadIdentityVerified": false,
			},
			"metrics": map[string]any{
				"invoiceImportCount": 1, "invoiceLineCount": len(lines),
				"estimatedChargeCount": len(estimates), "scheduledAuditCount": auditCount,
			},
			"assertions": map[string]bool{
				"postgresMigrationsApplied": true, "exactObjectVersionRead": true,
				"invoiceImported": true, "estimateTariffSegmentsPersisted": true,
				"reconciliationPersisted": true, "restartReplayIdempotent": true,
				"scheduledAuditIdempotent": true,
			},
		}
		if err := writeBillingRuntimeAcceptanceEvidence(evidencePath, detail); err != nil {
			t.Fatalf("write billing runtime acceptance evidence: %v", err)
		}
	}
}

func insertBillingRuntimePostgresWorker(
	t *testing.T,
	db *gorm.DB,
	base time.Time,
	tenantID uuid.UUID,
	targetID uuid.UUID,
	clusterID string,
	region string,
	namespace string,
	podName string,
	instanceUID string,
) persistence.WorkerIncarnationFact {
	t.Helper()
	workerID := uuid.New()
	terminatedAt := base.Add(2 * time.Hour)
	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: instanceUID,
		ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerMode: "general-pool",
		RegistrationTrustMode: "shared-token", ClusterID: clusterID, Namespace: namespace, PodName: podName,
		Version: "billing-runtime-acceptance", ProtocolVersion: 2, Capabilities: map[string]any{},
		CompatibilityStatus: "unknown", WorkerReleaseStatus: "unmanaged",
		LeaseSupported: true, FencingSupported: true,
		AuthTokenHash: secret.HashToken("billing-runtime-" + uuid.NewString()),
		Status:        "terminated", AdministrativeStatus: "active",
		RegisteredAt: base, LastHeartbeatAt: terminatedAt, TerminatedAt: &terminatedAt,
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}

	cpu := int64(1000)
	memory := int64(2 * 1024 * 1024 * 1024)
	ephemeral := int64(4 * 1024 * 1024 * 1024)
	reason := "completed"
	fact := persistence.WorkerIncarnationFact{
		WorkerID: workerID, WorkerIncarnation: 1, TenantID: &tenantID,
		ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerMode: "general-pool",
		ClusterID: clusterID, Region: region, Namespace: namespace, PodName: podName, InstanceUID: instanceUID,
		RegisteredAt: base, CurrentState: "terminated", StateChangedAt: terminatedAt,
		TerminatedAt: &terminatedAt, TerminalReason: &reason,
		AccumulatedActiveSeconds: 3600, AccumulatedIdleSeconds: 3600, ClaimCount: 0,
		RequestedCPUMillicores: &cpu, RequestedMemoryBytes: &memory,
		RequestedEphemeralStorageBytes: &ephemeral, CreatedAt: base, UpdatedAt: terminatedAt,
	}
	if err := db.Create(&fact).Error; err != nil {
		t.Fatal(err)
	}
	return fact
}

type billingRuntimeWorkerIdentity struct {
	resourceKey string
	clusterID   string
	region      string
	namespace   string
	podName     string
	instanceUID string
}

func requireBillingRuntimeAcceptanceInvoice(t *testing.T, invoice ImportedActualInvoice) billingRuntimeWorkerIdentity {
	t.Helper()
	expectedPeriodStart := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	expectedPeriodEnd := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	if invoice.ExternalImportID == "" || !invoice.BillingPeriodStartAt.Equal(expectedPeriodStart) ||
		!invoice.BillingPeriodEndAt.Equal(expectedPeriodEnd) || invoice.CurrencyCode != "USD" || len(invoice.Lines) != 3 {
		t.Fatalf("versioned S3 object was not the bounded AWS CUR acceptance fixture")
	}
	for _, line := range invoice.Lines {
		if line.ChargeKind != ChargeKindCPU || line.AmountMicros != 500_001 ||
			!strings.HasPrefix(line.ResourceCorrelationKey, "kubernetes:") {
			continue
		}
		parts := strings.Split(line.ResourceCorrelationKey, ":")
		if len(parts) != 6 || parts[0] != "kubernetes" {
			t.Fatalf("AWS CUR acceptance resource key is invalid")
		}
		return billingRuntimeWorkerIdentity{
			resourceKey: line.ResourceCorrelationKey, clusterID: parts[1], region: parts[2],
			namespace: parts[3], podName: parts[4], instanceUID: parts[5],
		}
	}
	t.Fatal("versioned S3 object omitted the expected tagged AWS CUR CPU line")
	return billingRuntimeWorkerIdentity{}
}

func billingRuntimeAcceptanceRegion() string {
	if value := strings.TrimSpace(os.Getenv("AWS_REGION")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION")); value != "" {
		return value
	}
	return "us-east-1"
}

func billingRuntimeAcceptanceTariffs(base time.Time, region string) []CreateTariffInput {
	firstEnd := base.Add(time.Hour)
	periodEnd := base.Add(4 * time.Hour)
	return []CreateTariffInput{
		{
			Provider: "aws", Region: region, CurrencyCode: "USD", Version: 1,
			EffectiveStartAt: base, EffectiveEndAt: &firstEnd,
			CPUCoreHourRateMicros: 250_000, MemoryGiBHourRateMicros: 375_000,
			EphemeralGiBHourRateMicros: 100_000, RequestRateMicros: 100_000, PodHourRateMicros: 100_000,
		},
		{
			Provider: "aws", Region: region, CurrencyCode: "USD", Version: 2,
			EffectiveStartAt: firstEnd, EffectiveEndAt: &periodEnd,
			CPUCoreHourRateMicros: 250_001, MemoryGiBHourRateMicros: 375_000,
			EphemeralGiBHourRateMicros: 100_000, RequestRateMicros: 100_000, PodHourRateMicros: 100_000,
		},
	}
}

func loadBillingRuntimeAcceptanceState(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID uuid.UUID,
	externalImportID string,
	targetID uuid.UUID,
) (persistence.BillingActualInvoiceImport, []persistence.BillingActualInvoiceLine, []persistence.BillingEstimatedUsageCharge, int64) {
	t.Helper()
	database := db.WithContext(ctx)
	var importModel persistence.BillingActualInvoiceImport
	if err := database.Where("tenant_id = ? AND provider = ? AND external_import_id = ?", tenantID, "aws", externalImportID).
		Take(&importModel).Error; err != nil {
		t.Fatal(err)
	}
	var lines []persistence.BillingActualInvoiceLine
	if err := database.Where("tenant_id = ? AND invoice_import_id = ?", tenantID, importModel.ID).
		Order("external_line_id").Find(&lines).Error; err != nil {
		t.Fatal(err)
	}
	var estimates []persistence.BillingEstimatedUsageCharge
	if err := database.Where("tenant_id = ? AND execution_target_id = ?", tenantID, targetID).
		Order("id").Find(&estimates).Error; err != nil {
		t.Fatal(err)
	}
	var auditEntries []persistence.AuditLog
	if err := database.
		Where("tenant_id = ? AND resource_id = ? AND action IN ?", tenantID, importModel.ID, []string{
			"billing.invoice_import_scheduled", "billing.invoice_reconciled_scheduled",
		}).Order("occurred_at, event_id").Find(&auditEntries).Error; err != nil {
		t.Fatal(err)
	}
	if len(auditEntries) > 0 {
		requestID := auditEntries[0].RequestID
		if !strings.HasPrefix(requestID, "billing-import-scheduler:") {
			t.Fatalf("billing runtime scheduled audit request ID = %q", requestID)
		}
		for _, entry := range auditEntries[1:] {
			if entry.RequestID != requestID {
				t.Fatalf("billing runtime import/reconcile audit correlation IDs differ: %q != %q", entry.RequestID, requestID)
			}
		}
	}
	return importModel, lines, estimates, int64(len(auditEntries))
}

func assertBillingRuntimeAcceptanceReconciliation(
	t *testing.T,
	lines []persistence.BillingActualInvoiceLine,
	estimates []persistence.BillingEstimatedUsageCharge,
	resourceKey string,
) {
	t.Helper()
	estimatedTotals := make(map[string]int64)
	estimatedCounts := make(map[string]int)
	for _, estimate := range estimates {
		if estimate.ResourceCorrelationKey == resourceKey {
			estimatedTotals[estimate.ChargeKind] += estimate.AmountMicros
			estimatedCounts[estimate.ChargeKind]++
		}
	}
	matchedKinds := map[string]bool{ChargeKindCPU: false, ChargeKindMemory: false}
	for _, line := range lines {
		if line.ResourceCorrelationKey != resourceKey {
			continue
		}
		if _, required := matchedKinds[line.ChargeKind]; !required {
			continue
		}
		if line.ReconciliationState != reconciliationStateMatched || line.ReconciledAt == nil ||
			line.MatchedEstimateCount != estimatedCounts[line.ChargeKind] ||
			line.MatchedEstimateAmountMicros != estimatedTotals[line.ChargeKind] ||
			line.AmountMicros != estimatedTotals[line.ChargeKind] {
			t.Fatalf("billing runtime %s reconciliation = %#v", line.ChargeKind, line)
		}
		matchedKinds[line.ChargeKind] = true
	}
	for kind, matched := range matchedKinds {
		if !matched {
			t.Fatalf("billing runtime did not persist matched %s reconciliation", kind)
		}
	}
}

func billingRuntimeEstimateIDs(estimates []persistence.BillingEstimatedUsageCharge) []string {
	ids := make([]string, 0, len(estimates))
	for _, estimate := range estimates {
		ids = append(ids, estimate.ID.String())
	}
	sort.Strings(ids)
	return ids
}

func equalBillingRuntimeEstimateIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func writeBillingRuntimeAcceptanceEvidence(path string, detail any) error {
	payload, err := json.MarshalIndent(detail, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".synara-billing-runtime-evidence-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("rename billing runtime acceptance evidence: %w", err)
	}
	return nil
}

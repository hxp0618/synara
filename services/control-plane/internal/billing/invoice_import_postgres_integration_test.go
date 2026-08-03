package billing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestPostgresConcurrentInitialInvoiceImportsSerializeByIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openBillingPostgresIntegrationDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)

	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("same checksum returns the committed identity to the scheduler path", func(t *testing.T) {
		externalImportID := "postgres-concurrent-same-" + uuid.NewString()
		invoice := postgresConcurrentInvoice(externalImportID, 1_000_000)
		service := NewService(db, NewFixtureAdapter(FixtureInvoice{
			TenantID: domain.TenantID,
			Provider: "aws",
			Invoice:  invoice,
		}))
		request := ImportActualInvoiceRequest{
			TenantID: domain.TenantID, Provider: "aws", ExternalImportID: externalImportID,
		}

		firstTx, firstBackendPID, first := stagePostgresInvoiceImport(t, ctx, db, service, request)
		defer func() { _ = firstTx.Rollback().Error }()

		type scheduledOutcome struct {
			mutation importedInvoiceMutation
			err      error
		}
		outcomes := make(chan scheduledOutcome, 1)
		done := make(chan struct{})
		go func() {
			mutation, importErr := service.importConfiguredInvoiceScheduled(ctx, ConfiguredImport{
				TenantID: domain.TenantID, Provider: "aws", ExternalImportID: externalImportID,
			}, "postgres-concurrent-scheduler:"+uuid.NewString())
			outcomes <- scheduledOutcome{mutation: mutation, err: importErr}
			close(done)
		}()

		if err := waitForPostgresBackendBlock(ctx, db, firstBackendPID, done); err != nil {
			_ = firstTx.Rollback().Error
			t.Fatal(err)
		}
		if err := firstTx.Commit().Error; err != nil {
			t.Fatal(err)
		}
		second := <-outcomes
		if second.err != nil {
			t.Fatalf("concurrent checksum-identical scheduler import: %v", second.err)
		}
		if !first.Created || second.mutation.Created ||
			second.mutation.Result.Import.ID != first.Result.Import.ID ||
			len(first.Result.Lines) != 1 || len(second.mutation.Result.Lines) != 1 ||
			second.mutation.Result.Lines[0].ID != first.Result.Lines[0].ID {
			t.Fatalf("concurrent checksum-identical imports = first %#v, second %#v", first, second.mutation)
		}
		assertPostgresInvoiceIdentityRowCounts(t, ctx, db, domain.TenantID, externalImportID, 1, 1)
	})

	t.Run("different checksum remains a stable conflict", func(t *testing.T) {
		externalImportID := "postgres-concurrent-conflict-" + uuid.NewString()
		firstService := NewService(db, NewFixtureAdapter(FixtureInvoice{
			TenantID: domain.TenantID,
			Provider: "aws",
			Invoice:  postgresConcurrentInvoice(externalImportID, 1_000_000),
		}))
		secondService := NewService(db, NewFixtureAdapter(FixtureInvoice{
			TenantID: domain.TenantID,
			Provider: "aws",
			Invoice:  postgresConcurrentInvoice(externalImportID, 2_000_000),
		}))
		request := ImportActualInvoiceRequest{
			TenantID: domain.TenantID, Provider: "aws", ExternalImportID: externalImportID,
		}

		firstTx, firstBackendPID, _ := stagePostgresInvoiceImport(t, ctx, db, firstService, request)
		defer func() { _ = firstTx.Rollback().Error }()

		type manualOutcome struct {
			result ImportActualInvoiceResult
			err    error
		}
		outcomes := make(chan manualOutcome, 1)
		done := make(chan struct{})
		go func() {
			result, importErr := secondService.ImportActualInvoice(ctx, request)
			outcomes <- manualOutcome{result: result, err: importErr}
			close(done)
		}()

		if err := waitForPostgresBackendBlock(ctx, db, firstBackendPID, done); err != nil {
			_ = firstTx.Rollback().Error
			t.Fatal(err)
		}
		if err := firstTx.Commit().Error; err != nil {
			t.Fatal(err)
		}
		second := <-outcomes
		assertProblemCode(t, second.err, "cost_accounting_invoice_import_conflict")
		assertPostgresInvoiceIdentityRowCounts(t, ctx, db, domain.TenantID, externalImportID, 1, 1)
	})
}

func stagePostgresInvoiceImport(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	service *Service,
	request ImportActualInvoiceRequest,
) (*gorm.DB, int, importedInvoiceMutation) {
	t.Helper()
	normalizedRequest, normalizedInvoice, checksum, err := service.prepareActualInvoiceImport(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	var backendPID int
	if err := tx.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
		_ = tx.Rollback().Error
		t.Fatal(err)
	}
	mutation, err := service.importActualInvoiceTx(ctx, tx, normalizedRequest, normalizedInvoice, checksum)
	if err != nil {
		_ = tx.Rollback().Error
		t.Fatal(err)
	}
	return tx, backendPID, mutation
}

func waitForPostgresBackendBlock(
	ctx context.Context,
	db *gorm.DB,
	blockingBackendPID int,
	done <-chan struct{},
) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := db.WithContext(ctx).Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE ? = ANY(pg_blocking_pids(pid))
			)
		`, blockingBackendPID).Scan(&blocked).Error; err != nil {
			return err
		}
		if blocked {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("concurrent invoice import returned before waiting for the first identity transaction")
		case <-ctx.Done():
			return fmt.Errorf("wait for concurrent invoice import to block: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertPostgresInvoiceIdentityRowCounts(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	tenantID uuid.UUID,
	externalImportID string,
	wantImports int64,
	wantLines int64,
) {
	t.Helper()
	var invoiceImport persistence.BillingActualInvoiceImport
	if err := db.WithContext(ctx).
		Where("tenant_id = ? AND provider = ? AND external_import_id = ?", tenantID, "aws", externalImportID).
		Take(&invoiceImport).Error; err != nil {
		t.Fatal(err)
	}
	var importCount int64
	if err := db.WithContext(ctx).Model(&persistence.BillingActualInvoiceImport{}).
		Where("tenant_id = ? AND provider = ? AND external_import_id = ?", tenantID, "aws", externalImportID).
		Count(&importCount).Error; err != nil {
		t.Fatal(err)
	}
	var lineCount int64
	if err := db.WithContext(ctx).Model(&persistence.BillingActualInvoiceLine{}).
		Where("tenant_id = ? AND invoice_import_id = ?", tenantID, invoiceImport.ID).
		Count(&lineCount).Error; err != nil {
		t.Fatal(err)
	}
	if importCount != wantImports || lineCount != wantLines {
		t.Fatalf("PostgreSQL invoice identity rows = imports %d, lines %d; want %d/%d", importCount, lineCount, wantImports, wantLines)
	}
}

func postgresConcurrentInvoice(externalImportID string, amountMicros int64) ImportedActualInvoice {
	periodStart := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	return ImportedActualInvoice{
		ExternalImportID:     externalImportID,
		BillingPeriodStartAt: periodStart,
		BillingPeriodEndAt:   periodStart.AddDate(0, 1, 0),
		CurrencyCode:         "USD",
		Lines: []ImportedActualInvoiceLine{{
			ExternalLineID:         "cpu",
			ChargeKind:             ChargeKindCPU,
			ResourceCorrelationKey: "kubernetes:cluster-a:us-east-1:default:worker-a:instance-a",
			AmountMicros:           amountMicros,
		}},
	}
}

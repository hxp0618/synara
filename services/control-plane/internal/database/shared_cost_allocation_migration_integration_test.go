package database

import (
	"context"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresSharedCostAllocationMigrationInstallsRetainedLedgerShape(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"billing_shared_target_ledger_coverages",
		"billing_shared_cost_allocation_runs",
		"billing_shared_estimated_charge_slices",
	} {
		var installed bool
		if err := db.Raw(`SELECT to_regclass(?) IS NOT NULL`, table).Scan(&installed).Error; err != nil {
			t.Fatal(err)
		}
		if !installed {
			t.Fatalf("PostgreSQL shared allocation table %s was not installed", table)
		}
	}

	for _, trigger := range []string{
		"trg_billing_shared_target_ledger_coverages_scope",
		"trg_billing_shared_cost_allocation_runs_scope",
		"trg_billing_shared_estimated_charge_slices_scope",
		"trg_billing_shared_target_ledger_coverages_immutable",
		"trg_billing_shared_cost_allocation_runs_immutable",
		"trg_billing_shared_estimated_charge_slices_immutable",
	} {
		var count int64
		if err := db.Raw(`SELECT count(*) FROM pg_trigger WHERE tgname = ? AND NOT tgisinternal`, trigger).
			Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("PostgreSQL shared allocation trigger %s count = %d, want 1", trigger, count)
		}
	}

	var semanticIndex bool
	if err := db.Raw(`SELECT to_regclass('uq_billing_shared_estimated_charge_slices_semantic') IS NOT NULL`).
		Scan(&semanticIndex).Error; err != nil {
		t.Fatal(err)
	}
	if !semanticIndex {
		t.Fatal("PostgreSQL shared allocation semantic slice uniqueness index was not installed")
	}
}

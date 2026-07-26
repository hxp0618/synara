package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestBillingSharedAllocationScheduleMetricsExposeBoundedDueState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.BillingSharedAllocationSchedulePeriod{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for index, row := range []persistence.BillingSharedAllocationSchedulePeriod{
		{
			ScheduleConfigSHA256: strings.Repeat("a", 64),
			BillingPeriodStartAt: now.AddDate(0, -1, 0), BillingPeriodEndAt: now,
			ExecutionTargetID: uuid.New(), Provider: "aws", CurrencyCode: "USD", ScheduleKind: "monthly-utc",
			SettlementDelaySeconds: 3600, ScheduleIntervalSeconds: 21600,
			NextAttemptAt: now.Add(-time.Hour), AttemptCount: 1,
			LastOutcome: "failed", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		},
		{
			ScheduleConfigSHA256: strings.Repeat("b", 64),
			BillingPeriodStartAt: now.Add(-2 * time.Hour), BillingPeriodEndAt: now.Add(-time.Hour),
			ExecutionTargetID: uuid.New(), Provider: "gcp", CurrencyCode: "USD", ScheduleKind: "static",
			SettlementDelaySeconds: 3600, ScheduleIntervalSeconds: 21600,
			NextAttemptAt: now.Add(time.Hour), LastOutcome: "never",
			CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("create schedule metric row %d: %v", index, err)
		}
	}
	var output bytes.Buffer
	if err := New(db).writeBillingSharedAllocationScheduleMetrics(context.Background(), &output, now); err != nil {
		t.Fatal(err)
	}
	metrics := output.String()
	for _, expected := range []string{
		`synara_billing_shared_allocation_schedule_periods{due_state="due",outcome="failed",schedule_kind="monthly-utc"} 1`,
		`synara_billing_shared_allocation_schedule_periods{due_state="waiting",outcome="never",schedule_kind="static"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("billing shared schedule metrics omitted %q:\n%s", expected, metrics)
		}
	}
}

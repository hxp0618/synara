package tenancy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestAuditLogFilteringPaginationExportAndIsolation(t *testing.T) {
	ctx := context.Background()
	platformConfig, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "audit-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	otherTenant, err := service.CreateTenant(ctx, owner, CreateTenantInput{
		Slug: "audit-other-" + uuid.NewString()[:8], Name: "Other audit tenant", Region: "default", PlanCode: "free",
	}, "audit-other-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	entries := []persistence.AuditLog{
		{EventID: uuid.New(), TenantID: domain.TenantID, ActorType: "user", ActorID: &domain.UserID, Action: "session.created", ResourceType: "agent_session", RequestID: "audit-session-1", Metadata: map[string]any{}, OccurredAt: base.Add(time.Minute)},
		{EventID: uuid.New(), TenantID: domain.TenantID, ActorType: "worker", Action: "artifact.ready", ResourceType: "artifact", RequestID: "audit-artifact-1", Metadata: map[string]any{}, OccurredAt: base.Add(2 * time.Minute)},
		{EventID: uuid.New(), TenantID: domain.TenantID, ActorType: "user", ActorID: &domain.UserID, Action: "session.created", ResourceType: "agent_session", RequestID: "audit-session-2", Metadata: map[string]any{}, OccurredAt: base.Add(3 * time.Minute)},
		{EventID: uuid.New(), TenantID: otherTenant.ID, ActorType: "system", Action: "other.secret", ResourceType: "tenant", RequestID: "audit-other-secret", Metadata: map[string]any{}, OccurredAt: base.Add(4 * time.Minute)},
	}
	for _, entry := range entries {
		if err := store.DB().Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}

	first, err := service.ListAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("unexpected first audit page: %#v", first)
	}
	second, err := service.ListAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{Limit: 2, Cursor: *first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]struct{}{}
	for _, entry := range append(first.Items, second.Items...) {
		if entry.TenantID != domain.TenantID || entry.Action == "other.secret" {
			t.Fatalf("audit query crossed tenant boundary: %#v", entry)
		}
		if _, duplicate := seen[entry.EventID]; duplicate {
			t.Fatalf("audit cursor repeated event %s", entry.EventID)
		}
		seen[entry.EventID] = struct{}{}
	}
	_, err = service.ListAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{
		Limit: 2, Cursor: *first.NextCursor, Action: "session.created",
	})
	assertAuditProblemCode(t, err, "invalid_audit_cursor")
	_, err = service.ListAuditLogs(ctx, owner, otherTenant.ID, AuditLogQuery{Limit: 2, Cursor: *first.NextCursor})
	assertAuditProblemCode(t, err, "invalid_audit_cursor")

	filtered, err := service.ListAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{Action: "session.created"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 2 {
		t.Fatalf("expected two filtered session audit rows, got %#v", filtered.Items)
	}

	exported := make([]AuditLogEntry, 0)
	if err := service.ExportAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{Action: "session.created"}, "jsonl", "audit-export", "127.0.0.1", func(entry AuditLogEntry) error {
		exported = append(exported, entry)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(exported) != 2 {
		t.Fatalf("expected two exported rows, got %#v", exported)
	}
	var exportAuditRows int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action IN ? AND request_id = ?", domain.TenantID, []string{"audit.export_started", "audit.export_completed"}, "audit-export").
		Count(&exportAuditRows).Error; err != nil {
		t.Fatal(err)
	}
	if exportAuditRows != 2 {
		t.Fatalf("audit export was not itself audited: %d", exportAuditRows)
	}

	bulk := make([]persistence.AuditLog, 0, 1_005)
	for index := range 1_005 {
		bulk = append(bulk, persistence.AuditLog{
			EventID: uuid.New(), TenantID: domain.TenantID, ActorType: "system",
			Action: "bulk.event", ResourceType: "audit_test", RequestID: "audit-bulk-" + uuid.NewString(),
			Metadata: map[string]any{"index": index}, OccurredAt: base.Add(time.Duration(index) * time.Microsecond),
		})
	}
	if err := store.DB().CreateInBatches(&bulk, 200).Error; err != nil {
		t.Fatal(err)
	}
	bulkExported := 0
	if err := service.ExportAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{Action: "bulk.event"}, "jsonl", "audit-bulk-export", "127.0.0.1", func(AuditLogEntry) error {
		bulkExported++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if bulkExported != len(bulk) {
		t.Fatalf("batched audit export returned %d rows, want %d", bulkExported, len(bulk))
	}

	memberID := uuid.New()
	now := time.Now().UTC()
	if err := store.DB().Create(&persistence.User{
		ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Audit member", Status: "active", EmailVerifiedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.ListAuditLogs(ctx, identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID}, domain.TenantID, AuditLogQuery{})
	assertAuditProblemCode(t, err, "tenant_forbidden")
	_, err = service.ListAuditLogs(ctx, owner, domain.TenantID, AuditLogQuery{Cursor: "not-a-cursor"})
	assertAuditProblemCode(t, err, "invalid_audit_cursor")
}

func assertAuditProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}

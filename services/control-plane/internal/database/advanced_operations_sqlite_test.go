package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type advancedOperationsSQLiteFixture struct {
	db             *gorm.DB
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	userID         uuid.UUID
	targetID       uuid.UUID
}

func TestSQLiteAdvancedOperationsSafetyMirrorsForkAndPrimaryCommandInvariants(t *testing.T) {
	fixture := openAdvancedOperationsSQLiteFixture(t)

	for _, name := range []string{
		"uq_execution_control_commands_primary_operation",
		"trg_agent_turns_advanced_shape_insert",
		"trg_agent_sessions_fork_lineage_insert",
		"trg_execution_control_commands_primary_kind_insert",
		"trg_execution_control_commands_primary_preserved_delete",
		"trg_agent_executions_control_commands_cascade",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite safety object %s count = %d, want 1", name, count)
		}
	}

	rootID := uuid.New()
	if err := fixture.createSession(rootID, 2, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	firstTurnID, secondTurnID := uuid.New(), uuid.New()
	for _, turn := range []struct {
		id    uuid.UUID
		input string
	}{
		{id: firstTurnID, input: "first"},
		{id: secondTurnID, input: "second"},
	} {
		if err := fixture.db.Create(&persistence.AgentTurn{
			ID: turn.id, TenantID: fixture.tenantID, SessionID: rootID, CreatedBy: fixture.userID,
			Status: "queued", InputText: turn.input, TurnKind: "message",
			RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for index, turnID := range []uuid.UUID{firstTurnID, secondTurnID} {
		if err := fixture.db.Create(&persistence.SessionEvent{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: rootID, Sequence: int64(index + 1), EventID: uuid.New(), EventVersion: 1,
			EventType: "turn.created", ActorType: "user", ActorID: &fixture.userID,
			Payload: map[string]any{"turnId": turnID.String()}, OccurredAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	strategy := "emulated"
	prefix := int64(1)
	firstForkID := uuid.New()
	if err := fixture.createSession(firstForkID, prefix, &rootID, &firstTurnID, &prefix, &strategy); err != nil {
		t.Fatalf("SQLite rejected valid first Fork: %v", err)
	}
	secondForkID := uuid.New()
	if err := fixture.createSession(secondForkID, prefix, &firstForkID, &firstTurnID, &prefix, &strategy); err != nil {
		t.Fatalf("SQLite rejected valid Fork-of-Fork ancestor Turn: %v", err)
	}
	if err := fixture.createSession(uuid.New(), prefix, &firstForkID, &secondTurnID, &prefix, &strategy); err == nil {
		t.Fatal("SQLite accepted a Fork source Turn outside the logical prefix")
	}
	if err := fixture.createSession(uuid.New(), prefix, &rootID, nil, &prefix, nil); err == nil {
		t.Fatal("SQLite accepted partial Fork lineage metadata")
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, firstForkID).
		Update("fork_strategy", "native").Error; err == nil {
		t.Fatal("SQLite allowed immutable Fork lineage to change")
	}

	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: rootID, CreatedBy: fixture.userID,
		Status: "queued", InputText: "invalid", TurnKind: "review",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("SQLite accepted non-empty input for a Review Turn")
	}

	operationSessionID := uuid.New()
	if err := fixture.createSession(operationSessionID, 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	turnID, executionID := uuid.New(), uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: operationSessionID, CreatedBy: fixture.userID,
		Status: "queued", InputText: "", TurnKind: "compact",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: fixture.tenantID, SessionID: operationSessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: fixture.targetID, TargetKind: "local",
		Generation: 0, RequestedBy: fixture.userID, QueuedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	command := fixture.primaryCommand(executionID, operationSessionID, turnID, "CompactSession")
	if err := fixture.db.Create(&command).Error; err != nil {
		t.Fatalf("SQLite rejected matching primary command: %v", err)
	}
	mismatch := fixture.primaryCommand(executionID, operationSessionID, turnID, "StartReview")
	if err := fixture.db.Create(&mismatch).Error; err == nil {
		t.Fatal("SQLite accepted a primary command that does not match the Turn kind")
	}
	duplicate := fixture.primaryCommand(executionID, operationSessionID, turnID, "CompactSession")
	if err := fixture.db.Create(&duplicate).Error; err == nil {
		t.Fatal("SQLite accepted two primary operations for one Execution")
	}
	if err := fixture.db.Model(&persistence.ExecutionControlCommand{}).
		Where("id = ?", command.ID).Update("command_type", "SteerTurn").Error; err == nil {
		t.Fatal("SQLite allowed a primary command to be rewritten")
	}
	if err := fixture.db.Where("id = ?", command.ID).
		Delete(&persistence.ExecutionControlCommand{}).Error; err == nil {
		t.Fatal("SQLite allowed a primary command to be deleted directly")
	}
	if err := fixture.db.Where("id = ?", executionID).Delete(&persistence.AgentExecution{}).Error; err != nil {
		t.Fatalf("SQLite parent Execution cascade failed: %v", err)
	}
	var remainingCommands int64
	if err := fixture.db.Model(&persistence.ExecutionControlCommand{}).
		Where("execution_id = ?", executionID).Count(&remainingCommands).Error; err != nil {
		t.Fatal(err)
	}
	if remainingCommands != 0 {
		t.Fatalf("SQLite parent Execution cascade retained %d commands", remainingCommands)
	}
}

func openAdvancedOperationsSQLiteFixture(t *testing.T) advancedOperationsSQLiteFixture {
	t.Helper()
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-advanced-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "SQLite advanced operations", DefaultBranch: "main", Visibility: "organization",
		CreatedBy: domain.UserID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	return advancedOperationsSQLiteFixture{
		db: store.DB(), tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, userID: domain.UserID, targetID: domain.ExecutionTargetID,
	}
}

func (fixture advancedOperationsSQLiteFixture) createSession(
	id uuid.UUID,
	lastEventSequence int64,
	sourceSessionID, sourceTurnID *uuid.UUID,
	sourceEventSequence *int64,
	strategy *string,
) error {
	return fixture.db.Create(&persistence.AgentSession{
		ID: id, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, CreatedBy: fixture.userID,
		Title: "SQLite advanced " + id.String(), Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: fixture.targetID, ProviderResumeCursorState: "absent",
		ForkSourceSessionID: sourceSessionID, ForkSourceTurnID: sourceTurnID,
		ForkSourceEventSequence: sourceEventSequence, ForkStrategy: strategy,
		LastEventSequence: lastEventSequence, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error
}

func (fixture advancedOperationsSQLiteFixture) primaryCommand(
	executionID, sessionID, turnID uuid.UUID,
	commandType string,
) persistence.ExecutionControlCommand {
	now := time.Now().UTC()
	id := uuid.New()
	return persistence.ExecutionControlCommand{
		ID: id, TenantID: fixture.tenantID, ExecutionID: executionID, SessionID: sessionID,
		TurnID: turnID, Provider: "codex", CommandType: commandType,
		CommandID: "sqlite:" + id.String(), Payload: map[string]any{}, Status: "pending",
		RequestedBy: fixture.userID, RequestedAt: now, DeliveryAvailableAt: now,
	}
}

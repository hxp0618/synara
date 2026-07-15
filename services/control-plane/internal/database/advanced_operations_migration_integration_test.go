package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type advancedOperationsMigrationFixture struct {
	db             *gorm.DB
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	userID         uuid.UUID
	targetID       uuid.UUID
	targetKind     string
}

func TestAdvancedOperationsMigrationRejectsDeferredForkCycles(t *testing.T) {
	fixture := openAdvancedOperationsMigrationFixture(t)
	firstID, secondID, thirdID := uuid.New(), uuid.New(), uuid.New()
	sequence := int64(0)
	strategy := "emulated"

	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		for _, item := range []struct {
			id       uuid.UUID
			sourceID uuid.UUID
		}{
			{id: firstID, sourceID: secondID},
			{id: secondID, sourceID: thirdID},
			{id: thirdID, sourceID: firstID},
		} {
			if err := fixture.insertSession(tx, item.id, 0, &item.sourceID, nil, &sequence, &strategy); err != nil {
				return err
			}
		}
		return nil
	})
	assertAdvancedMigrationRejected(t, err, "Fork lineage cannot contain a Session cycle")

	var persisted int64
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("id IN ?", []uuid.UUID{firstID, secondID, thirdID}).Count(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("deferred Fork cycle transaction persisted %d Sessions", persisted)
	}
}

func TestAdvancedOperationsMigrationValidatesForkShapeAndLogicalAncestorTurn(t *testing.T) {
	fixture := openAdvancedOperationsMigrationFixture(t)
	rootID := uuid.New()
	if err := fixture.insertSession(fixture.db, rootID, 2, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	firstTurnID, secondTurnID := uuid.New(), uuid.New()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := fixture.insertTurn(tx, rootID, firstTurnID, "message", "first"); err != nil {
			return err
		}
		if err := fixture.insertTurn(tx, rootID, secondTurnID, "message", "second"); err != nil {
			return err
		}
		if err := fixture.insertTurnEvent(tx, rootID, firstTurnID, 1); err != nil {
			return err
		}
		return fixture.insertTurnEvent(tx, rootID, secondTurnID, 2)
	}); err != nil {
		t.Fatal(err)
	}

	strategy := "emulated"
	prefix := int64(1)
	firstForkID := uuid.New()
	if err := fixture.insertSession(
		fixture.db, firstForkID, prefix, &rootID, &firstTurnID, &prefix, &strategy,
	); err != nil {
		t.Fatalf("create first logical Fork: %v", err)
	}
	secondForkID := uuid.New()
	if err := fixture.insertSession(
		fixture.db, secondForkID, prefix, &firstForkID, &firstTurnID, &prefix, &strategy,
	); err != nil {
		t.Fatalf("create Fork-of-Fork with ancestor source Turn: %v", err)
	}

	invalidForkID := uuid.New()
	err := fixture.insertSession(
		fixture.db, invalidForkID, prefix, &firstForkID, &secondTurnID, &prefix, &strategy,
	)
	assertAdvancedMigrationRejected(t, err, "outside the selected logical source Session history prefix")

	missingStrategyID := uuid.New()
	err = fixture.insertSession(fixture.db, missingStrategyID, prefix, &rootID, nil, &prefix, nil)
	assertAdvancedMigrationRejected(t, err, "chk_agent_sessions_fork_lineage_shape")

	zero := int64(0)
	zeroBoundaryTurnID := uuid.New()
	err = fixture.insertSession(
		fixture.db, uuid.New(), 0, &rootID, &zeroBoundaryTurnID, &zero, &strategy,
	)
	assertAdvancedMigrationRejected(t, err, "chk_agent_sessions_fork_lineage_shape")

	beyondSource := int64(3)
	err = fixture.insertSession(
		fixture.db, uuid.New(), beyondSource, &rootID, nil, &beyondSource, &strategy,
	)
	assertAdvancedMigrationRejected(t, err, "outside the source Session history")
}

func TestAdvancedOperationsMigrationValidatesTurnAndPrimaryCommandShape(t *testing.T) {
	fixture := openAdvancedOperationsMigrationFixture(t)
	sessionID := uuid.New()
	if err := fixture.insertSession(fixture.db, sessionID, 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	err := fixture.insertTurn(fixture.db, sessionID, uuid.New(), "compact", "must be empty")
	assertAdvancedMigrationRejected(t, err, "chk_agent_turns_input_shape")
	err = fixture.insertTurn(fixture.db, sessionID, uuid.New(), "message", "")
	assertAdvancedMigrationRejected(t, err, "chk_agent_turns_input_shape")

	operation := fixture.insertPrimaryOperation(t, sessionID, "compact", "CompactSession", `{}`)
	assertMigrationIndex(
		t,
		fixture.db,
		"uq_execution_control_commands_primary_operation",
		"tenant_id,execution_id",
		"command_type = ANY",
	)

	err = fixture.db.Model(&persistence.AgentTurn{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, operation.turnID).
		Update("turn_kind", "review").Error
	assertAdvancedMigrationRejected(t, err, "Turn kind is immutable")

	err = fixture.insertCommand(
		fixture.db, operation.executionID, sessionID, operation.turnID, "CompactSession", `{}`,
	)
	assertAdvancedMigrationRejected(t, err, "uq_execution_control_commands_primary_operation")

	mismatchSessionID := uuid.New()
	if err := fixture.insertSession(fixture.db, mismatchSessionID, 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	mismatchTurnID, mismatchExecutionID := uuid.New(), uuid.New()
	if err := fixture.insertTurn(fixture.db, mismatchSessionID, mismatchTurnID, "message", "message"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.insertExecution(fixture.db, mismatchSessionID, mismatchTurnID, mismatchExecutionID); err != nil {
		t.Fatal(err)
	}
	err = fixture.insertCommand(
		fixture.db, mismatchExecutionID, mismatchSessionID, mismatchTurnID, "StartReview", `{}`,
	)
	assertAdvancedMigrationRejected(t, err, "does not match the Execution Turn kind")

	arrayPayloadSessionID := uuid.New()
	if err := fixture.insertSession(fixture.db, arrayPayloadSessionID, 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		turnID, executionID := uuid.New(), uuid.New()
		if err := fixture.insertTurn(tx, arrayPayloadSessionID, turnID, "review", ""); err != nil {
			return err
		}
		if err := fixture.insertExecution(tx, arrayPayloadSessionID, turnID, executionID); err != nil {
			return err
		}
		return fixture.insertCommand(tx, executionID, arrayPayloadSessionID, turnID, "StartReview", `[]`)
	})
	assertAdvancedMigrationRejected(t, err, "chk_execution_control_commands_payload_object")

	nullPayloadSessionID := uuid.New()
	if err := fixture.insertSession(fixture.db, nullPayloadSessionID, 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		turnID, executionID := uuid.New(), uuid.New()
		if err := fixture.insertTurn(tx, nullPayloadSessionID, turnID, "review", ""); err != nil {
			return err
		}
		if err := fixture.insertExecution(tx, nullPayloadSessionID, turnID, executionID); err != nil {
			return err
		}
		return fixture.insertCommand(tx, executionID, nullPayloadSessionID, turnID, "StartReview", `null`)
	})
	assertAdvancedMigrationRejected(t, err, "chk_execution_control_commands_payload_object")
}

func TestAdvancedOperationsMigrationPreservesPrimaryCommandAndAllowsExecutionCascade(t *testing.T) {
	fixture := openAdvancedOperationsMigrationFixture(t)
	sessionID := uuid.New()
	if err := fixture.insertSession(fixture.db, sessionID, 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	operation := fixture.insertPrimaryOperation(t, sessionID, "review", "StartReview", `{}`)

	err := fixture.db.Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, operation.executionID).
		Update("command_type", "SteerTurn").Error
	assertAdvancedMigrationRejected(t, err, "must retain its matching primary Control command")

	err = fixture.db.Where(
		"tenant_id = ? AND execution_id = ?", fixture.tenantID, operation.executionID,
	).Delete(&persistence.ExecutionControlCommand{}).Error
	assertAdvancedMigrationRejected(t, err, "must retain its matching primary Control command")

	if err := fixture.db.Where(
		"tenant_id = ? AND id = ?", fixture.tenantID, operation.executionID,
	).Delete(&persistence.AgentExecution{}).Error; err != nil {
		t.Fatalf("delete parent Execution with cascading primary command: %v", err)
	}
	var commands int64
	if err := fixture.db.Model(&persistence.ExecutionControlCommand{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, operation.executionID).
		Count(&commands).Error; err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("parent Execution cascade retained %d primary commands", commands)
	}
}

type advancedMigrationOperation struct {
	turnID      uuid.UUID
	executionID uuid.UUID
}

func openAdvancedOperationsMigrationFixture(t *testing.T) advancedOperationsMigrationFixture {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_ADVANCED_OPERATIONS_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_ADVANCED_OPERATIONS_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000031_session_execution_cursor_lineage.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db.Config.TranslateError = false
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	return advancedOperationsMigrationFixture{
		db: db, tenantID: seed.tenantID, organizationID: session.OrganizationID,
		projectID: session.ProjectID, userID: execution.RequestedBy,
		targetID: execution.ExecutionTargetID, targetKind: execution.TargetKind,
	}
}

func (fixture advancedOperationsMigrationFixture) insertSession(
	db *gorm.DB,
	id uuid.UUID,
	lastEventSequence int64,
	sourceSessionID, sourceTurnID *uuid.UUID,
	sourceEventSequence *int64,
	strategy *string,
) error {
	return db.Exec(`
		INSERT INTO agent_sessions (
			id, tenant_id, organization_id, project_id, created_by, title, status, visibility,
			provider, execution_target_id, provider_resume_cursor_state, last_event_sequence,
			fork_source_session_id, fork_source_turn_id, fork_source_event_sequence, fork_strategy,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'active', 'private', 'codex', ?, 'absent', ?, ?, ?, ?, ?, ?, ?)
	`,
		id, fixture.tenantID, fixture.organizationID, fixture.projectID, fixture.userID,
		"Advanced migration "+id.String(), fixture.targetID, lastEventSequence,
		sourceSessionID, sourceTurnID, sourceEventSequence, strategy,
		time.Now().UTC(), time.Now().UTC(),
	).Error
}

func (fixture advancedOperationsMigrationFixture) insertTurn(
	db *gorm.DB,
	sessionID, turnID uuid.UUID,
	turnKind, inputText string,
) error {
	return db.Exec(`
		INSERT INTO agent_turns (
			id, tenant_id, session_id, created_by, status, input_text, turn_kind,
			runtime_mode, interaction_mode, created_at
		) VALUES (?, ?, ?, ?, 'queued', ?, ?, 'full-access', 'default', ?)
	`, turnID, fixture.tenantID, sessionID, fixture.userID, inputText, turnKind, time.Now().UTC()).Error
}

func (fixture advancedOperationsMigrationFixture) insertTurnEvent(
	db *gorm.DB,
	sessionID, turnID uuid.UUID,
	sequence int64,
) error {
	payload := fmt.Sprintf(`{"turnId":%q}`, turnID.String())
	return db.Exec(`
		INSERT INTO session_events (
			tenant_id, organization_id, project_id, session_id, sequence, event_id,
			event_version, event_type, actor_type, actor_id, payload, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, 1, 'turn.created', 'user', ?, CAST(? AS jsonb), ?)
	`,
		fixture.tenantID, fixture.organizationID, fixture.projectID, sessionID, sequence,
		uuid.New(), fixture.userID, payload, time.Now().UTC(),
	).Error
}

func (fixture advancedOperationsMigrationFixture) insertExecution(
	db *gorm.DB,
	sessionID, turnID, executionID uuid.UUID,
) error {
	return db.Exec(`
		INSERT INTO agent_executions (
			id, tenant_id, session_id, turn_id, attempt, status, execution_target_id,
			target_kind, provider, generation, requested_by, queued_at
		) VALUES (?, ?, ?, ?, 1, 'queued', ?, ?, 'codex', 0, ?, ?)
	`,
		executionID, fixture.tenantID, sessionID, turnID, fixture.targetID, fixture.targetKind,
		fixture.userID, time.Now().UTC(),
	).Error
}

func (fixture advancedOperationsMigrationFixture) insertCommand(
	db *gorm.DB,
	executionID, sessionID, turnID uuid.UUID,
	commandType, payload string,
) error {
	commandID := uuid.New()
	now := time.Now().UTC()
	return db.Exec(`
		INSERT INTO execution_control_commands (
			id, tenant_id, execution_id, session_id, turn_id, provider, command_type,
			command_id, payload, status, requested_by, requested_at, delivery_attempts,
			delivery_available_at
		) VALUES (?, ?, ?, ?, ?, 'codex', ?, ?, CAST(? AS jsonb), 'pending', ?, ?, 0, ?)
	`,
		commandID, fixture.tenantID, executionID, sessionID, turnID, commandType,
		"advanced:"+commandID.String(), payload, fixture.userID, now, now,
	).Error
}

func (fixture advancedOperationsMigrationFixture) insertPrimaryOperation(
	t *testing.T,
	sessionID uuid.UUID,
	turnKind, commandType, payload string,
) advancedMigrationOperation {
	t.Helper()
	operation := advancedMigrationOperation{turnID: uuid.New(), executionID: uuid.New()}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := fixture.insertTurn(tx, sessionID, operation.turnID, turnKind, ""); err != nil {
			return err
		}
		if err := fixture.insertExecution(tx, sessionID, operation.turnID, operation.executionID); err != nil {
			return err
		}
		return fixture.insertCommand(tx, operation.executionID, sessionID, operation.turnID, commandType, payload)
	}); err != nil {
		t.Fatalf("insert primary operation: %v", err)
	}
	return operation
}

func assertAdvancedMigrationRejected(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("migration accepted invalid state or returned wrong error: %v (want containing %q)", err, expected)
	}
}

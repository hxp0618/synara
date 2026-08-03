package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateRuntimeSecretRekeySQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_runtime_secret_rekey_entries_run_resource
		 ON runtime_secret_rekey_entries (run_id, resource_type, resource_id)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_secret_rekey_entries_resource
		 ON runtime_secret_rekey_entries (resource_type, resource_id, created_at, id)`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_runtime_key_insert`,
		`CREATE TRIGGER trg_execution_targets_runtime_key_insert
		 BEFORE INSERT ON execution_targets
		 WHEN NEW.configuration_key_id IS NOT NULL AND (
		   length(trim(NEW.configuration_key_id)) NOT BETWEEN 1 AND 200
		   OR instr(NEW.configuration_key_id, char(10)) > 0
		   OR instr(NEW.configuration_key_id, char(13)) > 0
		   OR instr(NEW.configuration_key_id, char(9)) > 0
		   OR length(NEW.configuration_encrypted) = 0
		 )
		 BEGIN SELECT RAISE(ABORT, 'invalid Execution Target runtime Secret key ID'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_runtime_key_update`,
		`CREATE TRIGGER trg_execution_targets_runtime_key_update
		 BEFORE UPDATE OF configuration_encrypted, configuration_key_id ON execution_targets
		 WHEN NEW.configuration_key_id IS NOT NULL AND (
		   length(trim(NEW.configuration_key_id)) NOT BETWEEN 1 AND 200
		   OR instr(NEW.configuration_key_id, char(10)) > 0
		   OR instr(NEW.configuration_key_id, char(13)) > 0
		   OR instr(NEW.configuration_key_id, char(9)) > 0
		   OR length(NEW.configuration_encrypted) = 0
		 )
		 BEGIN SELECT RAISE(ABORT, 'invalid Execution Target runtime Secret key ID'); END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_runtime_key_insert`,
		`CREATE TRIGGER trg_agent_sessions_runtime_key_insert
		 BEFORE INSERT ON agent_sessions
		 WHEN NEW.provider_resume_cursor_key_id IS NOT NULL AND (
		   length(trim(NEW.provider_resume_cursor_key_id)) NOT BETWEEN 1 AND 200
		   OR instr(NEW.provider_resume_cursor_key_id, char(10)) > 0
		   OR instr(NEW.provider_resume_cursor_key_id, char(13)) > 0
		   OR instr(NEW.provider_resume_cursor_key_id, char(9)) > 0
		   OR length(NEW.provider_resume_cursor_encrypted) = 0
		 )
		 BEGIN SELECT RAISE(ABORT, 'invalid Provider Cursor runtime Secret key ID'); END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_runtime_key_update`,
		`CREATE TRIGGER trg_agent_sessions_runtime_key_update
		 BEFORE UPDATE OF provider_resume_cursor_encrypted, provider_resume_cursor_key_id ON agent_sessions
		 WHEN NEW.provider_resume_cursor_key_id IS NOT NULL AND (
		   length(trim(NEW.provider_resume_cursor_key_id)) NOT BETWEEN 1 AND 200
		   OR instr(NEW.provider_resume_cursor_key_id, char(10)) > 0
		   OR instr(NEW.provider_resume_cursor_key_id, char(13)) > 0
		   OR instr(NEW.provider_resume_cursor_key_id, char(9)) > 0
		   OR length(NEW.provider_resume_cursor_encrypted) = 0
		 )
		 BEGIN SELECT RAISE(ABORT, 'invalid Provider Cursor runtime Secret key ID'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_runtime_key_guard`,
		`CREATE TRIGGER trg_execution_targets_runtime_key_guard
		 BEFORE UPDATE OF configuration_encrypted, configuration_key_id ON execution_targets
		 WHEN NEW.configuration_key_id IS NOT OLD.configuration_key_id
		   AND NEW.updated_at IS OLD.updated_at
		   AND NOT EXISTS (
		     SELECT 1
		     FROM runtime_secret_rekey_entries AS entry
		     JOIN runtime_secret_rekey_runs AS run ON run.id = entry.run_id
		     LEFT JOIN runtime_secret_rekey_receipts AS receipt ON receipt.run_id = run.id
		     WHERE receipt.run_id IS NULL
		       AND entry.resource_type = 'execution_target_configuration'
		       AND entry.resource_id IS OLD.id
		       AND entry.tenant_id IS OLD.tenant_id
		       AND entry.old_stored_key_id IS OLD.configuration_key_id
		       AND entry.new_key_id IS NEW.configuration_key_id
		       AND run.primary_key_id IS NEW.configuration_key_id
		   )
		 BEGIN SELECT RAISE(ABORT, 'Execution Target runtime Secret key change requires rekey evidence'); END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_runtime_key_guard`,
		`CREATE TRIGGER trg_agent_sessions_runtime_key_guard
		 BEFORE UPDATE OF provider_resume_cursor_encrypted, provider_resume_cursor_key_id ON agent_sessions
		 WHEN NEW.provider_resume_cursor_key_id IS NOT OLD.provider_resume_cursor_key_id
		   AND NEW.updated_at IS OLD.updated_at
		   AND NOT EXISTS (
		     SELECT 1
		     FROM runtime_secret_rekey_entries AS entry
		     JOIN runtime_secret_rekey_runs AS run ON run.id = entry.run_id
		     LEFT JOIN runtime_secret_rekey_receipts AS receipt ON receipt.run_id = run.id
		     WHERE receipt.run_id IS NULL
		       AND entry.resource_type = 'provider_resume_cursor'
		       AND entry.resource_id IS OLD.id
		       AND entry.tenant_id IS OLD.tenant_id
		       AND entry.old_stored_key_id IS OLD.provider_resume_cursor_key_id
		       AND entry.new_key_id IS NEW.provider_resume_cursor_key_id
		       AND run.primary_key_id IS NEW.provider_resume_cursor_key_id
		   )
		 BEGIN SELECT RAISE(ABORT, 'Provider Cursor runtime Secret key change requires rekey evidence'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_runs_insert`,
		`CREATE TRIGGER trg_runtime_secret_rekey_runs_insert
		 BEFORE INSERT ON runtime_secret_rekey_runs
		 WHEN length(trim(NEW.primary_key_id)) NOT BETWEEN 1 AND 200
		   OR length(trim(NEW.operator_reference)) NOT BETWEEN 3 AND 200
		   OR instr(NEW.primary_key_id, char(10)) > 0 OR instr(NEW.primary_key_id, char(13)) > 0
		   OR instr(NEW.primary_key_id, char(9)) > 0 OR instr(NEW.operator_reference, char(10)) > 0
		   OR instr(NEW.operator_reference, char(13)) > 0 OR instr(NEW.operator_reference, char(9)) > 0
		 BEGIN SELECT RAISE(ABORT, 'invalid runtime Secret rekey run'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_entries_insert`,
		`CREATE TRIGGER trg_runtime_secret_rekey_entries_insert
		 BEFORE INSERT ON runtime_secret_rekey_entries
		 WHEN NEW.resource_type NOT IN ('execution_target_configuration', 'provider_resume_cursor')
		   OR length(trim(NEW.decrypt_key_id)) NOT BETWEEN 1 AND 200
		   OR length(trim(NEW.new_key_id)) NOT BETWEEN 1 AND 200
		   OR length(NEW.old_ciphertext_sha256) <> 32 OR length(NEW.new_ciphertext_sha256) <> 32
		   OR EXISTS (SELECT 1 FROM runtime_secret_rekey_receipts WHERE run_id IS NEW.run_id)
		   OR NOT EXISTS (
		     SELECT 1 FROM runtime_secret_rekey_runs
		     WHERE id IS NEW.run_id AND primary_key_id IS NEW.new_key_id
		   )
		 BEGIN SELECT RAISE(ABORT, 'invalid runtime Secret rekey entry'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_receipts_insert`,
		`CREATE TRIGGER trg_runtime_secret_rekey_receipts_insert
		 BEFORE INSERT ON runtime_secret_rekey_receipts
		 WHEN NEW.execution_target_count < 0 OR NEW.provider_resume_cursor_count < 0
		   OR length(NEW.entry_digest) <> 32
		   OR NOT EXISTS (SELECT 1 FROM runtime_secret_rekey_runs WHERE id IS NEW.run_id)
		   OR NEW.execution_target_count <> (
		     SELECT count(*) FROM runtime_secret_rekey_entries
		     WHERE run_id IS NEW.run_id AND resource_type = 'execution_target_configuration'
		   )
		   OR NEW.provider_resume_cursor_count <> (
		     SELECT count(*) FROM runtime_secret_rekey_entries
		     WHERE run_id IS NEW.run_id AND resource_type = 'provider_resume_cursor'
		   )
		   OR EXISTS (
		     SELECT 1 FROM runtime_secret_rekey_runs AS run, execution_targets AS target
		     WHERE run.id IS NEW.run_id AND length(target.configuration_encrypted) > 0
		       AND target.configuration_key_id IS NOT run.primary_key_id
		   )
		   OR EXISTS (
		     SELECT 1 FROM runtime_secret_rekey_runs AS run, agent_sessions AS session
		     WHERE run.id IS NEW.run_id AND session.provider_resume_cursor_state = 'usable'
		       AND length(session.provider_resume_cursor_encrypted) > 0
		       AND session.provider_resume_cursor_key_id IS NOT run.primary_key_id
		   )
		 BEGIN SELECT RAISE(ABORT, 'invalid runtime Secret rekey receipt'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_runs_immutable_update`,
		`CREATE TRIGGER trg_runtime_secret_rekey_runs_immutable_update BEFORE UPDATE ON runtime_secret_rekey_runs
		 BEGIN SELECT RAISE(ABORT, 'runtime Secret rekey evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_runs_immutable_delete`,
		`CREATE TRIGGER trg_runtime_secret_rekey_runs_immutable_delete BEFORE DELETE ON runtime_secret_rekey_runs
		 BEGIN SELECT RAISE(ABORT, 'runtime Secret rekey evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_entries_immutable_update`,
		`CREATE TRIGGER trg_runtime_secret_rekey_entries_immutable_update BEFORE UPDATE ON runtime_secret_rekey_entries
		 BEGIN SELECT RAISE(ABORT, 'runtime Secret rekey evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_entries_immutable_delete`,
		`CREATE TRIGGER trg_runtime_secret_rekey_entries_immutable_delete BEFORE DELETE ON runtime_secret_rekey_entries
		 BEGIN SELECT RAISE(ABORT, 'runtime Secret rekey evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_receipts_immutable_update`,
		`CREATE TRIGGER trg_runtime_secret_rekey_receipts_immutable_update BEFORE UPDATE ON runtime_secret_rekey_receipts
		 BEGIN SELECT RAISE(ABORT, 'runtime Secret rekey evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_runtime_secret_rekey_receipts_immutable_delete`,
		`CREATE TRIGGER trg_runtime_secret_rekey_receipts_immutable_delete BEFORE DELETE ON runtime_secret_rekey_receipts
		 BEGIN SELECT RAISE(ABORT, 'runtime Secret rekey evidence is immutable'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("migrate SQLite runtime Secret rekey safety: %w", err)
		}
	}
	return nil
}

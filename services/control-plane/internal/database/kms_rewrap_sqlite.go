package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateKMSRewrapSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_kms_rewrap_entries_run_resource
		 ON kms_rewrap_entries (run_id, resource_type, resource_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kms_rewrap_entries_resource
		 ON kms_rewrap_entries (resource_type, resource_id, created_at, id)`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_runs_insert`,
		`CREATE TRIGGER trg_kms_rewrap_runs_insert
		 BEFORE INSERT ON kms_rewrap_runs
		 WHEN NEW.primary_provider NOT IN ('local', 'aws-kms')
		   OR length(trim(NEW.primary_key_id)) NOT BETWEEN 1 AND 500
		   OR length(trim(NEW.operator_reference)) NOT BETWEEN 3 AND 200
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid KMS rewrap run');
		 END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_entries_insert`,
		`CREATE TRIGGER trg_kms_rewrap_entries_insert
		 BEFORE INSERT ON kms_rewrap_entries
		 WHEN NEW.resource_type NOT IN ('provider_credential', 'identity_connection', 'identity_login_attempt')
		   OR length(NEW.old_encrypted_data_key) NOT BETWEEN 1 AND 16384
		   OR length(NEW.new_encrypted_data_key) NOT BETWEEN 1 AND 16384
		   OR (NEW.old_kms_provider IS NEW.new_kms_provider AND NEW.old_kms_key_id IS NEW.new_kms_key_id)
		   OR EXISTS (SELECT 1 FROM kms_rewrap_receipts AS receipt WHERE receipt.run_id IS NEW.run_id)
		   OR NOT EXISTS (
		     SELECT 1 FROM kms_rewrap_runs AS run
		     WHERE run.id IS NEW.run_id
		       AND run.primary_provider IS NEW.new_kms_provider
		       AND run.primary_key_id IS NEW.new_kms_key_id
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid KMS rewrap entry');
		 END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_receipts_insert`,
		`CREATE TRIGGER trg_kms_rewrap_receipts_insert
		 BEFORE INSERT ON kms_rewrap_receipts
		 WHEN NEW.provider_credential_count < 0
		   OR NEW.identity_connection_count < 0
		   OR NEW.identity_login_attempt_count < 0
		   OR length(NEW.entry_digest) <> 32
		   OR NOT EXISTS (SELECT 1 FROM kms_rewrap_runs AS run WHERE run.id IS NEW.run_id)
		   OR NEW.provider_credential_count <> (
		     SELECT count(*) FROM kms_rewrap_entries
		     WHERE run_id IS NEW.run_id AND resource_type = 'provider_credential'
		   )
		   OR NEW.identity_connection_count <> (
		     SELECT count(*) FROM kms_rewrap_entries
		     WHERE run_id IS NEW.run_id AND resource_type = 'identity_connection'
		   )
		   OR NEW.identity_login_attempt_count <> (
		     SELECT count(*) FROM kms_rewrap_entries
		     WHERE run_id IS NEW.run_id AND resource_type = 'identity_login_attempt'
		   )
		   OR EXISTS (
		     SELECT 1
		     FROM kms_rewrap_runs AS run, provider_credentials AS credential
		     WHERE run.id IS NEW.run_id AND credential.encrypted_payload IS NOT NULL
		       AND (length(credential.encrypted_payload) = 0 OR credential.encrypted_data_key IS NULL
		         OR length(credential.encrypted_data_key) = 0 OR credential.kms_provider IS NOT run.primary_provider
		         OR credential.kms_key_id IS NOT run.primary_key_id)
		   )
		   OR EXISTS (
		     SELECT 1
		     FROM kms_rewrap_runs AS run, identity_connections AS connection
		     WHERE run.id IS NEW.run_id AND connection.encrypted_secret IS NOT NULL
		       AND (length(connection.encrypted_secret) = 0 OR connection.encrypted_data_key IS NULL
		         OR length(connection.encrypted_data_key) = 0 OR connection.kms_provider IS NOT run.primary_provider
		         OR connection.kms_key_id IS NOT run.primary_key_id)
		   )
		   OR EXISTS (
		     SELECT 1
		     FROM kms_rewrap_runs AS run, identity_login_attempts AS attempt
		     WHERE run.id IS NEW.run_id AND attempt.encrypted_payload IS NOT NULL
		       AND (length(attempt.encrypted_payload) = 0 OR attempt.encrypted_data_key IS NULL
		         OR length(attempt.encrypted_data_key) = 0 OR attempt.kms_provider IS NOT run.primary_provider
		         OR attempt.kms_key_id IS NOT run.primary_key_id)
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid KMS rewrap receipt');
		 END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_runs_immutable_update`,
		`CREATE TRIGGER trg_kms_rewrap_runs_immutable_update BEFORE UPDATE ON kms_rewrap_runs
		 BEGIN SELECT RAISE(ABORT, 'KMS rewrap evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_runs_immutable_delete`,
		`CREATE TRIGGER trg_kms_rewrap_runs_immutable_delete BEFORE DELETE ON kms_rewrap_runs
		 BEGIN SELECT RAISE(ABORT, 'KMS rewrap evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_entries_immutable_update`,
		`CREATE TRIGGER trg_kms_rewrap_entries_immutable_update BEFORE UPDATE ON kms_rewrap_entries
		 BEGIN SELECT RAISE(ABORT, 'KMS rewrap evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_entries_immutable_delete`,
		`CREATE TRIGGER trg_kms_rewrap_entries_immutable_delete BEFORE DELETE ON kms_rewrap_entries
		 BEGIN SELECT RAISE(ABORT, 'KMS rewrap evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_receipts_immutable_update`,
		`CREATE TRIGGER trg_kms_rewrap_receipts_immutable_update BEFORE UPDATE ON kms_rewrap_receipts
		 BEGIN SELECT RAISE(ABORT, 'KMS rewrap evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_kms_rewrap_receipts_immutable_delete`,
		`CREATE TRIGGER trg_kms_rewrap_receipts_immutable_delete BEFORE DELETE ON kms_rewrap_receipts
		 BEGIN SELECT RAISE(ABORT, 'KMS rewrap evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_identity_connections_kms_rewrap_guard`,
		`CREATE TRIGGER trg_identity_connections_kms_rewrap_guard
		 BEFORE UPDATE OF encrypted_secret, encrypted_data_key, kms_provider, kms_key_id
		 ON identity_connections
		 WHEN NEW.encrypted_secret IS OLD.encrypted_secret
		   AND (
		     NEW.encrypted_data_key IS NOT OLD.encrypted_data_key
		     OR NEW.kms_provider IS NOT OLD.kms_provider
		     OR NEW.kms_key_id IS NOT OLD.kms_key_id
		   )
		   AND NOT EXISTS (
		     SELECT 1
		     FROM kms_rewrap_entries AS entry
		     JOIN kms_rewrap_runs AS run ON run.id = entry.run_id
		     WHERE entry.tenant_id IS OLD.tenant_id
		       AND entry.resource_type = 'identity_connection'
		       AND entry.resource_id IS OLD.id
		       AND entry.old_kms_provider IS OLD.kms_provider
		       AND entry.old_kms_key_id IS OLD.kms_key_id
		       AND entry.old_encrypted_data_key IS OLD.encrypted_data_key
		       AND entry.new_kms_provider IS NEW.kms_provider
		       AND entry.new_kms_key_id IS NEW.kms_key_id
		       AND entry.new_encrypted_data_key IS NEW.encrypted_data_key
		       AND run.primary_provider IS NEW.kms_provider
		       AND run.primary_key_id IS NEW.kms_key_id
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'identity envelope-only change requires an authorized KMS rewrap');
		 END`,
		`DROP TRIGGER IF EXISTS trg_identity_login_attempts_kms_rewrap_guard`,
		`CREATE TRIGGER trg_identity_login_attempts_kms_rewrap_guard
		 BEFORE UPDATE OF encrypted_payload, encrypted_data_key, kms_provider, kms_key_id
		 ON identity_login_attempts
		 WHEN NEW.encrypted_payload IS OLD.encrypted_payload
		   AND (
		     NEW.encrypted_data_key IS NOT OLD.encrypted_data_key
		     OR NEW.kms_provider IS NOT OLD.kms_provider
		     OR NEW.kms_key_id IS NOT OLD.kms_key_id
		   )
		   AND NOT EXISTS (
		     SELECT 1
		     FROM kms_rewrap_entries AS entry
		     JOIN kms_rewrap_runs AS run ON run.id = entry.run_id
		     WHERE entry.tenant_id IS OLD.tenant_id
		       AND entry.resource_type = 'identity_login_attempt'
		       AND entry.resource_id IS OLD.id
		       AND entry.old_kms_provider IS OLD.kms_provider
		       AND entry.old_kms_key_id IS OLD.kms_key_id
		       AND entry.old_encrypted_data_key IS OLD.encrypted_data_key
		       AND entry.new_kms_provider IS NEW.kms_provider
		       AND entry.new_kms_key_id IS NEW.kms_key_id
		       AND entry.new_encrypted_data_key IS NEW.encrypted_data_key
		       AND run.primary_provider IS NEW.kms_provider
		       AND run.primary_key_id IS NEW.kms_key_id
		   )
		 BEGIN
		   SELECT RAISE(ABORT, 'identity envelope-only change requires an authorized KMS rewrap');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("migrate SQLite KMS rewrap safety: %w", err)
		}
	}
	return nil
}

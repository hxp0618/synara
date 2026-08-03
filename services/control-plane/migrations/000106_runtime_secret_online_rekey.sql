ALTER TABLE execution_targets
  ADD COLUMN configuration_key_id TEXT;

ALTER TABLE execution_targets
  ADD CONSTRAINT execution_targets_configuration_key_id_check CHECK (
    configuration_key_id IS NULL OR (
      length(trim(configuration_key_id)) BETWEEN 1 AND 200 AND
      configuration_key_id !~ E'[\r\n\t]' AND
      length(configuration_encrypted) > 0
    )
  );

ALTER TABLE agent_sessions
  ADD COLUMN provider_resume_cursor_key_id TEXT;

ALTER TABLE agent_sessions
  ADD CONSTRAINT agent_sessions_provider_cursor_key_id_check CHECK (
    provider_resume_cursor_key_id IS NULL OR (
      length(trim(provider_resume_cursor_key_id)) BETWEEN 1 AND 200 AND
      provider_resume_cursor_key_id !~ E'[\r\n\t]' AND
      length(provider_resume_cursor_encrypted) > 0
    )
  );

CREATE TABLE runtime_secret_rekey_runs (
  id UUID PRIMARY KEY,
  primary_key_id TEXT NOT NULL CHECK (
    length(trim(primary_key_id)) BETWEEN 1 AND 200 AND primary_key_id !~ E'[\r\n\t]'
  ),
  operator_reference TEXT NOT NULL CHECK (
    length(trim(operator_reference)) BETWEEN 3 AND 200 AND operator_reference !~ E'[\r\n\t]'
  ),
  started_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE runtime_secret_rekey_entries (
  id UUID PRIMARY KEY,
  run_id UUID NOT NULL REFERENCES runtime_secret_rekey_runs(id) ON DELETE RESTRICT,
  tenant_id UUID,
  resource_type TEXT NOT NULL CHECK (
    resource_type IN ('execution_target_configuration', 'provider_resume_cursor')
  ),
  resource_id UUID NOT NULL,
  old_stored_key_id TEXT,
  decrypt_key_id TEXT NOT NULL,
  new_key_id TEXT NOT NULL,
  old_ciphertext_sha256 BYTEA NOT NULL CHECK (length(old_ciphertext_sha256) = 32),
  new_ciphertext_sha256 BYTEA NOT NULL CHECK (length(new_ciphertext_sha256) = 32),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (run_id, resource_type, resource_id),
  CHECK (length(trim(decrypt_key_id)) BETWEEN 1 AND 200 AND decrypt_key_id !~ E'[\r\n\t]'),
  CHECK (length(trim(new_key_id)) BETWEEN 1 AND 200 AND new_key_id !~ E'[\r\n\t]')
);

CREATE INDEX idx_runtime_secret_rekey_entries_resource
  ON runtime_secret_rekey_entries (resource_type, resource_id, created_at, id);

CREATE TABLE runtime_secret_rekey_receipts (
  run_id UUID PRIMARY KEY REFERENCES runtime_secret_rekey_runs(id) ON DELETE RESTRICT,
  execution_target_count BIGINT NOT NULL CHECK (execution_target_count >= 0),
  provider_resume_cursor_count BIGINT NOT NULL CHECK (provider_resume_cursor_count >= 0),
  entry_digest BYTEA NOT NULL CHECK (length(entry_digest) = 32),
  completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION reject_runtime_secret_rekey_evidence_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'runtime Secret rekey evidence is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_runtime_secret_rekey_runs_immutable
BEFORE UPDATE OR DELETE ON runtime_secret_rekey_runs
FOR EACH ROW EXECUTE FUNCTION reject_runtime_secret_rekey_evidence_mutation();

CREATE TRIGGER trg_runtime_secret_rekey_entries_immutable
BEFORE UPDATE OR DELETE ON runtime_secret_rekey_entries
FOR EACH ROW EXECUTE FUNCTION reject_runtime_secret_rekey_evidence_mutation();

CREATE TRIGGER trg_runtime_secret_rekey_receipts_immutable
BEFORE UPDATE OR DELETE ON runtime_secret_rekey_receipts
FOR EACH ROW EXECUTE FUNCTION reject_runtime_secret_rekey_evidence_mutation();

CREATE OR REPLACE FUNCTION validate_runtime_secret_rekey_entry_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM runtime_secret_rekey_receipts WHERE run_id = NEW.run_id) THEN
    RAISE EXCEPTION 'completed runtime Secret rekey runs cannot accept new entries'
      USING ERRCODE = '23514';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM runtime_secret_rekey_runs
    WHERE id = NEW.run_id AND primary_key_id = NEW.new_key_id
  ) THEN
    RAISE EXCEPTION 'runtime Secret rekey entry does not match its run primary key'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_runtime_secret_rekey_entries_insert
BEFORE INSERT ON runtime_secret_rekey_entries
FOR EACH ROW EXECUTE FUNCTION validate_runtime_secret_rekey_entry_insert();

CREATE OR REPLACE FUNCTION runtime_secret_rekey_entry_authorizes(
  resource_kind TEXT,
  resource_tenant_id UUID,
  resource_uuid UUID,
  old_stored_key TEXT,
  new_stored_key TEXT,
  old_ciphertext BYTEA,
  new_ciphertext BYTEA
)
RETURNS BOOLEAN
LANGUAGE sql
STABLE
AS $$
  SELECT EXISTS (
    SELECT 1
    FROM runtime_secret_rekey_entries AS entry
    JOIN runtime_secret_rekey_runs AS run ON run.id = entry.run_id
    LEFT JOIN runtime_secret_rekey_receipts AS receipt ON receipt.run_id = run.id
    WHERE receipt.run_id IS NULL
      AND entry.resource_type = resource_kind
      AND entry.resource_id = resource_uuid
      AND entry.tenant_id IS NOT DISTINCT FROM resource_tenant_id
      AND entry.old_stored_key_id IS NOT DISTINCT FROM old_stored_key
      AND entry.new_key_id = new_stored_key
      AND run.primary_key_id = new_stored_key
      AND entry.old_ciphertext_sha256 = sha256(old_ciphertext)
      AND entry.new_ciphertext_sha256 = sha256(new_ciphertext)
  );
$$;

CREATE OR REPLACE FUNCTION guard_execution_target_runtime_secret_rekey()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.configuration_key_id IS DISTINCT FROM OLD.configuration_key_id AND
     NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at AND
     NOT runtime_secret_rekey_entry_authorizes(
       'execution_target_configuration', OLD.tenant_id, OLD.id,
       OLD.configuration_key_id, NEW.configuration_key_id,
       OLD.configuration_encrypted, NEW.configuration_encrypted
     ) THEN
    RAISE EXCEPTION 'Execution Target runtime Secret key change requires exact rekey evidence'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_targets_runtime_key_guard
BEFORE UPDATE OF configuration_encrypted, configuration_key_id ON execution_targets
FOR EACH ROW EXECUTE FUNCTION guard_execution_target_runtime_secret_rekey();

CREATE OR REPLACE FUNCTION guard_provider_cursor_runtime_secret_rekey()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.provider_resume_cursor_key_id IS DISTINCT FROM OLD.provider_resume_cursor_key_id AND
     NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at AND
     NOT runtime_secret_rekey_entry_authorizes(
       'provider_resume_cursor', OLD.tenant_id, OLD.id,
       OLD.provider_resume_cursor_key_id, NEW.provider_resume_cursor_key_id,
       OLD.provider_resume_cursor_encrypted, NEW.provider_resume_cursor_encrypted
     ) THEN
    RAISE EXCEPTION 'Provider Cursor runtime Secret key change requires exact rekey evidence'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_sessions_runtime_key_guard
BEFORE UPDATE OF provider_resume_cursor_encrypted, provider_resume_cursor_key_id ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION guard_provider_cursor_runtime_secret_rekey();

CREATE OR REPLACE FUNCTION validate_runtime_secret_rekey_receipt_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  run runtime_secret_rekey_runs%ROWTYPE;
BEGIN
  SELECT * INTO STRICT run FROM runtime_secret_rekey_runs WHERE id = NEW.run_id;
  IF NEW.execution_target_count <> (
       SELECT count(*) FROM runtime_secret_rekey_entries
       WHERE run_id = NEW.run_id AND resource_type = 'execution_target_configuration'
     ) OR NEW.provider_resume_cursor_count <> (
       SELECT count(*) FROM runtime_secret_rekey_entries
       WHERE run_id = NEW.run_id AND resource_type = 'provider_resume_cursor'
     ) THEN
    RAISE EXCEPTION 'runtime Secret rekey receipt counts do not match immutable entries'
      USING ERRCODE = '23514';
  END IF;
  IF EXISTS (
       SELECT 1 FROM execution_targets
       WHERE length(configuration_encrypted) > 0 AND configuration_key_id IS DISTINCT FROM run.primary_key_id
     ) OR EXISTS (
       SELECT 1 FROM agent_sessions
       WHERE provider_resume_cursor_state = 'usable'
         AND length(provider_resume_cursor_encrypted) > 0
         AND provider_resume_cursor_key_id IS DISTINCT FROM run.primary_key_id
     ) THEN
    RAISE EXCEPTION 'runtime Secret rekey receipt requires zero non-primary usable envelopes'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
EXCEPTION
  WHEN NO_DATA_FOUND THEN
    RAISE EXCEPTION 'runtime Secret rekey receipt references an unknown run'
      USING ERRCODE = '23503';
END;
$$;

CREATE TRIGGER trg_runtime_secret_rekey_receipts_insert
BEFORE INSERT ON runtime_secret_rekey_receipts
FOR EACH ROW EXECUTE FUNCTION validate_runtime_secret_rekey_receipt_insert();

COMMENT ON TABLE runtime_secret_rekey_runs IS
  'Immutable intent for online Provider Cursor and Execution Target configuration key rotation.';
COMMENT ON TABLE runtime_secret_rekey_entries IS
  'Ciphertext-digest evidence for one compare-and-swap runtime Secret re-encryption.';
COMMENT ON TABLE runtime_secret_rekey_receipts IS
  'Completion evidence issued only after every usable runtime Secret envelope names the primary key.';

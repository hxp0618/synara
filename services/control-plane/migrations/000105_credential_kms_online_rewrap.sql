CREATE TABLE kms_rewrap_runs (
  id UUID PRIMARY KEY,
  primary_provider TEXT NOT NULL CHECK (primary_provider IN ('local', 'aws-kms')),
  primary_key_id TEXT NOT NULL CHECK (length(trim(primary_key_id)) BETWEEN 1 AND 500),
  operator_reference TEXT NOT NULL CHECK (length(trim(operator_reference)) BETWEEN 3 AND 200),
  started_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE kms_rewrap_entries (
  id UUID PRIMARY KEY,
  run_id UUID NOT NULL REFERENCES kms_rewrap_runs(id) ON DELETE RESTRICT,
  tenant_id UUID NOT NULL,
  resource_type TEXT NOT NULL CHECK (
    resource_type IN ('provider_credential', 'identity_connection', 'identity_login_attempt')
  ),
  resource_id UUID NOT NULL,
  old_kms_provider TEXT NOT NULL,
  old_kms_key_id TEXT NOT NULL,
  old_encrypted_data_key BYTEA NOT NULL,
  new_kms_provider TEXT NOT NULL,
  new_kms_key_id TEXT NOT NULL,
  new_encrypted_data_key BYTEA NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (run_id, resource_type, resource_id),
  CHECK (
    length(old_encrypted_data_key) BETWEEN 1 AND 16384 AND
    length(new_encrypted_data_key) BETWEEN 1 AND 16384
  ),
  CHECK (
    old_kms_provider <> new_kms_provider OR
    old_kms_key_id <> new_kms_key_id
  )
);

CREATE INDEX idx_kms_rewrap_entries_resource
  ON kms_rewrap_entries (resource_type, resource_id, created_at, id);

CREATE TABLE kms_rewrap_receipts (
  run_id UUID PRIMARY KEY REFERENCES kms_rewrap_runs(id) ON DELETE RESTRICT,
  provider_credential_count BIGINT NOT NULL CHECK (provider_credential_count >= 0),
  identity_connection_count BIGINT NOT NULL CHECK (identity_connection_count >= 0),
  identity_login_attempt_count BIGINT NOT NULL CHECK (identity_login_attempt_count >= 0),
  entry_digest BYTEA NOT NULL CHECK (length(entry_digest) = 32),
  completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION reject_kms_rewrap_evidence_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'KMS rewrap evidence is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_kms_rewrap_runs_immutable
BEFORE UPDATE OR DELETE ON kms_rewrap_runs
FOR EACH ROW EXECUTE FUNCTION reject_kms_rewrap_evidence_mutation();

CREATE TRIGGER trg_kms_rewrap_entries_immutable
BEFORE UPDATE OR DELETE ON kms_rewrap_entries
FOR EACH ROW EXECUTE FUNCTION reject_kms_rewrap_evidence_mutation();

CREATE TRIGGER trg_kms_rewrap_receipts_immutable
BEFORE UPDATE OR DELETE ON kms_rewrap_receipts
FOR EACH ROW EXECUTE FUNCTION reject_kms_rewrap_evidence_mutation();

CREATE OR REPLACE FUNCTION validate_kms_rewrap_entry_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM kms_rewrap_receipts WHERE run_id = NEW.run_id) THEN
    RAISE EXCEPTION 'completed KMS rewrap runs cannot accept new entries'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_kms_rewrap_entries_insert
BEFORE INSERT ON kms_rewrap_entries
FOR EACH ROW EXECUTE FUNCTION validate_kms_rewrap_entry_insert();

CREATE OR REPLACE FUNCTION validate_kms_rewrap_receipt_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  run kms_rewrap_runs%ROWTYPE;
BEGIN
  SELECT * INTO STRICT run FROM kms_rewrap_runs WHERE id = NEW.run_id;

  IF NEW.provider_credential_count <> (
       SELECT count(*) FROM kms_rewrap_entries
       WHERE run_id = NEW.run_id AND resource_type = 'provider_credential'
     ) OR
     NEW.identity_connection_count <> (
       SELECT count(*) FROM kms_rewrap_entries
       WHERE run_id = NEW.run_id AND resource_type = 'identity_connection'
     ) OR
     NEW.identity_login_attempt_count <> (
       SELECT count(*) FROM kms_rewrap_entries
       WHERE run_id = NEW.run_id AND resource_type = 'identity_login_attempt'
     ) THEN
    RAISE EXCEPTION 'KMS rewrap receipt counts do not match immutable entries'
      USING ERRCODE = '23514';
  END IF;

  IF EXISTS (
       SELECT 1 FROM provider_credentials
       WHERE encrypted_payload IS NOT NULL AND (
         length(encrypted_payload) = 0 OR encrypted_data_key IS NULL OR length(encrypted_data_key) = 0 OR
         kms_provider IS DISTINCT FROM run.primary_provider OR
         kms_key_id IS DISTINCT FROM run.primary_key_id
       )
     ) OR EXISTS (
       SELECT 1 FROM identity_connections
       WHERE encrypted_secret IS NOT NULL AND (
         length(encrypted_secret) = 0 OR encrypted_data_key IS NULL OR length(encrypted_data_key) = 0 OR
         kms_provider IS DISTINCT FROM run.primary_provider OR
         kms_key_id IS DISTINCT FROM run.primary_key_id
       )
     ) OR EXISTS (
       SELECT 1 FROM identity_login_attempts
       WHERE encrypted_payload IS NOT NULL AND (
         length(encrypted_payload) = 0 OR encrypted_data_key IS NULL OR length(encrypted_data_key) = 0 OR
         kms_provider IS DISTINCT FROM run.primary_provider OR
         kms_key_id IS DISTINCT FROM run.primary_key_id
       )
     ) THEN
    RAISE EXCEPTION 'KMS rewrap receipt requires zero non-primary envelopes'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
EXCEPTION
  WHEN NO_DATA_FOUND THEN
    RAISE EXCEPTION 'KMS rewrap receipt references an unknown run'
      USING ERRCODE = '23503';
END;
$$;

CREATE TRIGGER trg_kms_rewrap_receipts_insert
BEFORE INSERT ON kms_rewrap_receipts
FOR EACH ROW EXECUTE FUNCTION validate_kms_rewrap_receipt_insert();

CREATE OR REPLACE FUNCTION kms_rewrap_entry_authorizes(
  resource_kind TEXT,
  resource_tenant_id UUID,
  resource_uuid UUID,
  old_provider TEXT,
  old_key_id TEXT,
  old_wrapped_key BYTEA,
  new_provider TEXT,
  new_key_id TEXT,
  new_wrapped_key BYTEA
)
RETURNS BOOLEAN
LANGUAGE sql
STABLE
AS $$
  SELECT EXISTS (
    SELECT 1
    FROM kms_rewrap_entries AS entry
    JOIN kms_rewrap_runs AS run ON run.id = entry.run_id
    WHERE entry.tenant_id = resource_tenant_id
      AND entry.resource_type = resource_kind
      AND entry.resource_id = resource_uuid
      AND entry.old_kms_provider = old_provider
      AND entry.old_kms_key_id = old_key_id
      AND entry.old_encrypted_data_key = old_wrapped_key
      AND entry.new_kms_provider = new_provider
      AND entry.new_kms_key_id = new_key_id
      AND entry.new_encrypted_data_key = new_wrapped_key
      AND run.primary_provider = new_provider
      AND run.primary_key_id = new_key_id
  );
$$;

CREATE OR REPLACE FUNCTION reject_credential_identity_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.tenant_id <> OLD.tenant_id OR
     NEW.purpose <> OLD.purpose OR
     NEW.provider <> OLD.provider OR
     NEW.credential_type <> OLD.credential_type OR
     NEW.scope <> OLD.scope OR
     NEW.scope_user_id IS DISTINCT FROM OLD.scope_user_id OR
     NEW.organization_id IS DISTINCT FROM OLD.organization_id OR
     NEW.selector_organization_id IS DISTINCT FROM OLD.selector_organization_id OR
     NEW.selector_model IS DISTINCT FROM OLD.selector_model THEN
    RAISE EXCEPTION 'credential tenant, purpose, provider, type, and scope identity are immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.aad_version <> OLD.aad_version AND NOT (
    OLD.aad_version IN (1, 2) AND NEW.aad_version = 3
  ) THEN
    RAISE EXCEPTION 'credential AAD version may only upgrade from legacy v1/v2 to v3 during rotation'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version AND NOT (
    NEW.version = OLD.version + 1 AND
    NEW.encrypted_payload IS DISTINCT FROM OLD.encrypted_payload AND
    NEW.encrypted_data_key IS DISTINCT FROM OLD.encrypted_data_key
  ) THEN
    RAISE EXCEPTION 'credential rotation must advance one version with new ciphertext and wrapped data key'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version = OLD.version AND (
    NEW.aad_version <> OLD.aad_version OR
    NEW.encrypted_payload IS DISTINCT FROM OLD.encrypted_payload OR
    NEW.encrypted_data_key IS DISTINCT FROM OLD.encrypted_data_key OR
    NEW.kms_provider <> OLD.kms_provider OR
    NEW.kms_key_id <> OLD.kms_key_id
  ) AND NOT (
    NEW.aad_version = OLD.aad_version AND
    NEW.encrypted_payload IS NOT DISTINCT FROM OLD.encrypted_payload AND
    kms_rewrap_entry_authorizes(
      'provider_credential', OLD.tenant_id, OLD.id,
      OLD.kms_provider, OLD.kms_key_id, OLD.encrypted_data_key,
      NEW.kms_provider, NEW.kms_key_id, NEW.encrypted_data_key
    )
  ) THEN
    RAISE EXCEPTION 'credential envelope cannot change without rotation or authorized KMS rewrap'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION reject_identity_envelope_only_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  resource_kind TEXT;
  old_payload BYTEA;
  new_payload BYTEA;
BEGIN
  IF TG_TABLE_NAME = 'identity_connections' THEN
    resource_kind := 'identity_connection';
    old_payload := OLD.encrypted_secret;
    new_payload := NEW.encrypted_secret;
  ELSE
    resource_kind := 'identity_login_attempt';
    old_payload := OLD.encrypted_payload;
    new_payload := NEW.encrypted_payload;
  END IF;

  IF new_payload IS NOT DISTINCT FROM old_payload AND (
    NEW.encrypted_data_key IS DISTINCT FROM OLD.encrypted_data_key OR
    NEW.kms_provider IS DISTINCT FROM OLD.kms_provider OR
    NEW.kms_key_id IS DISTINCT FROM OLD.kms_key_id
  ) AND NOT kms_rewrap_entry_authorizes(
    resource_kind, OLD.tenant_id, OLD.id,
    OLD.kms_provider, OLD.kms_key_id, OLD.encrypted_data_key,
    NEW.kms_provider, NEW.kms_key_id, NEW.encrypted_data_key
  ) THEN
    RAISE EXCEPTION 'identity envelope-only change requires an authorized KMS rewrap'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_identity_connections_kms_rewrap_guard
BEFORE UPDATE OF encrypted_secret, encrypted_data_key, kms_provider, kms_key_id
ON identity_connections
FOR EACH ROW EXECUTE FUNCTION reject_identity_envelope_only_change();

CREATE TRIGGER trg_identity_login_attempts_kms_rewrap_guard
BEFORE UPDATE OF encrypted_payload, encrypted_data_key, kms_provider, kms_key_id
ON identity_login_attempts
FOR EACH ROW EXECUTE FUNCTION reject_identity_envelope_only_change();

COMMENT ON TABLE kms_rewrap_runs IS
  'Immutable intent for an online envelope-KMS key rotation; a missing receipt means interrupted work may resume.';
COMMENT ON TABLE kms_rewrap_entries IS
  'Immutable per-resource authorization and audit evidence for changing only the wrapped data key.';
COMMENT ON TABLE kms_rewrap_receipts IS
  'Immutable completion evidence issued only after no configured encrypted resource remains on a non-primary key.';

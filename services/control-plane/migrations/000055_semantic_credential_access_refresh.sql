ALTER TABLE agent_sessions
  ADD COLUMN meaningful_activity_sequence BIGINT;

UPDATE agent_sessions
SET meaningful_activity_sequence = last_event_sequence
WHERE meaningful_activity_sequence IS NULL;

ALTER TABLE agent_sessions
  ALTER COLUMN meaningful_activity_sequence SET DEFAULT 0,
  ALTER COLUMN meaningful_activity_sequence SET NOT NULL,
  ADD CONSTRAINT chk_agent_sessions_meaningful_activity_sequence
    CHECK (meaningful_activity_sequence >= 0 AND meaningful_activity_sequence <= last_event_sequence);

COMMENT ON COLUMN agent_sessions.meaningful_activity_sequence IS
  'Broker watermark for semantic renewal: the last authoritative Session history sequence that advanced meaningful activity.';

ALTER TABLE worker_leases
  ADD COLUMN provider_credential_grant_id UUID,
  ADD COLUMN provider_credential_access_serial BIGINT,
  ADD COLUMN provider_credential_activity_sequence BIGINT,
  ADD COLUMN provider_credential_activity_at TIMESTAMPTZ,
  ADD COLUMN provider_credential_access_issued_at TIMESTAMPTZ,
  ADD COLUMN provider_credential_access_renewed_at TIMESTAMPTZ,
  ADD COLUMN provider_credential_access_expires_at TIMESTAMPTZ,
  ADD COLUMN provider_credential_refresh_deadline_at TIMESTAMPTZ,
  ADD COLUMN provider_credential_hard_expires_at TIMESTAMPTZ,
  ADD CONSTRAINT fk_worker_leases_provider_credential_grant
    FOREIGN KEY (tenant_id, provider_credential_grant_id)
    REFERENCES execution_provider_credential_grants(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_worker_leases_provider_credential_access_shape
    CHECK (
      (
        provider_credential_grant_id IS NULL
        AND provider_credential_access_serial IS NULL
        AND provider_credential_activity_sequence IS NULL
        AND provider_credential_activity_at IS NULL
        AND provider_credential_access_issued_at IS NULL
        AND provider_credential_access_renewed_at IS NULL
        AND provider_credential_access_expires_at IS NULL
        AND provider_credential_refresh_deadline_at IS NULL
        AND provider_credential_hard_expires_at IS NULL
      )
      OR (
        provider_credential_grant_id IS NOT NULL
        AND provider_credential_access_serial IS NOT NULL
        AND provider_credential_activity_sequence IS NOT NULL
        AND provider_credential_activity_at IS NOT NULL
        AND provider_credential_access_issued_at IS NOT NULL
        AND provider_credential_access_renewed_at IS NOT NULL
        AND provider_credential_access_expires_at IS NOT NULL
        AND provider_credential_refresh_deadline_at IS NOT NULL
        AND provider_credential_access_serial > 0
        AND provider_credential_activity_sequence >= 0
        AND provider_credential_access_renewed_at >= provider_credential_access_issued_at
        AND provider_credential_access_expires_at > provider_credential_access_renewed_at
        AND provider_credential_refresh_deadline_at >= provider_credential_activity_at
        AND (
          provider_credential_hard_expires_at IS NULL
          OR provider_credential_access_expires_at <= provider_credential_hard_expires_at
        )
      )
    );

CREATE INDEX IF NOT EXISTS idx_worker_leases_provider_credential_access_expiry
  ON worker_leases (provider_credential_access_expires_at, tenant_id, execution_id)
  WHERE provider_credential_grant_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_worker_leases_provider_credential_refresh_deadline
  ON worker_leases (provider_credential_refresh_deadline_at, tenant_id, execution_id)
  WHERE provider_credential_grant_id IS NOT NULL;

CREATE OR REPLACE FUNCTION enforce_worker_lease_provider_credential_access()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  grant_row execution_provider_credential_grants%ROWTYPE;
  access_payload_changed BOOLEAN;
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.provider_credential_grant_id IS NOT NULL THEN
    IF NEW.provider_credential_grant_id IS DISTINCT FROM OLD.provider_credential_grant_id THEN
      RAISE EXCEPTION 'Worker lease Provider Credential Grant is immutable'
        USING ERRCODE = '23514';
    END IF;
    IF NEW.provider_credential_access_issued_at IS DISTINCT FROM OLD.provider_credential_access_issued_at THEN
      RAISE EXCEPTION 'Worker lease Provider Credential access issued-at is immutable'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.provider_credential_grant_id IS NULL THEN
    RETURN NEW;
  END IF;

  SELECT * INTO grant_row
  FROM execution_provider_credential_grants
  WHERE tenant_id = NEW.tenant_id
    AND id = NEW.provider_credential_grant_id
  FOR SHARE;
  IF NOT FOUND
     OR grant_row.execution_id <> NEW.execution_id
     OR grant_row.generation <> NEW.generation THEN
    RAISE EXCEPTION 'Worker lease Provider Credential access must match its frozen Execution Provider Credential Grant'
      USING ERRCODE = '23514';
  END IF;

  IF TG_OP = 'UPDATE' AND OLD.provider_credential_grant_id IS NOT NULL THEN
    access_payload_changed :=
      NEW.provider_credential_activity_sequence IS DISTINCT FROM OLD.provider_credential_activity_sequence
      OR NEW.provider_credential_activity_at IS DISTINCT FROM OLD.provider_credential_activity_at
      OR NEW.provider_credential_access_renewed_at IS DISTINCT FROM OLD.provider_credential_access_renewed_at
      OR NEW.provider_credential_access_expires_at IS DISTINCT FROM OLD.provider_credential_access_expires_at
      OR NEW.provider_credential_refresh_deadline_at IS DISTINCT FROM OLD.provider_credential_refresh_deadline_at
      OR NEW.provider_credential_hard_expires_at IS DISTINCT FROM OLD.provider_credential_hard_expires_at;

    IF access_payload_changed THEN
      IF NEW.provider_credential_access_serial <> OLD.provider_credential_access_serial + 1 THEN
        RAISE EXCEPTION 'Worker lease Provider Credential access serial must advance exactly once per state update'
          USING ERRCODE = '23514';
      END IF;
    ELSIF NEW.provider_credential_access_serial IS DISTINCT FROM OLD.provider_credential_access_serial THEN
      RAISE EXCEPTION 'Worker lease Provider Credential access serial must advance exactly once per state update'
        USING ERRCODE = '23514';
    END IF;

    IF NEW.provider_credential_activity_sequence < OLD.provider_credential_activity_sequence THEN
      RAISE EXCEPTION 'Worker lease Provider Credential activity sequence cannot go backwards'
        USING ERRCODE = '23514';
    END IF;
    IF NEW.provider_credential_activity_sequence = OLD.provider_credential_activity_sequence
       AND NEW.provider_credential_activity_at IS DISTINCT FROM OLD.provider_credential_activity_at THEN
      RAISE EXCEPTION 'Worker lease Provider Credential activity_at must stay frozen without a newer activity sequence'
        USING ERRCODE = '23514';
    END IF;
    IF NEW.provider_credential_activity_sequence > OLD.provider_credential_activity_sequence
       AND NEW.provider_credential_activity_at < OLD.provider_credential_activity_at THEN
      RAISE EXCEPTION 'Worker lease Provider Credential activity_at cannot go backwards'
        USING ERRCODE = '23514';
    END IF;
    IF OLD.provider_credential_hard_expires_at IS NOT NULL
       AND (
         NEW.provider_credential_hard_expires_at IS NULL
         OR NEW.provider_credential_hard_expires_at > OLD.provider_credential_hard_expires_at
       ) THEN
      RAISE EXCEPTION 'Worker lease Provider Credential hard expiry cannot be extended'
        USING ERRCODE = '23514';
    END IF;
    IF NEW.provider_credential_access_renewed_at < OLD.provider_credential_access_renewed_at
       OR NEW.provider_credential_access_expires_at < OLD.provider_credential_access_expires_at THEN
      RAISE EXCEPTION 'Worker lease Provider Credential access renewal windows cannot move backwards'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_leases_provider_credential_access ON worker_leases;
CREATE TRIGGER trg_worker_leases_provider_credential_access
BEFORE INSERT OR UPDATE OF tenant_id, execution_id, generation,
  provider_credential_grant_id, provider_credential_access_serial,
  provider_credential_activity_sequence, provider_credential_activity_at,
  provider_credential_access_issued_at, provider_credential_access_renewed_at,
  provider_credential_access_expires_at, provider_credential_refresh_deadline_at,
  provider_credential_hard_expires_at
ON worker_leases
FOR EACH ROW EXECUTE FUNCTION enforce_worker_lease_provider_credential_access();

COMMENT ON COLUMN worker_leases.provider_credential_grant_id IS
  'Immutable Execution Provider Credential Grant lineage selected for this Worker Lease once semantic access renewal begins.';
COMMENT ON COLUMN worker_leases.provider_credential_access_serial IS
  'Monotonic semantic access state serial; each renewal or activity watermark change increments it by exactly one.';
COMMENT ON COLUMN worker_leases.provider_credential_activity_sequence IS
  'Last meaningful Session history sequence observed by the broker when issuing or renewing Provider Credential access for this Lease.';
COMMENT ON COLUMN worker_leases.provider_credential_activity_at IS
  'Server-authoritative timestamp paired with provider_credential_activity_sequence.';

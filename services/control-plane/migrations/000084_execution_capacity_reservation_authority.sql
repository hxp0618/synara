ALTER TABLE execution_target_health
  ADD COLUMN reservation_authority_mode TEXT,
  ADD COLUMN reservation_acknowledged_units INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN reservation_acknowledgements_sha256 TEXT,
  ADD CONSTRAINT chk_execution_target_health_reservation_authority
    CHECK (
      (reservation_authority_mode IS NULL
        AND reservation_acknowledged_units = 0
        AND reservation_acknowledgements_sha256 IS NULL)
      OR
      (reservation_authority_mode = 'exact-active-v1'
        AND reservation_acknowledged_units >= 0
        AND reservation_acknowledged_units <= allocated_capacity_units
        AND reservation_acknowledgements_sha256 ~ '^[0-9a-f]{64}$')
    );

CREATE TABLE execution_target_reservation_acknowledgements (
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  execution_id UUID NOT NULL REFERENCES agent_executions(id) ON DELETE CASCADE,
  execution_generation BIGINT NOT NULL CHECK (execution_generation >= 0),
  health_version BIGINT NOT NULL CHECK (health_version > 0),
  acknowledged_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (execution_target_id, execution_id, execution_generation)
);

CREATE INDEX idx_target_reservation_acknowledgements_health
  ON execution_target_reservation_acknowledgements (execution_target_id, health_version, execution_id);

CREATE TABLE execution_capacity_admissions (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  admission_mode TEXT NOT NULL
    CHECK (admission_mode IN ('fixed-unbounded-v1', 'publisher-health-v1', 'exact-active-v1')),
  health_version BIGINT CHECK (health_version IS NULL OR health_version > 0),
  health_source TEXT,
  health_observed_at TIMESTAMPTZ,
  health_expires_at TIMESTAMPTZ,
  capacity_status TEXT CHECK (capacity_status IS NULL OR capacity_status IN ('available', 'saturated', 'unknown')),
  capacity_ceiling_units INTEGER CHECK (capacity_ceiling_units IS NULL OR capacity_ceiling_units >= 0),
  allocated_capacity_units INTEGER CHECK (allocated_capacity_units IS NULL OR allocated_capacity_units >= 0),
  reservation_authority_mode TEXT CHECK (reservation_authority_mode IS NULL OR reservation_authority_mode = 'exact-active-v1'),
  reservation_acknowledged_units INTEGER CHECK (reservation_acknowledged_units IS NULL OR reservation_acknowledged_units >= 0),
  reservation_acknowledgements_sha256 TEXT
    CHECK (reservation_acknowledgements_sha256 IS NULL OR reservation_acknowledgements_sha256 ~ '^[0-9a-f]{64}$'),
  active_reservation_units BIGINT NOT NULL CHECK (active_reservation_units >= 0),
  unacknowledged_reservation_units BIGINT CHECK (unacknowledged_reservation_units IS NULL OR unacknowledged_reservation_units >= 0),
  strict_capacity_used_units BIGINT CHECK (strict_capacity_used_units IS NULL OR strict_capacity_used_units >= 0),
  admitted_at TIMESTAMPTZ NOT NULL,
  snapshot_sha256 TEXT NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
  PRIMARY KEY (tenant_id, execution_id),
  UNIQUE (execution_id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE
    DEFERRABLE INITIALLY DEFERRED,
  CHECK (
    (admission_mode = 'fixed-unbounded-v1'
      AND health_version IS NULL AND health_source IS NULL
      AND health_observed_at IS NULL AND health_expires_at IS NULL
      AND capacity_status IS NULL AND capacity_ceiling_units IS NULL
      AND allocated_capacity_units IS NULL
      AND reservation_authority_mode IS NULL
      AND reservation_acknowledged_units IS NULL
      AND reservation_acknowledgements_sha256 IS NULL
      AND unacknowledged_reservation_units IS NULL
      AND strict_capacity_used_units IS NULL)
    OR
    (admission_mode = 'publisher-health-v1'
      AND health_version IS NOT NULL AND health_source IS NOT NULL
      AND health_observed_at IS NOT NULL AND health_expires_at > health_observed_at
      AND capacity_status IS NOT NULL AND allocated_capacity_units IS NOT NULL
      AND reservation_authority_mode IS NULL
      AND reservation_acknowledged_units IS NULL
      AND reservation_acknowledgements_sha256 IS NULL
      AND unacknowledged_reservation_units IS NULL
      AND strict_capacity_used_units IS NULL)
    OR
    (admission_mode = 'exact-active-v1'
      AND health_version IS NOT NULL AND health_source IS NOT NULL
      AND health_observed_at IS NOT NULL AND health_expires_at > health_observed_at
      AND capacity_status IS NOT NULL AND allocated_capacity_units IS NOT NULL
      AND reservation_authority_mode = 'exact-active-v1'
      AND reservation_acknowledged_units IS NOT NULL
      AND reservation_acknowledgements_sha256 IS NOT NULL
      AND unacknowledged_reservation_units IS NOT NULL
      AND strict_capacity_used_units = allocated_capacity_units + unacknowledged_reservation_units)
  )
);

CREATE INDEX idx_execution_capacity_admissions_target_time
  ON execution_capacity_admissions (execution_target_id, admitted_at DESC, execution_id);

CREATE OR REPLACE FUNCTION capacity_reservation_acknowledgements_sha256(
  target_id UUID,
  target_health_version BIGINT
)
RETURNS TEXT
LANGUAGE sql
STABLE
AS $$
  SELECT encode(digest(
    'synara/capacity-reservation-acknowledgements/exact-active-v1' || E'\n' ||
    scheduling_evidence_field('execution_target_id', target_id::text) ||
    scheduling_evidence_field('health_version', target_health_version::text) ||
    COALESCE((
      SELECT string_agg(
        scheduling_evidence_field('execution_id', acknowledgement.execution_id::text) ||
        scheduling_evidence_field('execution_generation', acknowledgement.execution_generation::text),
        '' ORDER BY acknowledgement.execution_id, acknowledgement.execution_generation
      )
      FROM execution_target_reservation_acknowledgements AS acknowledgement
      WHERE acknowledgement.execution_target_id = target_id
        AND acknowledgement.health_version = target_health_version
    ), ''),
    'sha256'
  ), 'hex')
$$;

CREATE OR REPLACE FUNCTION enforce_execution_target_reservation_acknowledgement_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  current_health_version BIGINT;
BEGIN
  SELECT health.version INTO current_health_version
  FROM execution_target_health AS health
  WHERE health.execution_target_id = NEW.execution_target_id;

  IF NEW.health_version <> COALESCE(current_health_version + 1, 1) THEN
    RAISE EXCEPTION 'Reservation acknowledgement is not staged for the next Health version'
      USING ERRCODE = '23514';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM agent_executions AS execution
    WHERE execution.id = NEW.execution_id
      AND execution.execution_target_id = NEW.execution_target_id
  ) THEN
    RAISE EXCEPTION 'Reservation acknowledgement belongs to another Execution Target'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_target_reservation_acknowledgements_insert
BEFORE INSERT ON execution_target_reservation_acknowledgements
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_reservation_acknowledgement_insert();

CREATE OR REPLACE FUNCTION enforce_execution_target_reservation_acknowledgement_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Reservation acknowledgements cannot be updated in place'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_execution_target_reservation_acknowledgements_update
BEFORE UPDATE ON execution_target_reservation_acknowledgements
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_reservation_acknowledgement_update();

CREATE OR REPLACE FUNCTION validate_execution_target_health_reservation_authority()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  staged_units BIGINT;
  staged_sha256 TEXT;
BEGIN
  SELECT count(*) INTO staged_units
  FROM execution_target_reservation_acknowledgements AS acknowledgement
  WHERE acknowledgement.execution_target_id = NEW.execution_target_id
    AND acknowledgement.health_version = NEW.version;

  IF NEW.reservation_authority_mode IS NULL THEN
    IF staged_units <> 0 OR NEW.reservation_acknowledged_units <> 0
      OR NEW.reservation_acknowledgements_sha256 IS NOT NULL THEN
      RAISE EXCEPTION 'Health reservation authority shape is invalid'
        USING ERRCODE = '23514';
    END IF;
  ELSE
    staged_sha256 := capacity_reservation_acknowledgements_sha256(
      NEW.execution_target_id,
      NEW.version
    );
    IF NEW.reservation_authority_mode <> 'exact-active-v1'
      OR NEW.reservation_acknowledged_units <> staged_units
      OR NEW.reservation_acknowledgements_sha256 IS DISTINCT FROM staged_sha256 THEN
      RAISE EXCEPTION 'Health reservation acknowledgements do not match the staged authority'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_target_health_reservation_authority
BEFORE INSERT OR UPDATE ON execution_target_health
FOR EACH ROW EXECUTE FUNCTION validate_execution_target_health_reservation_authority();

CREATE OR REPLACE FUNCTION enforce_execution_target_health_monotonic()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution Target health observations cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.execution_target_id <> OLD.execution_target_id
    OR NEW.version <> OLD.version + 1
    OR NEW.observed_at <= OLD.observed_at
    OR (OLD.reservation_authority_mode IS NOT NULL AND NEW.reservation_authority_mode IS NULL) THEN
    RAISE EXCEPTION 'Execution Target health update is stale or not monotonic' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION lock_execution_target_for_capacity_reservation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
	IF TG_OP = 'INSERT' AND NEW.status IN ('queued', 'recovering') THEN
	  PERFORM 1 FROM execution_targets AS target
	  WHERE target.id = NEW.execution_target_id
	  FOR UPDATE;
	ELSIF TG_OP = 'UPDATE' AND NEW.status IN ('queued', 'recovering')
	  AND OLD.status NOT IN ('queued', 'recovering') THEN
    PERFORM 1 FROM execution_targets AS target
    WHERE target.id = NEW.execution_target_id
    FOR UPDATE;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_executions_capacity_reservation_lock
BEFORE INSERT OR UPDATE OF status ON agent_executions
FOR EACH ROW EXECUTE FUNCTION lock_execution_target_for_capacity_reservation();

CREATE OR REPLACE FUNCTION enforce_execution_capacity_admission_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Execution capacity admission evidence is immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_execution_capacity_admissions_immutable
BEFORE UPDATE ON execution_capacity_admissions
FOR EACH ROW EXECUTE FUNCTION enforce_execution_capacity_admission_immutable();

COMMENT ON TABLE execution_target_reservation_acknowledgements IS
  'Exact current Health-version proof of queued/recovering Execution generations already represented in Target occupancy.';

COMMENT ON TABLE execution_capacity_admissions IS
  'Immutable per-Execution snapshot of the final Target capacity admission authority.';

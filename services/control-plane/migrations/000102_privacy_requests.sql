CREATE TABLE privacy_requests (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  subject_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  request_type TEXT NOT NULL CHECK (request_type IN ('access_export', 'erasure')),
  status TEXT NOT NULL DEFAULT 'requested'
    CHECK (status IN ('requested', 'verified', 'approved', 'processing', 'completed', 'denied', 'cancelled', 'failed')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  requested_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  intake_reason TEXT NOT NULL,
  due_at TIMESTAMPTZ NOT NULL,
  last_transition_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  last_transition_reason TEXT NOT NULL,
  completed_at TIMESTAMPTZ,
  result_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
  result_digest_sha256 TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  CHECK (length(btrim(intake_reason)) BETWEEN 10 AND 2000),
  CHECK (length(btrim(last_transition_reason)) BETWEEN 10 AND 2000),
  CHECK (due_at >= created_at),
  CHECK ((status = 'completed') = (completed_at IS NOT NULL)),
  CHECK (result_digest_sha256 IS NULL OR result_digest_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE UNIQUE INDEX uq_privacy_requests_active_subject_type
  ON privacy_requests (tenant_id, subject_user_id, request_type)
  WHERE status IN ('requested', 'verified', 'approved', 'processing', 'failed');

CREATE INDEX idx_privacy_requests_tenant_queue
  ON privacy_requests (tenant_id, status, due_at, created_at, id);

CREATE TABLE privacy_request_events (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  privacy_request_id UUID NOT NULL,
  version BIGINT NOT NULL CHECK (version > 0),
  from_status TEXT,
  to_status TEXT NOT NULL,
  actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL,
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, privacy_request_id)
    REFERENCES privacy_requests(tenant_id, id) ON DELETE RESTRICT,
  UNIQUE (tenant_id, privacy_request_id, version),
  CHECK (from_status IS NULL OR from_status IN ('requested', 'verified', 'approved', 'processing', 'completed', 'denied', 'cancelled', 'failed')),
  CHECK (to_status IN ('requested', 'verified', 'approved', 'processing', 'completed', 'denied', 'cancelled', 'failed')),
  CHECK (length(btrim(reason)) BETWEEN 10 AND 2000),
  CHECK ((version = 1 AND from_status IS NULL AND to_status = 'requested') OR
         (version > 1 AND from_status IS NOT NULL))
);

CREATE INDEX idx_privacy_request_events_history
  ON privacy_request_events (tenant_id, privacy_request_id, version);

CREATE OR REPLACE FUNCTION block_legal_hold_during_erasure()
RETURNS trigger AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM privacy_requests
    WHERE tenant_id = NEW.tenant_id
      AND request_type = 'erasure'
      AND status = 'processing'
  ) THEN
    RAISE EXCEPTION 'Legal Hold cannot be created while Tenant erasure is processing';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_legal_holds_privacy_gate
BEFORE INSERT ON legal_holds
FOR EACH ROW EXECUTE FUNCTION block_legal_hold_during_erasure();

CREATE OR REPLACE FUNCTION validate_privacy_request_insert()
RETURNS trigger AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM tenant_memberships
    WHERE tenant_id = NEW.tenant_id AND user_id = NEW.subject_user_id
  ) THEN
    RAISE EXCEPTION 'Privacy Request subject does not belong to Tenant';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM tenant_memberships
    WHERE tenant_id = NEW.tenant_id AND user_id = NEW.requested_by AND status = 'active'
  ) THEN
    RAISE EXCEPTION 'Privacy Request requester does not belong to Tenant';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_privacy_requests_insert
BEFORE INSERT ON privacy_requests
FOR EACH ROW EXECUTE FUNCTION validate_privacy_request_insert();

CREATE OR REPLACE FUNCTION validate_privacy_request_update()
RETURNS trigger AS $$
BEGIN
  IF NEW.id <> OLD.id
    OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.subject_user_id <> OLD.subject_user_id
    OR NEW.request_type <> OLD.request_type
    OR NEW.requested_by <> OLD.requested_by
    OR NEW.intake_reason <> OLD.intake_reason
    OR NEW.due_at <> OLD.due_at
    OR NEW.created_at <> OLD.created_at
    OR NEW.version <> OLD.version + 1
    OR NOT (
      (OLD.status = 'requested' AND NEW.status IN ('verified', 'denied', 'cancelled'))
      OR (OLD.status = 'verified' AND NEW.status IN ('approved', 'denied', 'cancelled'))
      OR (OLD.status = 'approved' AND NEW.status IN ('processing', 'cancelled'))
      OR (OLD.status = 'processing' AND NEW.status IN ('completed', 'failed'))
      OR (OLD.status = 'failed' AND NEW.status IN ('approved', 'denied'))
    )
  THEN
    RAISE EXCEPTION 'Invalid Privacy Request transition';
  END IF;
  IF NEW.request_type = 'erasure' AND NEW.status = 'processing' AND EXISTS (
    SELECT 1
    FROM legal_holds AS legal_hold
    WHERE legal_hold.tenant_id = NEW.tenant_id
      AND legal_hold.status = 'active'
      AND (
        legal_hold.scope_type = 'tenant'
        OR (legal_hold.scope_type = 'user' AND legal_hold.scope_id = NEW.subject_user_id)
        OR (legal_hold.scope_type IN ('organization', 'project', 'session') AND EXISTS (
          SELECT 1
          FROM agent_sessions AS held_session
          WHERE held_session.tenant_id = NEW.tenant_id
            AND (
              held_session.created_by = NEW.subject_user_id
              OR EXISTS (
                SELECT 1 FROM agent_turns AS held_turn
                WHERE held_turn.tenant_id = held_session.tenant_id
                  AND held_turn.session_id = held_session.id
                  AND held_turn.created_by = NEW.subject_user_id
              )
            )
            AND (
              (legal_hold.scope_type = 'organization' AND legal_hold.scope_id = held_session.organization_id)
              OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = held_session.project_id)
              OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = held_session.id)
            )
        ))
      )
  ) THEN
    RAISE EXCEPTION 'Privacy erasure is blocked by an active Legal Hold';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_privacy_requests_update_guard
BEFORE UPDATE ON privacy_requests
FOR EACH ROW EXECUTE FUNCTION validate_privacy_request_update();

CREATE TRIGGER trg_privacy_requests_updated_at
BEFORE UPDATE ON privacy_requests
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION reject_privacy_request_event_mutation()
RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'Privacy Request events are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_privacy_request_events_update
BEFORE UPDATE ON privacy_request_events
FOR EACH ROW EXECUTE FUNCTION reject_privacy_request_event_mutation();

CREATE TRIGGER trg_privacy_request_events_delete
BEFORE DELETE ON privacy_request_events
FOR EACH ROW EXECUTE FUNCTION reject_privacy_request_event_mutation();

COMMENT ON TABLE privacy_requests IS
  'Tenant-scoped DSAR state machine for access export and erasure, with versioned transitions and immutable history.';

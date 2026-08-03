CREATE TABLE legal_holds (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  scope_type TEXT NOT NULL CHECK (scope_type IN ('tenant', 'user', 'organization', 'project', 'session')),
  scope_id UUID NOT NULL,
  name TEXT NOT NULL,
  matter_reference TEXT NOT NULL,
  reason TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'released')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  released_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  release_reason TEXT,
  released_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160),
  CHECK (length(btrim(matter_reference)) BETWEEN 1 AND 160),
  CHECK (length(btrim(reason)) BETWEEN 10 AND 2000),
  CHECK (release_reason IS NULL OR length(btrim(release_reason)) BETWEEN 10 AND 2000),
  CHECK (
    (status = 'active' AND released_by IS NULL AND release_reason IS NULL AND released_at IS NULL)
    OR
    (status = 'released' AND released_by IS NOT NULL AND release_reason IS NOT NULL AND released_at IS NOT NULL)
  ),
  CHECK (released_at IS NULL OR released_at >= created_at)
);

CREATE UNIQUE INDEX uq_legal_holds_active_scope_matter
  ON legal_holds (tenant_id, scope_type, scope_id, lower(matter_reference))
  WHERE status = 'active';

CREATE INDEX idx_legal_holds_retention_gate
  ON legal_holds (tenant_id, status, scope_type, scope_id, id);

CREATE OR REPLACE FUNCTION validate_legal_hold_scope()
RETURNS trigger AS $$
BEGIN
  IF NEW.scope_type = 'tenant' THEN
    IF NEW.scope_id <> NEW.tenant_id THEN
      RAISE EXCEPTION 'Tenant Legal Hold scope must equal tenant_id';
    END IF;
  ELSIF NEW.scope_type = 'user' THEN
    IF NOT EXISTS (
      SELECT 1 FROM tenant_memberships
      WHERE tenant_id = NEW.tenant_id AND user_id = NEW.scope_id
    ) THEN
      RAISE EXCEPTION 'User Legal Hold scope does not belong to Tenant';
    END IF;
  ELSIF NEW.scope_type = 'organization' THEN
    IF NOT EXISTS (
      SELECT 1 FROM organizations
      WHERE tenant_id = NEW.tenant_id AND id = NEW.scope_id
    ) THEN
      RAISE EXCEPTION 'Organization Legal Hold scope does not belong to Tenant';
    END IF;
  ELSIF NEW.scope_type = 'project' THEN
    IF NOT EXISTS (
      SELECT 1 FROM projects
      WHERE tenant_id = NEW.tenant_id AND id = NEW.scope_id
    ) THEN
      RAISE EXCEPTION 'Project Legal Hold scope does not belong to Tenant';
    END IF;
  ELSIF NEW.scope_type = 'session' THEN
    IF NOT EXISTS (
      SELECT 1 FROM agent_sessions
      WHERE tenant_id = NEW.tenant_id AND id = NEW.scope_id
    ) THEN
      RAISE EXCEPTION 'Session Legal Hold scope does not belong to Tenant';
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_legal_holds_scope
BEFORE INSERT ON legal_holds
FOR EACH ROW EXECUTE FUNCTION validate_legal_hold_scope();

CREATE OR REPLACE FUNCTION validate_legal_hold_update()
RETURNS trigger AS $$
BEGIN
  IF NEW.id <> OLD.id
    OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.scope_type <> OLD.scope_type
    OR NEW.scope_id <> OLD.scope_id
    OR NEW.name <> OLD.name
    OR NEW.matter_reference <> OLD.matter_reference
    OR NEW.reason <> OLD.reason
    OR NEW.created_by <> OLD.created_by
    OR NEW.created_at <> OLD.created_at
    OR OLD.status <> 'active'
    OR NEW.status <> 'released'
    OR NEW.version <> OLD.version + 1
  THEN
    RAISE EXCEPTION 'Legal Hold updates must be one-way active to released transitions';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_legal_holds_update_guard
BEFORE UPDATE ON legal_holds
FOR EACH ROW EXECUTE FUNCTION validate_legal_hold_update();

CREATE TRIGGER trg_legal_holds_updated_at
BEFORE UPDATE ON legal_holds
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE legal_holds IS
  'Audited, one-way Legal Hold records that gate Tenant deletion and scoped retention cleanup.';

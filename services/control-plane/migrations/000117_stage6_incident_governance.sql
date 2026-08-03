CREATE TABLE stage6_incidents (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  incident_key TEXT NOT NULL UNIQUE CHECK (length(incident_key) BETWEEN 3 AND 120),
  severity TEXT NOT NULL CHECK (severity IN ('SEV-0', 'SEV-1', 'SEV-2', 'SEV-3')),
  state TEXT NOT NULL CHECK (state IN ('investigating', 'identified', 'monitoring', 'resolved', 'cancelled')),
  title TEXT NOT NULL CHECK (length(trim(title)) BETWEEN 10 AND 200),
  customer_impact_summary TEXT NOT NULL CHECK (length(trim(customer_impact_summary)) BETWEEN 20 AND 2000),
  public_impact BOOLEAN NOT NULL,
  security_privacy_impact BOOLEAN NOT NULL,
  status_page_origin TEXT CHECK (
    status_page_origin ~ '^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$'
    AND length(status_page_origin) BETWEEN 12 AND 300
  ),
  status_page_incident_reference TEXT CHECK (length(trim(status_page_incident_reference)) BETWEEN 3 AND 300),
  affected_components JSONB NOT NULL CHECK (jsonb_typeof(affected_components) = 'array'),
  affected_regions JSONB NOT NULL CHECK (jsonb_typeof(affected_regions) = 'array'),
  incident_commander_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  communications_lead_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  security_privacy_lead_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
  started_at TIMESTAMPTZ NOT NULL,
  impact_confirmed_at TIMESTAMPTZ NOT NULL,
  first_public_update_due_at TIMESTAMPTZ,
  next_public_update_due_at TIMESTAMPTZ,
  resolved_at TIMESTAMPTZ,
  cancelled_at TIMESTAMPTZ,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_stage6_incidents_state
  ON stage6_incidents (state, severity, updated_at DESC, id);

CREATE TABLE stage6_incident_updates (
  id UUID PRIMARY KEY,
  incident_id UUID NOT NULL REFERENCES stage6_incidents(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  update_kind TEXT NOT NULL CHECK (update_kind IN ('initial', 'progress', 'resolved')),
  summary TEXT NOT NULL CHECK (length(trim(summary)) BETWEEN 20 AND 2000),
  published_at TIMESTAMPTZ NOT NULL,
  external_reference TEXT NOT NULL CHECK (length(trim(external_reference)) BETWEEN 12 AND 2048),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (incident_id, published_at),
  UNIQUE (incident_id, external_reference)
);

CREATE INDEX idx_stage6_incident_updates_incident
  ON stage6_incident_updates (incident_id, published_at, id);

CREATE TABLE stage6_incident_resolution_approvals (
  id UUID PRIMARY KEY,
  incident_id UUID NOT NULL UNIQUE REFERENCES stage6_incidents(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 12 AND 2048),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION enforce_stage6_incident()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  component_count INTEGER;
  region_count INTEGER;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT count(*) INTO component_count FROM jsonb_array_elements_text(NEW.affected_components);
    SELECT count(*) INTO region_count FROM jsonb_array_elements_text(NEW.affected_regions);
    IF NEW.state <> 'investigating' OR NEW.version <> 1
       OR NEW.started_at > NEW.impact_confirmed_at OR NEW.impact_confirmed_at > NEW.created_at + interval '5 minutes'
       OR NEW.resolved_at IS NOT NULL OR NEW.cancelled_at IS NOT NULL
       OR NEW.incident_commander_user_id = NEW.communications_lead_user_id
       OR NEW.created_by <> NEW.incident_commander_user_id
       OR NEW.severity IN ('SEV-0', 'SEV-1') AND NOT NEW.public_impact
       OR NEW.security_privacy_impact AND (
         NEW.security_privacy_lead_user_id IS NULL
         OR NEW.security_privacy_lead_user_id = NEW.incident_commander_user_id
       )
       OR NOT NEW.security_privacy_impact AND NEW.security_privacy_lead_user_id IS NOT NULL
       OR NEW.public_impact AND (
         NEW.status_page_origin IS NULL
         OR NEW.first_public_update_due_at IS NULL OR NEW.next_public_update_due_at IS NULL
       )
       OR NOT NEW.public_impact AND (
         NEW.status_page_origin IS NOT NULL OR NEW.status_page_incident_reference IS NOT NULL
         OR NEW.first_public_update_due_at IS NOT NULL OR NEW.next_public_update_due_at IS NOT NULL
       )
       OR component_count NOT BETWEEN 1 AND 6
       OR EXISTS (
         SELECT 1 FROM jsonb_array_elements_text(NEW.affected_components) AS component
         WHERE component NOT IN (
           'control-plane-api', 'authentication-sso', 'execution-scheduling',
           'worker-runtime', 'artifact-service', 'web-application'
         )
       )
       OR EXISTS (
         SELECT value FROM jsonb_array_elements_text(NEW.affected_components)
         GROUP BY value HAVING count(*) > 1
       )
       OR region_count NOT BETWEEN 1 AND 32
       OR EXISTS (
         SELECT value FROM jsonb_array_elements_text(NEW.affected_regions)
         GROUP BY value HAVING count(*) > 1
       )
       OR NOT EXISTS (
         SELECT 1 FROM tenant_memberships membership
         JOIN tenants operator_tenant ON operator_tenant.id = membership.tenant_id
         JOIN users operator_user ON operator_user.id = membership.user_id
         WHERE membership.tenant_id = NEW.operator_tenant_id
           AND membership.user_id = NEW.incident_commander_user_id
           AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
           AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
           AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
       )
       OR NOT EXISTS (
         SELECT 1 FROM tenant_memberships membership
         JOIN users operator_user ON operator_user.id = membership.user_id
         WHERE membership.tenant_id = NEW.operator_tenant_id
           AND membership.user_id = NEW.communications_lead_user_id
           AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
           AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
       )
       OR NEW.security_privacy_lead_user_id IS NOT NULL AND NOT EXISTS (
         SELECT 1 FROM tenant_memberships membership
         JOIN users operator_user ON operator_user.id = membership.user_id
         WHERE membership.tenant_id = NEW.operator_tenant_id
           AND membership.user_id = NEW.security_privacy_lead_user_id
           AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
           AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 incident initial state' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.incident_key IS DISTINCT FROM OLD.incident_key
     OR NEW.severity IS DISTINCT FROM OLD.severity
     OR NEW.title IS DISTINCT FROM OLD.title
     OR NEW.customer_impact_summary IS DISTINCT FROM OLD.customer_impact_summary
     OR NEW.public_impact IS DISTINCT FROM OLD.public_impact
     OR NEW.security_privacy_impact IS DISTINCT FROM OLD.security_privacy_impact
     OR NEW.status_page_origin IS DISTINCT FROM OLD.status_page_origin
     OR NEW.affected_components IS DISTINCT FROM OLD.affected_components
     OR NEW.affected_regions IS DISTINCT FROM OLD.affected_regions
     OR NEW.incident_commander_user_id IS DISTINCT FROM OLD.incident_commander_user_id
     OR NEW.communications_lead_user_id IS DISTINCT FROM OLD.communications_lead_user_id
     OR NEW.security_privacy_lead_user_id IS DISTINCT FROM OLD.security_privacy_lead_user_id
     OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.impact_confirmed_at IS DISTINCT FROM OLD.impact_confirmed_at
     OR NEW.first_public_update_due_at IS DISTINCT FROM OLD.first_public_update_due_at
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Stage 6 incident identity and authority are immutable' USING ERRCODE = '23514';
  END IF;

  IF OLD.state IN ('resolved', 'cancelled') THEN
    RAISE EXCEPTION 'Terminal Stage 6 incidents are immutable' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = OLD.state THEN
    IF OLD.status_page_incident_reference IS NULL
       AND NEW.status_page_incident_reference IS NOT NULL
       AND NEW.status_page_incident_reference <> ''
       AND NEW.next_public_update_due_at IS NOT DISTINCT FROM OLD.next_public_update_due_at
       AND NEW.resolved_at IS NOT DISTINCT FROM OLD.resolved_at
       AND NEW.cancelled_at IS NOT DISTINCT FROM OLD.cancelled_at THEN
      RETURN NEW;
    END IF;
    IF NEW.status_page_incident_reference IS NOT DISTINCT FROM OLD.status_page_incident_reference
       AND NEW.resolved_at IS NOT DISTINCT FROM OLD.resolved_at
       AND NEW.cancelled_at IS NOT DISTINCT FROM OLD.cancelled_at
       AND EXISTS (
         SELECT 1 FROM (
           SELECT update_kind, published_at
           FROM stage6_incident_updates
           WHERE incident_id = NEW.id
           ORDER BY published_at DESC, id DESC LIMIT 1
         ) latest
         WHERE NEW.next_public_update_due_at IS NOT DISTINCT FROM CASE
           WHEN latest.update_kind = 'resolved' THEN NULL
           WHEN NEW.severity IN ('SEV-0', 'SEV-1') THEN latest.published_at + interval '30 minutes'
           ELSE latest.published_at + interval '60 minutes'
         END
       ) THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'Invalid Stage 6 incident same-state projection' USING ERRCODE = '23514';
  END IF;

  IF NEW.status_page_incident_reference IS DISTINCT FROM OLD.status_page_incident_reference
     OR (OLD.state = 'investigating' AND NEW.state NOT IN ('identified', 'cancelled'))
     OR (OLD.state = 'identified' AND NEW.state NOT IN ('monitoring', 'cancelled'))
     OR (OLD.state = 'monitoring' AND NEW.state <> 'resolved') THEN
    RAISE EXCEPTION 'Invalid Stage 6 incident transition' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = 'cancelled' AND (
    NEW.cancelled_at IS NULL OR NEW.resolved_at IS NOT NULL
    OR EXISTS (SELECT 1 FROM stage6_incident_updates WHERE incident_id = NEW.id)
  ) THEN
    RAISE EXCEPTION 'Published Stage 6 incidents cannot be cancelled' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = 'resolved' AND (
    NEW.resolved_at IS NULL OR NEW.cancelled_at IS NOT NULL OR NEW.next_public_update_due_at IS NOT NULL
    OR NEW.public_impact AND NOT EXISTS (
      SELECT 1 FROM stage6_incident_updates WHERE incident_id = NEW.id AND update_kind = 'resolved'
    )
    OR NEW.security_privacy_impact AND NOT EXISTS (
      SELECT 1 FROM stage6_incident_resolution_approvals
      WHERE incident_id = NEW.id AND decision = 'approved'
    )
  ) THEN
    RAISE EXCEPTION 'Stage 6 incident resolution evidence is incomplete' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_incidents_guard
BEFORE INSERT OR UPDATE ON stage6_incidents
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_incident();

CREATE OR REPLACE FUNCTION enforce_stage6_incident_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  incident stage6_incidents%ROWTYPE;
  previous_kind TEXT;
  previous_published_at TIMESTAMPTZ;
BEGIN
  SELECT * INTO incident FROM stage6_incidents WHERE id = NEW.incident_id FOR UPDATE;
  SELECT update_kind, published_at INTO previous_kind, previous_published_at
  FROM stage6_incident_updates WHERE incident_id = NEW.incident_id
  ORDER BY published_at DESC, id DESC LIMIT 1;

  IF incident.id IS NULL OR incident.operator_tenant_id <> NEW.operator_tenant_id
     OR NOT incident.public_impact OR incident.status_page_incident_reference IS NULL
     OR incident.state IN ('resolved', 'cancelled')
     OR NEW.created_by <> incident.communications_lead_user_id
     OR NEW.published_at < incident.impact_confirmed_at OR NEW.published_at > now() + interval '5 minutes'
     OR previous_kind = 'resolved'
     OR previous_published_at IS NOT NULL AND NEW.published_at <= previous_published_at
     OR previous_kind IS NULL AND NEW.update_kind <> 'initial'
     OR previous_kind IS NOT NULL AND NEW.update_kind = 'initial'
     OR NEW.update_kind = 'resolved' AND incident.state <> 'monitoring'
     OR NEW.external_reference !~ '^https://'
     OR left(NEW.external_reference, length(incident.status_page_origin) + 1) <> incident.status_page_origin || '/'
     OR NOT EXISTS (
       SELECT 1 FROM tenant_memberships membership
       JOIN users operator_user ON operator_user.id = membership.user_id
       WHERE membership.tenant_id = incident.operator_tenant_id
         AND membership.user_id = NEW.created_by
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 public incident update' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_incident_updates_insert
BEFORE INSERT ON stage6_incident_updates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_incident_update();

CREATE OR REPLACE FUNCTION reject_stage6_incident_update_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 public incident updates are immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_incident_updates_no_update
BEFORE UPDATE ON stage6_incident_updates
FOR EACH ROW EXECUTE FUNCTION reject_stage6_incident_update_mutation();

CREATE TRIGGER trg_stage6_incident_updates_no_delete
BEFORE DELETE ON stage6_incident_updates
FOR EACH ROW EXECUTE FUNCTION reject_stage6_incident_update_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_incident_resolution_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM stage6_incidents incident
    JOIN tenant_memberships membership
      ON membership.tenant_id = incident.operator_tenant_id
     AND membership.user_id = NEW.approver_user_id
     AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
    JOIN users operator_user ON operator_user.id = membership.user_id
     AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
    WHERE incident.id = NEW.incident_id
      AND incident.operator_tenant_id = NEW.operator_tenant_id
      AND incident.security_privacy_impact
      AND incident.state = 'monitoring'
      AND incident.security_privacy_lead_user_id = NEW.approver_user_id
      AND incident.incident_commander_user_id <> NEW.approver_user_id
  ) OR NEW.evidence_reference !~ '^https://' THEN
    RAISE EXCEPTION 'Invalid Stage 6 incident resolution approval' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_incident_resolution_approvals_insert
BEFORE INSERT ON stage6_incident_resolution_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_incident_resolution_approval();

CREATE TRIGGER trg_stage6_incident_resolution_approvals_no_update
BEFORE UPDATE ON stage6_incident_resolution_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_incident_update_mutation();

CREATE TRIGGER trg_stage6_incident_resolution_approvals_no_delete
BEFORE DELETE ON stage6_incident_resolution_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_incident_update_mutation();

CREATE OR REPLACE FUNCTION reject_stage6_incident_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 incidents are retained as immutable operational history' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_incidents_no_delete
BEFORE DELETE ON stage6_incidents
FOR EACH ROW EXECUTE FUNCTION reject_stage6_incident_delete();

CREATE TRIGGER trg_stage6_incidents_updated_at
BEFORE UPDATE ON stage6_incidents
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE stage6_incidents IS
  'Platform incident-governance authority; external Status Page delivery and real paging exercises remain separate GA evidence.';

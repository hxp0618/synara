CREATE TABLE execution_location_outages (
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  region TEXT NOT NULL,
  cluster_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('draining', 'unreachable')),
  publisher_identity TEXT NOT NULL,
  reason TEXT,
  observed_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, region, cluster_id),
  CHECK (expires_at > observed_at),
  CHECK (length(btrim(region)) BETWEEN 1 AND 120),
  CHECK (cluster_id = '' OR length(btrim(cluster_id)) BETWEEN 1 AND 200),
  CHECK (length(btrim(publisher_identity)) BETWEEN 1 AND 200),
  CHECK (reason IS NULL OR length(reason) <= 2000)
);

CREATE INDEX idx_execution_location_outages_route
  ON execution_location_outages (tenant_id, region, cluster_id, expires_at);

CREATE OR REPLACE FUNCTION enforce_execution_location_outages_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Location outage observations cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.tenant_id <> OLD.tenant_id
     OR NEW.region <> OLD.region
     OR NEW.cluster_id <> OLD.cluster_id THEN
    RAISE EXCEPTION 'Location outage authority identity is immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 OR NEW.observed_at <= OLD.observed_at THEN
    RAISE EXCEPTION 'Location outage authority update is stale or not monotonic'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_location_outages_update
  ON execution_location_outages;
CREATE TRIGGER trg_execution_location_outages_update
BEFORE UPDATE OR DELETE ON execution_location_outages
FOR EACH ROW EXECUTE FUNCTION enforce_execution_location_outages_update();

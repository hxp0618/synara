CREATE TABLE outbox_pressure_state (
  singleton_key TEXT PRIMARY KEY CHECK (singleton_key = 'global'),
  base_batch_size INTEGER NOT NULL CHECK (base_batch_size BETWEEN 1 AND 1000),
  max_batch_size INTEGER NOT NULL CHECK (max_batch_size BETWEEN base_batch_size AND 10000),
  max_concurrency INTEGER NOT NULL CHECK (max_concurrency BETWEEN 1 AND 128),
  scale_up_depth BIGINT NOT NULL CHECK (scale_up_depth BETWEEN 1 AND 1000000000),
  target_delay_nanoseconds BIGINT NOT NULL CHECK (target_delay_nanoseconds BETWEEN 1000000 AND 3600000000000),
  throttle_depth BIGINT NOT NULL CHECK (throttle_depth > scale_up_depth AND throttle_depth <= 1000000000),
  pending BIGINT NOT NULL DEFAULT 0 CHECK (pending >= 0),
  oldest_pending_nanoseconds BIGINT NOT NULL DEFAULT 0 CHECK (oldest_pending_nanoseconds >= 0),
  desired_batch_size INTEGER NOT NULL CHECK (desired_batch_size BETWEEN base_batch_size AND max_batch_size),
  desired_concurrency INTEGER NOT NULL CHECK (desired_concurrency BETWEEN 1 AND max_concurrency),
  status TEXT NOT NULL CHECK (status IN ('normal', 'scaling', 'throttled')),
  observed_at TIMESTAMPTZ NOT NULL,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO outbox_pressure_state (
  singleton_key, base_batch_size, max_batch_size, max_concurrency,
  scale_up_depth, target_delay_nanoseconds, throttle_depth,
  pending, oldest_pending_nanoseconds, desired_batch_size, desired_concurrency,
  status, observed_at, version
) VALUES (
  'global', 50, 500, 8, 100, 5000000000, 100000,
  0, 0, 50, 1, 'normal', now(), 1
);

CREATE OR REPLACE FUNCTION enforce_outbox_pressure_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND (
    NEW.singleton_key <> OLD.singleton_key
    OR NEW.version <> OLD.version + 1
  ) THEN
    RAISE EXCEPTION 'Outbox pressure state must advance exactly once' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_outbox_pressure_state ON outbox_pressure_state;
CREATE TRIGGER trg_outbox_pressure_state
BEFORE UPDATE ON outbox_pressure_state
FOR EACH ROW EXECUTE FUNCTION enforce_outbox_pressure_state();

CREATE TABLE billing_shared_allocation_schedule_periods (
  schedule_config_sha256 TEXT NOT NULL,
  billing_period_start_at TIMESTAMPTZ NOT NULL,
  billing_period_end_at TIMESTAMPTZ NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  currency_code TEXT NOT NULL,
  schedule_kind TEXT NOT NULL,
  settlement_delay_seconds BIGINT NOT NULL,
  schedule_interval_seconds BIGINT NOT NULL,
  next_attempt_at TIMESTAMPTZ NOT NULL,
  attempt_count BIGINT NOT NULL DEFAULT 0,
  last_started_at TIMESTAMPTZ,
  last_finished_at TIMESTAMPTZ,
  last_success_at TIMESTAMPTZ,
  last_outcome TEXT NOT NULL DEFAULT 'never',
  last_error_code TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_billing_shared_allocation_schedule_periods PRIMARY KEY (
    schedule_config_sha256, billing_period_start_at, billing_period_end_at
  ),
  CONSTRAINT chk_billing_shared_allocation_schedule_digest CHECK (
    schedule_config_sha256 ~ '^[0-9a-f]{64}$'
  ),
  CONSTRAINT chk_billing_shared_allocation_schedule_scope CHECK (
    provider IN ('aws', 'gcp', 'azure')
    AND currency_code ~ '^[A-Z]{3}$'
    AND schedule_kind IN ('static', 'monthly-utc')
  ),
  CONSTRAINT chk_billing_shared_allocation_schedule_period CHECK (
    billing_period_end_at > billing_period_start_at
    AND billing_period_end_at <= billing_period_start_at + INTERVAL '366 days'
    AND (
      schedule_kind = 'static'
      OR (
        billing_period_start_at = date_trunc('month', billing_period_start_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
        AND billing_period_end_at = billing_period_start_at + INTERVAL '1 month'
      )
    )
  ),
  CONSTRAINT chk_billing_shared_allocation_schedule_settings CHECK (
    settlement_delay_seconds BETWEEN 60 AND 7776000
    AND schedule_interval_seconds >= 60
    AND next_attempt_at >= billing_period_end_at + settlement_delay_seconds * INTERVAL '1 second'
  ),
  CONSTRAINT chk_billing_shared_allocation_schedule_attempt CHECK (
    attempt_count >= 0
    AND created_at <= updated_at
    AND (
      (
        last_outcome = 'never'
        AND attempt_count = 0
        AND last_started_at IS NULL
        AND last_finished_at IS NULL
        AND last_success_at IS NULL
        AND last_error_code IS NULL
      )
      OR (
        last_outcome = 'running'
        AND attempt_count > 0
        AND last_started_at IS NOT NULL
        AND next_attempt_at > last_started_at
        AND last_error_code IS NULL
      )
      OR (
        last_outcome = 'completed'
        AND attempt_count > 0
        AND last_started_at IS NOT NULL
        AND last_finished_at >= last_started_at
        AND last_success_at = last_finished_at
        AND last_error_code IS NULL
      )
      OR (
        last_outcome = 'failed'
        AND attempt_count > 0
        AND last_started_at IS NOT NULL
        AND last_finished_at >= last_started_at
        AND last_error_code ~ '^[a-z0-9][a-z0-9_-]{0,119}$'
        AND (last_success_at IS NULL OR last_success_at <= last_finished_at)
      )
    )
  )
);

CREATE INDEX idx_billing_shared_allocation_schedule_due
  ON billing_shared_allocation_schedule_periods (
    next_attempt_at, execution_target_id, billing_period_start_at
  );

CREATE OR REPLACE FUNCTION enforce_billing_shared_allocation_schedule_period()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Billing shared allocation schedule periods cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.schedule_config_sha256 <> OLD.schedule_config_sha256
     OR NEW.billing_period_start_at <> OLD.billing_period_start_at
     OR NEW.billing_period_end_at <> OLD.billing_period_end_at
     OR NEW.execution_target_id <> OLD.execution_target_id
     OR NEW.provider <> OLD.provider
     OR NEW.currency_code <> OLD.currency_code
     OR NEW.schedule_kind <> OLD.schedule_kind
     OR NEW.settlement_delay_seconds <> OLD.settlement_delay_seconds
     OR NEW.schedule_interval_seconds <> OLD.schedule_interval_seconds
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Billing shared allocation schedule period identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.attempt_count < OLD.attempt_count
     OR NEW.next_attempt_at < OLD.next_attempt_at
     OR NEW.updated_at < OLD.updated_at
     OR (OLD.last_started_at IS NOT NULL AND (
       NEW.last_started_at IS NULL OR NEW.last_started_at < OLD.last_started_at
     ))
     OR (OLD.last_finished_at IS NOT NULL AND (
       NEW.last_finished_at IS NULL OR NEW.last_finished_at < OLD.last_finished_at
     ))
     OR (OLD.last_success_at IS NOT NULL AND (
       NEW.last_success_at IS NULL OR NEW.last_success_at < OLD.last_success_at
     )) THEN
    RAISE EXCEPTION 'Billing shared allocation schedule period timeline cannot regress' USING ERRCODE = '23514';
  END IF;
  IF NEW.last_started_at IS DISTINCT FROM OLD.last_started_at THEN
    IF NEW.last_started_at IS NULL
       OR (OLD.last_started_at IS NOT NULL AND NEW.last_started_at <= OLD.last_started_at)
       OR NEW.attempt_count <> OLD.attempt_count + 1
       OR NEW.last_outcome <> 'running'
       OR NEW.last_error_code IS NOT NULL
       OR NEW.next_attempt_at <= NEW.last_started_at THEN
      RAISE EXCEPTION 'Billing shared allocation schedule period claim is invalid' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.attempt_count <> OLD.attempt_count THEN
    RAISE EXCEPTION 'Billing shared allocation schedule attempt count requires a new claim' USING ERRCODE = '23514';
  END IF;
  IF NEW.last_finished_at IS DISTINCT FROM OLD.last_finished_at THEN
    IF OLD.last_outcome <> 'running'
       OR NEW.last_finished_at IS NULL
       OR NEW.last_started_at IS NULL
       OR NEW.last_finished_at < NEW.last_started_at
       OR NEW.last_outcome NOT IN ('completed', 'failed') THEN
      RAISE EXCEPTION 'Billing shared allocation schedule period completion is invalid' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.last_started_at IS NOT DISTINCT FROM OLD.last_started_at
        AND (
          NEW.next_attempt_at <> OLD.next_attempt_at
          OR NEW.last_outcome <> OLD.last_outcome
          OR NEW.last_error_code IS DISTINCT FROM OLD.last_error_code
          OR NEW.last_success_at IS DISTINCT FROM OLD.last_success_at
        ) THEN
    RAISE EXCEPTION 'Billing shared allocation schedule period mutation lacks a claim or completion' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_billing_shared_allocation_schedule_periods_immutable
BEFORE UPDATE OR DELETE ON billing_shared_allocation_schedule_periods
FOR EACH ROW EXECUTE FUNCTION enforce_billing_shared_allocation_schedule_period();

COMMENT ON TABLE billing_shared_allocation_schedule_periods IS
  'Durable due, claim, and outcome state for exact static or generated monthly-UTC shared allocation periods.';

ALTER TABLE execution_usage_summaries
  ADD COLUMN provider_cost_reported BOOLEAN NOT NULL DEFAULT false;

UPDATE execution_usage_summaries
SET provider_cost_reported = true
WHERE provider_cost_micros > 0;

ALTER TABLE execution_usage_summaries
  ADD CONSTRAINT execution_usage_provider_cost_reporting_shape CHECK (
    provider_cost_reported OR provider_cost_micros = 0
  );

CREATE OR REPLACE FUNCTION enforce_execution_usage_provider_cost_reporting()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.provider_cost_reported AND NOT NEW.provider_cost_reported
     OR NEW.provider_cost_micros < OLD.provider_cost_micros
     OR OLD.provider_cost_reported AND NEW.currency_code <> OLD.currency_code THEN
    RAISE EXCEPTION 'Execution Usage Provider cost reporting is monotonic'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_usage_provider_cost_reporting
BEFORE UPDATE ON execution_usage_summaries
FOR EACH ROW EXECUTE FUNCTION enforce_execution_usage_provider_cost_reporting();

COMMENT ON COLUMN execution_usage_summaries.provider_cost_reported IS
  'True only when the Provider/runtime explicitly reported total cost, including an explicit zero; false means unavailable and must not be displayed as zero cost.';

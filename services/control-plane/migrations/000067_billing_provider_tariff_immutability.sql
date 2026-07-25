-- Serialize one provider/region/currency catalog key before checking overlap.
-- The transaction-scoped advisory lock closes the READ COMMITTED race where
-- two different tariff versions could otherwise pass the trigger against
-- each other's uncommitted rows.
CREATE OR REPLACE FUNCTION assert_billing_provider_tariff_non_overlapping()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(
    hashtextextended(
      concat_ws(chr(31),
        'synara:billing-provider-tariff',
        NEW.provider,
        NEW.region,
        NEW.currency_code
      ),
      0
    )
  );
  IF EXISTS (
    SELECT 1
    FROM billing_provider_tariffs AS existing
    WHERE existing.id <> NEW.id
      AND existing.provider = NEW.provider
      AND existing.region = NEW.region
      AND existing.currency_code = NEW.currency_code
      AND existing.effective_start_at < COALESCE(NEW.effective_end_at, 'infinity'::timestamptz)
      AND COALESCE(existing.effective_end_at, 'infinity'::timestamptz) > NEW.effective_start_at
  ) THEN
    RAISE EXCEPTION 'billing provider tariff effective interval overlaps another tariff'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_billing_provider_tariff_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'billing provider tariffs are immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_billing_provider_tariffs_immutable ON billing_provider_tariffs;
CREATE TRIGGER trg_billing_provider_tariffs_immutable
BEFORE UPDATE OR DELETE ON billing_provider_tariffs
FOR EACH ROW EXECUTE FUNCTION enforce_billing_provider_tariff_immutable();

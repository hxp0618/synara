ALTER TABLE execution_interactions
  ADD COLUMN event_version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE execution_interactions
  ADD CONSTRAINT chk_execution_interactions_event_version
    CHECK (event_version IN (1, 2));

COMMENT ON COLUMN execution_interactions.event_version IS
  'Runtime Event contract version used by the request and its matching resolution event.';

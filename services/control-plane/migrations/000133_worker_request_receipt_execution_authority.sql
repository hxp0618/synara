ALTER TABLE worker_request_receipts
  ADD COLUMN tenant_id UUID,
  ADD COLUMN execution_id UUID,
  ADD COLUMN execution_target_id UUID,
  ADD COLUMN execution_generation BIGINT,
  ADD CONSTRAINT chk_worker_request_receipts_execution_authority
    CHECK (
      (tenant_id IS NULL
        AND execution_id IS NULL
        AND execution_target_id IS NULL
        AND execution_generation IS NULL)
      OR
      (tenant_id IS NOT NULL
        AND execution_id IS NOT NULL
        AND execution_target_id IS NOT NULL
        AND execution_generation > 0)
    ),
  ADD CONSTRAINT fk_worker_request_receipts_execution
    FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id)
    ON DELETE CASCADE,
  ADD CONSTRAINT fk_worker_request_receipts_execution_target
    FOREIGN KEY (execution_target_id)
    REFERENCES execution_targets(id)
    ON DELETE CASCADE;

CREATE INDEX idx_worker_request_receipts_execution_authority
  ON worker_request_receipts (
    tenant_id, execution_id, execution_generation, execution_target_id, worker_id, request_id
  )
  WHERE execution_id IS NOT NULL;

COMMENT ON COLUMN worker_request_receipts.execution_generation IS
  'Immutable Execution Generation authority revalidated before an idempotent Worker response may be replayed.';

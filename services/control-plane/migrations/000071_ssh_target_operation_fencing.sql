ALTER TABLE execution_targets
  ADD COLUMN ssh_operation_generation BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN ssh_operation_kind TEXT,
  ADD COLUMN ssh_operation_started_at TIMESTAMPTZ,
  ADD CONSTRAINT chk_execution_targets_ssh_operation_fence
  CHECK (
    (
      ssh_operation_kind IS NULL
      AND ssh_operation_started_at IS NULL
    ) OR (
      kind = 'ssh'
      AND ssh_operation_generation > 0
      AND ssh_operation_kind IN ('install', 'upgrade', 'revoke')
      AND ssh_operation_started_at IS NOT NULL
    )
  );

CREATE INDEX idx_execution_targets_ssh_operation_active
  ON execution_targets (ssh_operation_started_at, id)
  WHERE kind = 'ssh' AND ssh_operation_kind IS NOT NULL;

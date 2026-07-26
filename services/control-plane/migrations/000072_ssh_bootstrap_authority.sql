ALTER TABLE execution_targets
  ADD COLUMN ssh_expected_instance_uid UUID;

-- Pre-authority install/upgrade rows cannot be resumed safely because their
-- exact provisioner-generated instance UID was never persisted. Leave the
-- Target offline and clear only the stale bootstrap operation.
UPDATE execution_targets
SET ssh_operation_kind = NULL,
    ssh_operation_started_at = NULL
WHERE kind = 'ssh'
  AND ssh_operation_kind IN ('install', 'upgrade');

ALTER TABLE execution_targets
  DROP CONSTRAINT chk_execution_targets_ssh_operation_fence,
  ADD CONSTRAINT chk_execution_targets_ssh_operation_fence
  CHECK (
    (
      ssh_operation_kind IS NULL
      AND ssh_operation_started_at IS NULL
      AND ssh_expected_instance_uid IS NULL
    ) OR (
      kind = 'ssh'
      AND ssh_operation_generation > 0
      AND ssh_operation_kind IN ('install', 'upgrade')
      AND ssh_operation_started_at IS NOT NULL
      AND ssh_expected_instance_uid IS NOT NULL
    ) OR (
      kind = 'ssh'
      AND ssh_operation_generation > 0
      AND ssh_operation_kind = 'revoke'
      AND ssh_operation_started_at IS NOT NULL
      AND ssh_expected_instance_uid IS NULL
    )
  );

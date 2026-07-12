ALTER TABLE execution_interactions
  ADD COLUMN turn_id UUID,
  ADD COLUMN provider TEXT,
  ADD COLUMN expires_at TIMESTAMPTZ,
  ADD COLUMN resolution_kind TEXT,
  ADD COLUMN resolution_command_id TEXT,
  ADD COLUMN delivery_status TEXT NOT NULL DEFAULT 'not-ready',
  ADD COLUMN delivery_worker_id UUID REFERENCES worker_instances(id) ON DELETE RESTRICT,
  ADD COLUMN delivery_generation BIGINT,
  ADD COLUMN delivery_attempts INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN delivery_available_at TIMESTAMPTZ,
  ADD COLUMN delivered_at TIMESTAMPTZ,
  ADD COLUMN acknowledged_at TIMESTAMPTZ,
  ADD COLUMN delivery_error TEXT;

UPDATE execution_interactions AS interaction
SET turn_id = execution.turn_id,
    provider = execution.provider,
    expires_at = interaction.requested_at + interval '24 hours'
FROM agent_executions AS execution
WHERE execution.tenant_id = interaction.tenant_id
  AND execution.id = interaction.execution_id
  AND (interaction.turn_id IS NULL OR interaction.provider IS NULL OR interaction.expires_at IS NULL);

UPDATE execution_interactions
SET resolution_kind = CASE
      WHEN kind = 'approval' AND lower(resolution ->> 'decision') = 'accept' THEN 'approved'
      WHEN kind = 'approval' THEN 'denied'
      ELSE 'answered'
    END,
    resolution_command_id = request_id || ':resolution',
    delivery_status = 'pending',
    delivery_worker_id = worker_id,
    delivery_generation = generation,
    delivery_available_at = resolved_at
WHERE status = 'resolved';

ALTER TABLE execution_interactions
  ALTER COLUMN turn_id SET NOT NULL,
  ALTER COLUMN provider SET NOT NULL,
  ALTER COLUMN expires_at SET NOT NULL,
  ADD CONSTRAINT fk_execution_interactions_turn
    FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE CASCADE,
  ADD CONSTRAINT chk_execution_interactions_provider
    CHECK (length(provider) BETWEEN 1 AND 80),
  ADD CONSTRAINT chk_execution_interactions_expiry
    CHECK (expires_at > requested_at),
  ADD CONSTRAINT chk_execution_interactions_resolution_kind
    CHECK (resolution_kind IS NULL OR resolution_kind IN ('approved', 'denied', 'answered')),
  ADD CONSTRAINT chk_execution_interactions_resolution_command
    CHECK (resolution_command_id IS NULL OR length(resolution_command_id) BETWEEN 1 AND 240),
  ADD CONSTRAINT chk_execution_interactions_delivery_status
    CHECK (delivery_status IN ('not-ready', 'pending', 'delivered', 'acknowledged', 'failed', 'superseded')),
  ADD CONSTRAINT chk_execution_interactions_delivery_generation
    CHECK (delivery_generation IS NULL OR delivery_generation > 0),
  ADD CONSTRAINT chk_execution_interactions_delivery_attempts
    CHECK (delivery_attempts >= 0),
  ADD CONSTRAINT chk_execution_interactions_delivery_error
    CHECK (delivery_error IS NULL OR length(delivery_error) <= 2000),
  ADD CONSTRAINT chk_execution_interactions_delivery_state
    CHECK (
      (status = 'pending' AND resolution_kind IS NULL AND resolution_command_id IS NULL
        AND delivery_status = 'not-ready' AND delivery_worker_id IS NULL AND delivery_generation IS NULL
        AND delivery_available_at IS NULL AND delivered_at IS NULL AND acknowledged_at IS NULL) OR
      (status = 'resolved' AND resolution_kind IS NOT NULL AND resolution_command_id IS NOT NULL
        AND delivery_status <> 'not-ready' AND delivery_worker_id IS NOT NULL AND delivery_generation IS NOT NULL
        AND delivery_available_at IS NOT NULL) OR
      (status = 'expired' AND resolution_kind IS NULL AND delivery_status IN ('not-ready', 'superseded'))
    ),
  ADD CONSTRAINT chk_execution_interactions_delivered_at
    CHECK (delivery_status NOT IN ('delivered', 'acknowledged') OR delivered_at IS NOT NULL),
  ADD CONSTRAINT chk_execution_interactions_acknowledged_at
    CHECK (delivery_status <> 'acknowledged' OR acknowledged_at IS NOT NULL);

CREATE UNIQUE INDEX uq_execution_interactions_resolution_command
  ON execution_interactions (tenant_id, execution_id, resolution_command_id)
  WHERE resolution_command_id IS NOT NULL;

CREATE INDEX idx_execution_interactions_delivery
  ON execution_interactions (
    delivery_worker_id, delivery_generation, delivery_status, delivery_available_at, id
  )
  WHERE delivery_status IN ('pending', 'delivered', 'failed');

CREATE INDEX idx_execution_interactions_expiry
  ON execution_interactions (expires_at, tenant_id, execution_id, id)
  WHERE status = 'pending';

COMMENT ON TABLE execution_interactions IS
  'Persisted Provider approval/user-input request, user resolution, Worker delivery and acknowledgement state.';

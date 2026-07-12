ALTER TABLE agent_turns
  DROP CONSTRAINT IF EXISTS agent_turns_status_check;

ALTER TABLE agent_turns
  ADD CONSTRAINT agent_turns_status_check
  CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled', 'interrupted'));

ALTER TABLE agent_executions
  DROP CONSTRAINT IF EXISTS agent_executions_status_check,
  DROP CONSTRAINT IF EXISTS agent_executions_check;

ALTER TABLE agent_executions
  ADD CONSTRAINT agent_executions_status_check
    CHECK (status IN (
      'queued', 'leased', 'running', 'waiting-for-approval', 'recovering',
      'completed', 'failed', 'cancelled', 'interrupted'
    )),
  ADD CONSTRAINT agent_executions_terminal_time_check
    CHECK (
      (status IN ('completed', 'failed', 'cancelled', 'interrupted') AND finished_at IS NOT NULL) OR
      status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
    );

CREATE OR REPLACE FUNCTION assert_terminal_execution_has_no_lease()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status IN ('completed', 'failed', 'cancelled', 'interrupted') AND EXISTS (
    SELECT 1 FROM worker_leases lease WHERE lease.execution_id = NEW.id
  ) THEN
    RAISE EXCEPTION 'terminal execution cannot retain a worker lease'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TABLE execution_control_commands (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  provider TEXT NOT NULL,
  command_type TEXT NOT NULL,
  command_id TEXT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'pending',
  requested_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivery_worker_id UUID REFERENCES worker_instances(id) ON DELETE RESTRICT,
  delivery_generation BIGINT,
  delivery_attempts INTEGER NOT NULL DEFAULT 0,
  delivery_available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at TIMESTAMPTZ,
  acknowledged_at TIMESTAMPTZ,
  delivery_error TEXT,
  UNIQUE (tenant_id, execution_id, command_id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE CASCADE,
  CHECK (length(provider) BETWEEN 1 AND 80),
  CHECK (command_type IN (
    'SteerTurn', 'InterruptTurn', 'CompactSession', 'RollbackSession',
    'ForkSession', 'StartReview', 'StopSession'
  )),
  CHECK (length(command_id) BETWEEN 1 AND 240),
  CHECK (status IN ('pending', 'delivered', 'acknowledged', 'superseded')),
  CHECK (delivery_generation IS NULL OR delivery_generation > 0),
  CHECK (delivery_attempts >= 0),
  CHECK (delivery_error IS NULL OR length(delivery_error) <= 2000),
  CHECK (
    (delivery_worker_id IS NULL AND delivery_generation IS NULL) OR
    (delivery_worker_id IS NOT NULL AND delivery_generation IS NOT NULL)
  ),
  CHECK (status NOT IN ('delivered', 'acknowledged') OR delivered_at IS NOT NULL),
  CHECK (status <> 'acknowledged' OR acknowledged_at IS NOT NULL)
);

CREATE INDEX idx_execution_control_commands_delivery
  ON execution_control_commands (
    delivery_worker_id, delivery_generation, status, delivery_available_at, id
  )
  WHERE status IN ('pending', 'delivered');

CREATE UNIQUE INDEX uq_execution_control_commands_active_interrupt
  ON execution_control_commands (tenant_id, execution_id, command_type)
  WHERE command_type = 'InterruptTurn' AND status IN ('pending', 'delivered');

COMMENT ON TABLE execution_control_commands IS
  'Durable Generation-fenced Provider Host control commands such as Interrupt, Steer, Compact and Review.';

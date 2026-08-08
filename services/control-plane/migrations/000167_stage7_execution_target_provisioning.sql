CREATE TABLE execution_target_provisioning_operations (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  actor_type TEXT NOT NULL,
  actor_id UUID NOT NULL,
  action TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'accepted',
  attempt_generation BIGINT NOT NULL DEFAULT 0,
  claim_holder TEXT,
  claim_expires_at TIMESTAMPTZ,
  result JSONB NOT NULL DEFAULT '{}'::jsonb,
  error_code TEXT,
  error_message TEXT,
  request_id TEXT NOT NULL,
  ip_address TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  CONSTRAINT uq_execution_target_provisioning_idempotency
    UNIQUE (tenant_id, actor_type, actor_id, idempotency_key),
  CONSTRAINT chk_execution_target_provisioning_actor CHECK (actor_type IN ('user', 'service_account')),
  CONSTRAINT chk_execution_target_provisioning_action CHECK (action IN ('install', 'upgrade', 'revoke')),
  CONSTRAINT chk_execution_target_provisioning_state CHECK (state IN ('accepted', 'running', 'succeeded', 'failed')),
  CONSTRAINT chk_execution_target_provisioning_attempt CHECK (attempt_generation >= 0),
  CONSTRAINT chk_execution_target_provisioning_key CHECK (char_length(idempotency_key) BETWEEN 1 AND 200),
  CONSTRAINT chk_execution_target_provisioning_hash CHECK (char_length(request_hash) = 64),
  CONSTRAINT chk_execution_target_provisioning_claim CHECK (
    (state = 'accepted' AND claim_holder IS NULL AND claim_expires_at IS NULL AND completed_at IS NULL) OR
    (state = 'running' AND claim_holder IS NOT NULL AND claim_expires_at IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NULL) OR
    (state IN ('succeeded', 'failed') AND claim_holder IS NULL AND claim_expires_at IS NULL AND completed_at IS NOT NULL)
  ),
  CONSTRAINT chk_execution_target_provisioning_error CHECK (
    (state = 'failed' AND error_code IS NOT NULL AND error_message IS NOT NULL) OR
    (state <> 'failed' AND error_code IS NULL AND error_message IS NULL)
  )
);

CREATE UNIQUE INDEX uq_execution_target_provisioning_active_target
  ON execution_target_provisioning_operations (execution_target_id)
  WHERE state IN ('accepted', 'running');

CREATE INDEX idx_execution_target_provisioning_claim
  ON execution_target_provisioning_operations (state, claim_expires_at, created_at, id)
  WHERE state IN ('accepted', 'running');

CREATE INDEX idx_execution_target_provisioning_tenant_target
  ON execution_target_provisioning_operations (tenant_id, execution_target_id, created_at DESC, id DESC);

ALTER TABLE execution_targets
  ADD COLUMN ssh_provisioning_operation_id UUID
    REFERENCES execution_target_provisioning_operations(id) ON DELETE RESTRICT;

CREATE INDEX idx_execution_targets_ssh_provisioning_operation
  ON execution_targets (ssh_provisioning_operation_id)
  WHERE ssh_provisioning_operation_id IS NOT NULL;

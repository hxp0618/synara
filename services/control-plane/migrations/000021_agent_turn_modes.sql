ALTER TABLE agent_turns
  ADD COLUMN runtime_mode TEXT NOT NULL DEFAULT 'full-access'
    CHECK (runtime_mode IN ('approval-required', 'full-access')),
  ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default'
    CHECK (interaction_mode IN ('default', 'plan'));

COMMENT ON COLUMN agent_turns.runtime_mode IS
  'Immutable permission mode captured when the Turn is created: approval-required or full-access.';
COMMENT ON COLUMN agent_turns.interaction_mode IS
  'Immutable Provider interaction mode captured when the Turn is created: default or plan.';

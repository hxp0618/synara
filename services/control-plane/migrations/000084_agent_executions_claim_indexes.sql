-- Claim fair-share ordering computes, per queued candidate, a correlated
-- COUNT of the Tenant's active service units on the same Execution Target
-- (see executions/claim_fair_queue.go). Without this index the subquery
-- scans agent_executions per candidate row on every worker claim poll.
CREATE INDEX IF NOT EXISTS idx_agent_executions_fair_share_active
  ON agent_executions (execution_target_id, target_kind, tenant_id)
  WHERE status IN ('leased', 'running', 'waiting-for-approval');

-- The Kubernetes reconciler loads all non-terminal executions for a target
-- every cycle with status IN ('queued','recovering','leased','running',
-- 'waiting-for-approval'). The existing claimable index only covers
-- ('queued','recovering'), so the active statuses fall back to a scan.
CREATE INDEX IF NOT EXISTS idx_agent_executions_target_nonterminal
  ON agent_executions (execution_target_id, target_kind, status, queued_at, id)
  WHERE status IN ('queued', 'recovering', 'leased', 'running', 'waiting-for-approval');

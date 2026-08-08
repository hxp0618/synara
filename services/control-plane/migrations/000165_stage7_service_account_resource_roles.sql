ALTER TABLE service_accounts
  ADD COLUMN role TEXT NOT NULL DEFAULT 'member';

ALTER TABLE service_accounts
  ADD CONSTRAINT chk_service_accounts_resource_role
  CHECK (
    (organization_id IS NULL AND role IN (
      'owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member'
    )) OR
    (organization_id IS NOT NULL AND role IN (
      'owner', 'admin', 'agent_operator', 'member', 'viewer'
    ))
  );

COMMENT ON COLUMN service_accounts.role IS
  'Fixed tenant or organization RBAC role used by Stage 7 resource API authentication.';

ALTER TABLE service_accounts
  ADD COLUMN rate_limit_per_minute INTEGER NOT NULL DEFAULT 600;

ALTER TABLE service_accounts
  ADD CONSTRAINT chk_service_accounts_rate_limit_per_minute
  CHECK (rate_limit_per_minute BETWEEN 1 AND 60000);

COMMENT ON COLUMN service_accounts.rate_limit_per_minute IS
  'Persistent per-key developer API admission limit for each UTC minute window.';

CREATE TABLE service_account_api_usage_windows (
  tenant_id UUID NOT NULL,
  service_account_id UUID NOT NULL,
  window_started_at TIMESTAMPTZ NOT NULL,
  route_pattern TEXT NOT NULL,
  organization_id UUID,
  admitted_count BIGINT NOT NULL DEFAULT 0,
  rate_limited_count BIGINT NOT NULL DEFAULT 0,
  request_count BIGINT NOT NULL DEFAULT 0,
  success_count BIGINT NOT NULL DEFAULT 0,
  client_error_count BIGINT NOT NULL DEFAULT 0,
  server_error_count BIGINT NOT NULL DEFAULT 0,
  total_duration_ms BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (tenant_id, service_account_id, window_started_at, route_pattern),
  CONSTRAINT fk_service_account_api_usage_account
    FOREIGN KEY (tenant_id, service_account_id)
    REFERENCES service_accounts(tenant_id, id)
    ON DELETE CASCADE,
  CONSTRAINT fk_service_account_api_usage_organization
    FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id),
  CONSTRAINT chk_service_account_api_usage_route
    CHECK (char_length(route_pattern) BETWEEN 1 AND 500),
  CONSTRAINT chk_service_account_api_usage_counts
    CHECK (
      admitted_count >= 0 AND rate_limited_count >= 0 AND request_count >= 0 AND
      success_count >= 0 AND client_error_count >= 0 AND server_error_count >= 0 AND
      total_duration_ms >= 0
    )
);

CREATE INDEX idx_service_account_api_usage_key_window
  ON service_account_api_usage_windows (tenant_id, service_account_id, window_started_at DESC);

CREATE INDEX idx_service_account_api_usage_org_window
  ON service_account_api_usage_windows (tenant_id, organization_id, window_started_at DESC)
  WHERE organization_id IS NOT NULL;

COMMENT ON TABLE service_account_api_usage_windows IS
  'Stage 7 persistent per-key developer API admission and outcome attribution; route_pattern=* stores the key-wide window.';

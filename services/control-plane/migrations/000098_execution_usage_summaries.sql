CREATE TABLE execution_usage_summaries (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  provider TEXT NOT NULL,
  model TEXT,
  input_tokens BIGINT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  cached_input_tokens BIGINT NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
  output_tokens BIGINT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  reasoning_tokens BIGINT NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
  total_tokens BIGINT NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
  network_ingress_bytes BIGINT NOT NULL DEFAULT 0 CHECK (network_ingress_bytes >= 0),
  network_egress_bytes BIGINT NOT NULL DEFAULT 0 CHECK (network_egress_bytes >= 0),
  duration_millis BIGINT NOT NULL DEFAULT 0 CHECK (duration_millis >= 0),
  provider_cost_micros BIGINT NOT NULL DEFAULT 0 CHECK (provider_cost_micros >= 0),
  currency_code TEXT NOT NULL DEFAULT 'USD' CHECK (currency_code ~ '^[A-Z]{3}$'),
  final BOOLEAN NOT NULL DEFAULT false,
  latest_event_sequence BIGINT NOT NULL DEFAULT 0 CHECK (latest_event_sequence >= 0),
  network_report_sequence BIGINT NOT NULL DEFAULT 0 CHECK (network_report_sequence >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, execution_id, generation),
  FOREIGN KEY (tenant_id, execution_id) REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, session_id) REFERENCES agent_sessions(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, turn_id) REFERENCES agent_turns(tenant_id, id) ON DELETE CASCADE,
  CHECK (length(provider) BETWEEN 1 AND 80),
  CHECK (model IS NULL OR length(model) BETWEEN 1 AND 200),
  CHECK (total_tokens >= input_tokens + output_tokens)
);

CREATE INDEX idx_execution_usage_summaries_session
  ON execution_usage_summaries (tenant_id, session_id, updated_at DESC, execution_id, generation);

CREATE INDEX idx_execution_usage_summaries_turn
  ON execution_usage_summaries (tenant_id, turn_id, generation DESC, execution_id);

CREATE TRIGGER trg_execution_usage_summaries_updated_at
BEFORE UPDATE ON execution_usage_summaries
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE outbox_messages
  ADD COLUMN claimed_by TEXT,
  ADD COLUMN claimed_at TIMESTAMPTZ,
  ADD COLUMN claim_expires_at TIMESTAMPTZ,
  ADD COLUMN dead_lettered_at TIMESTAMPTZ;

ALTER TABLE outbox_messages
  ADD CONSTRAINT chk_outbox_claim_fields
    CHECK (
      (claimed_by IS NULL AND claimed_at IS NULL AND claim_expires_at IS NULL) OR
      (claimed_by IS NOT NULL AND claimed_at IS NOT NULL AND claim_expires_at IS NOT NULL)
    ),
  ADD CONSTRAINT chk_outbox_claim_expiry
    CHECK (claim_expires_at IS NULL OR claim_expires_at > claimed_at),
  ADD CONSTRAINT chk_outbox_terminal_state
    CHECK (published_at IS NULL OR dead_lettered_at IS NULL),
  ADD CONSTRAINT chk_outbox_claimed_by_length
    CHECK (claimed_by IS NULL OR length(claimed_by) BETWEEN 1 AND 160),
  ADD CONSTRAINT chk_outbox_last_error_length
    CHECK (last_error IS NULL OR length(last_error) <= 512);

DROP INDEX IF EXISTS idx_outbox_messages_pending;

CREATE INDEX idx_outbox_messages_claimable
  ON outbox_messages (available_at, created_at, id)
  WHERE published_at IS NULL AND dead_lettered_at IS NULL;

CREATE INDEX idx_outbox_messages_expired_claim
  ON outbox_messages (claim_expires_at, created_at, id)
  WHERE published_at IS NULL
    AND dead_lettered_at IS NULL
    AND claim_expires_at IS NOT NULL;

CREATE INDEX idx_outbox_messages_dead_letter
  ON outbox_messages (dead_lettered_at DESC, created_at, id)
  WHERE dead_lettered_at IS NOT NULL;

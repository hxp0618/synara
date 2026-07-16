ALTER TABLE artifacts
  DROP CONSTRAINT IF EXISTS artifacts_kind_check;

ALTER TABLE artifacts
  ADD CONSTRAINT artifacts_kind_check
  CHECK (kind IN ('attachment', 'generated_file', 'terminal_log', 'diff', 'workspace_snapshot', 'checkpoint'));

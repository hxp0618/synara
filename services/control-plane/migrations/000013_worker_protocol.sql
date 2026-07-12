ALTER TABLE worker_instances
  ADD COLUMN IF NOT EXISTS protocol_version INTEGER NOT NULL DEFAULT 1;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'worker_instances_protocol_version_check'
      AND conrelid = 'worker_instances'::regclass
  ) THEN
    ALTER TABLE worker_instances
      ADD CONSTRAINT worker_instances_protocol_version_check CHECK (protocol_version > 0);
  END IF;
END $$;

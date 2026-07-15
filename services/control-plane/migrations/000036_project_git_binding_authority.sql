-- Retire projects.git_credential_id as a writable authority. Credential Bindings are the
-- only source of truth; the legacy column remains nullable for rolling compatibility only.

-- A post-000035 deployment may still have written the legacy column. Re-run the deterministic
-- backfill before clearing it. The validation trigger is temporarily removed so historical
-- bindings remain recoverable even when the referenced Credential was revoked after binding.
DROP TRIGGER IF EXISTS trg_credential_bindings_validate ON credential_bindings;

INSERT INTO credential_bindings (
  id, tenant_id, organization_id, project_id, credential_id,
  binding_kind, selector_value, created_by, created_at
)
SELECT
  (
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:000036:' || project.git_credential_id::text), 1, 8) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:000036:' || project.git_credential_id::text), 9, 4) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:000036:' || project.git_credential_id::text), 13, 4) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:000036:' || project.git_credential_id::text), 17, 4) || '-' ||
    substr(md5(project.tenant_id::text || ':' || project.id::text || ':git_fetch:000036:' || project.git_credential_id::text), 21, 12)
  )::uuid,
  project.tenant_id,
  project.organization_id,
  project.id,
  project.git_credential_id,
  'git_fetch',
  project.repository_url,
  project.created_by,
  project.updated_at
FROM projects AS project
WHERE project.git_credential_id IS NOT NULL
  AND project.repository_url IS NOT NULL
  AND NOT EXISTS (
    SELECT 1
    FROM credential_bindings AS binding
    WHERE binding.tenant_id = project.tenant_id
      AND binding.project_id = project.id
      AND binding.credential_id = project.git_credential_id
      AND binding.binding_kind = 'git_fetch'
      AND binding.selector_value = project.repository_url
      AND binding.disabled_at IS NULL
  )
ON CONFLICT DO NOTHING;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM projects AS project
    WHERE project.git_credential_id IS NOT NULL
      AND (
        project.repository_url IS NULL OR
        NOT EXISTS (
          SELECT 1
          FROM credential_bindings AS binding
          WHERE binding.tenant_id = project.tenant_id
            AND binding.project_id = project.id
            AND binding.credential_id = project.git_credential_id
            AND binding.binding_kind = 'git_fetch'
            AND binding.selector_value = project.repository_url
            AND binding.disabled_at IS NULL
        )
      )
  ) THEN
    RAISE EXCEPTION 'legacy Project Git Credential could not be preserved as an active git_fetch Binding'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

UPDATE projects
SET git_credential_id = NULL
WHERE git_credential_id IS NOT NULL;

CREATE TRIGGER trg_credential_bindings_validate
BEFORE INSERT ON credential_bindings
FOR EACH ROW EXECUTE FUNCTION enforce_credential_binding();

DROP TRIGGER IF EXISTS trg_projects_git_credential_binding ON projects;
DROP FUNCTION IF EXISTS enforce_project_git_credential_binding();
ALTER TABLE projects DROP CONSTRAINT IF EXISTS fk_projects_git_credential;
DROP INDEX IF EXISTS idx_projects_git_credential;

CREATE OR REPLACE FUNCTION reject_legacy_project_git_credential_write()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.git_credential_id IS NOT NULL THEN
    RAISE EXCEPTION 'projects.git_credential_id is retired; use credential_bindings'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_projects_legacy_git_credential_insert ON projects;
CREATE TRIGGER trg_projects_legacy_git_credential_insert
BEFORE INSERT ON projects
FOR EACH ROW EXECUTE FUNCTION reject_legacy_project_git_credential_write();

DROP TRIGGER IF EXISTS trg_projects_legacy_git_credential_update ON projects;
CREATE TRIGGER trg_projects_legacy_git_credential_update
BEFORE UPDATE OF git_credential_id ON projects
FOR EACH ROW EXECUTE FUNCTION reject_legacy_project_git_credential_write();

CREATE OR REPLACE FUNCTION enforce_project_repository_git_binding_selector()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.repository_url IS DISTINCT FROM OLD.repository_url AND EXISTS (
    SELECT 1
    FROM credential_bindings AS binding
    WHERE binding.tenant_id = NEW.tenant_id
      AND binding.project_id = NEW.id
      AND binding.binding_kind IN ('git_fetch', 'git_push')
      AND binding.disabled_at IS NULL
      AND binding.selector_value IS DISTINCT FROM NEW.repository_url
  ) THEN
    RAISE EXCEPTION 'active Project Git Credential Bindings must be disabled before changing repository_url'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_projects_repository_git_binding_selector ON projects;
CREATE TRIGGER trg_projects_repository_git_binding_selector
BEFORE UPDATE OF repository_url ON projects
FOR EACH ROW EXECUTE FUNCTION enforce_project_repository_git_binding_selector();

CREATE OR REPLACE FUNCTION enforce_credential_binding_git_selector()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  project_repository_url TEXT;
BEGIN
  IF NEW.project_id IS NULL OR NEW.binding_kind NOT IN ('git_fetch', 'git_push') THEN
    RETURN NEW;
  END IF;

  SELECT project.repository_url INTO project_repository_url
  FROM projects AS project
  WHERE project.tenant_id = NEW.tenant_id
    AND project.id = NEW.project_id
  FOR SHARE;

  IF NOT FOUND OR project_repository_url IS NULL OR
     project_repository_url IS DISTINCT FROM NEW.selector_value THEN
    RAISE EXCEPTION 'Project Git Credential Binding selector must exactly match repository_url'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_credential_bindings_git_selector ON credential_bindings;
CREATE TRIGGER trg_credential_bindings_git_selector
BEFORE INSERT ON credential_bindings
FOR EACH ROW EXECUTE FUNCTION enforce_credential_binding_git_selector();

COMMENT ON COLUMN projects.git_credential_id IS
  'Retired compatibility column. Must remain NULL; active credential_bindings are authoritative.';

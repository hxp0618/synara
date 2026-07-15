package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateProjectGitBindingAuthoritySQLiteSafety(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var unresolved int64
		if err := tx.Raw(`
			SELECT count(*)
			FROM projects AS project
			WHERE project.git_credential_id IS NOT NULL
			  AND (
			    project.repository_url IS NULL
			    OR NOT EXISTS (
			      SELECT 1
			      FROM credential_bindings AS binding
			      WHERE binding.tenant_id = project.tenant_id
			        AND binding.project_id = project.id
			        AND binding.credential_id = project.git_credential_id
			        AND binding.binding_kind = 'git_fetch'
			        AND binding.selector_value = project.repository_url
			        AND binding.disabled_at IS NULL
			    )
			  )`).Scan(&unresolved).Error; err != nil {
			return fmt.Errorf("inspect legacy Project Git Credential backfill: %w", err)
		}
		if unresolved != 0 {
			return fmt.Errorf("retire legacy Project Git Credential authority: %d Project bindings could not be preserved", unresolved)
		}

		statements := []string{
			`UPDATE projects SET git_credential_id = NULL WHERE git_credential_id IS NOT NULL`,
			`DROP INDEX IF EXISTS idx_projects_git_credential`,
			`DROP TRIGGER IF EXISTS trg_projects_legacy_git_credential_insert`,
			`CREATE TRIGGER trg_projects_legacy_git_credential_insert
			 BEFORE INSERT ON projects
			 WHEN NEW.git_credential_id IS NOT NULL
			 BEGIN
			   SELECT RAISE(ABORT, 'projects.git_credential_id is retired; use credential_bindings');
			 END`,
			`DROP TRIGGER IF EXISTS trg_projects_legacy_git_credential_update`,
			`CREATE TRIGGER trg_projects_legacy_git_credential_update
			 BEFORE UPDATE OF git_credential_id ON projects
			 WHEN NEW.git_credential_id IS NOT NULL
			 BEGIN
			   SELECT RAISE(ABORT, 'projects.git_credential_id is retired; use credential_bindings');
			 END`,
			`DROP TRIGGER IF EXISTS trg_projects_repository_git_binding_selector`,
			`CREATE TRIGGER trg_projects_repository_git_binding_selector
			 BEFORE UPDATE OF repository_url ON projects
			 WHEN NEW.repository_url IS NOT OLD.repository_url
			   AND EXISTS (
			     SELECT 1
			     FROM credential_bindings AS binding
			     WHERE binding.tenant_id = NEW.tenant_id
			       AND binding.project_id = NEW.id
			       AND binding.binding_kind IN ('git_fetch', 'git_push')
			       AND binding.disabled_at IS NULL
			       AND binding.selector_value IS NOT NEW.repository_url
			   )
			 BEGIN
			   SELECT RAISE(ABORT, 'active Project Git Credential Bindings must be disabled before changing repository_url');
			 END`,
			`DROP TRIGGER IF EXISTS trg_credential_bindings_git_selector_insert`,
			`CREATE TRIGGER trg_credential_bindings_git_selector_insert
			 BEFORE INSERT ON credential_bindings
			 WHEN NEW.project_id IS NOT NULL
			   AND NEW.binding_kind IN ('git_fetch', 'git_push')
			   AND NOT EXISTS (
			     SELECT 1
			     FROM projects AS project
			     WHERE project.tenant_id = NEW.tenant_id
			       AND project.id = NEW.project_id
			       AND project.repository_url IS NOT NULL
			       AND project.repository_url IS NEW.selector_value
			   )
			 BEGIN
			   SELECT RAISE(ABORT, 'Project Git Credential Binding selector must exactly match repository_url');
			 END`,
		}
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("apply SQLite Project Git Binding authority migration: %w", err)
			}
		}
		return nil
	})
}

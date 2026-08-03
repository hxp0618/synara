package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateProjectCostAllocationSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_project_cost_allocations_insert`,
		`CREATE TRIGGER trg_project_cost_allocations_insert BEFORE INSERT ON project_cost_allocations BEGIN
		  SELECT RAISE(ABORT, 'invalid Project internal cost allocation') WHERE
		    NEW.version <> 1 OR length(NEW.cost_center_code) NOT BETWEEN 1 AND 64
		    OR NEW.cost_center_code GLOB '*[^A-Za-z0-9._/-]*'
		    OR substr(NEW.cost_center_code, 1, 1) GLOB '[^A-Za-z0-9]'
		    OR length(NEW.department_code) NOT BETWEEN 1 AND 64
		    OR NEW.department_code GLOB '*[^A-Za-z0-9._/-]*'
		    OR substr(NEW.department_code, 1, 1) GLOB '[^A-Za-z0-9]';
		END`,
		`DROP TRIGGER IF EXISTS trg_project_cost_allocations_update`,
		`CREATE TRIGGER trg_project_cost_allocations_update BEFORE UPDATE ON project_cost_allocations BEGIN
		  SELECT RAISE(ABORT, 'invalid Project internal cost allocation update') WHERE
		    NEW.tenant_id IS NOT OLD.tenant_id OR NEW.project_id IS NOT OLD.project_id
		    OR NEW.created_at IS NOT OLD.created_at OR NEW.version <> OLD.version + 1
		    OR julianday(NEW.updated_at) <= julianday(OLD.updated_at)
		    OR length(NEW.cost_center_code) NOT BETWEEN 1 AND 64
		    OR NEW.cost_center_code GLOB '*[^A-Za-z0-9._/-]*'
		    OR substr(NEW.cost_center_code, 1, 1) GLOB '[^A-Za-z0-9]'
		    OR length(NEW.department_code) NOT BETWEEN 1 AND 64
		    OR NEW.department_code GLOB '*[^A-Za-z0-9._/-]*'
		    OR substr(NEW.department_code, 1, 1) GLOB '[^A-Za-z0-9]';
		END`,
		`DROP TRIGGER IF EXISTS trg_project_cost_allocations_no_delete`,
		`CREATE TRIGGER trg_project_cost_allocations_no_delete BEFORE DELETE ON project_cost_allocations
		 BEGIN SELECT RAISE(ABORT, 'Project internal cost allocations cannot be deleted'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply Project cost allocation SQLite safety: %w", err)
		}
	}
	return nil
}

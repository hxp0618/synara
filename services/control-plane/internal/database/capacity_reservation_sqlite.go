package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateCapacityReservationSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_target_reservation_acknowledgements_health
		 ON execution_target_reservation_acknowledgements
		 (execution_target_id, health_version, execution_id)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_capacity_admissions_target_time
		 ON execution_capacity_admissions (execution_target_id, admitted_at DESC, execution_id)`,
		`DROP TRIGGER IF EXISTS trg_target_reservation_acknowledgements_insert`,
		`CREATE TRIGGER trg_target_reservation_acknowledgements_insert
		 BEFORE INSERT ON execution_target_reservation_acknowledgements
		 BEGIN
		   SELECT RAISE(ABORT, 'Reservation acknowledgement is not staged for the next Health version')
		   WHERE NEW.execution_generation < 0 OR NEW.health_version <= 0
		      OR NEW.health_version IS NOT COALESCE((
		        SELECT health.version + 1 FROM execution_target_health AS health
		        WHERE health.execution_target_id = NEW.execution_target_id
		      ), 1);
		   SELECT RAISE(ABORT, 'Reservation acknowledgement belongs to another Execution Target')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.id = NEW.execution_id
		       AND execution.execution_target_id = NEW.execution_target_id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_target_reservation_acknowledgements_update`,
		`CREATE TRIGGER trg_target_reservation_acknowledgements_update
		 BEFORE UPDATE ON execution_target_reservation_acknowledgements
		 BEGIN SELECT RAISE(ABORT, 'Reservation acknowledgements cannot be updated in place'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_health_reservation_insert`,
		`CREATE TRIGGER trg_execution_target_health_reservation_insert
		 BEFORE INSERT ON execution_target_health
		 BEGIN
		   SELECT RAISE(ABORT, 'Health reservation authority shape is invalid')
		   WHERE NOT (
		     (NEW.reservation_authority_mode IS NULL
		       AND NEW.reservation_acknowledged_units = 0
		       AND NEW.reservation_acknowledgements_sha256 IS NULL)
		     OR
		     (NEW.reservation_authority_mode = 'exact-active-v1'
		       AND NEW.reservation_acknowledged_units >= 0
		       AND NEW.reservation_acknowledged_units <= NEW.allocated_capacity_units
		       AND length(NEW.reservation_acknowledgements_sha256) = 64
		       AND NEW.reservation_acknowledgements_sha256 NOT GLOB '*[^0-9a-f]*')
		   );
		   SELECT RAISE(ABORT, 'Health reservation acknowledgements do not match the staged authority')
		   WHERE NEW.reservation_acknowledged_units IS NOT (
		     SELECT count(*) FROM execution_target_reservation_acknowledgements AS acknowledgement
		     WHERE acknowledgement.execution_target_id = NEW.execution_target_id
		       AND acknowledgement.health_version = NEW.version
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_health_reservation_update`,
		`CREATE TRIGGER trg_execution_target_health_reservation_update
		 BEFORE UPDATE ON execution_target_health
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Target reservation authority cannot be removed')
		   WHERE OLD.reservation_authority_mode IS NOT NULL
		     AND NEW.reservation_authority_mode IS NULL;
		   SELECT RAISE(ABORT, 'Health reservation authority shape is invalid')
		   WHERE NOT (
		     (NEW.reservation_authority_mode IS NULL
		       AND NEW.reservation_acknowledged_units = 0
		       AND NEW.reservation_acknowledgements_sha256 IS NULL)
		     OR
		     (NEW.reservation_authority_mode = 'exact-active-v1'
		       AND NEW.reservation_acknowledged_units >= 0
		       AND NEW.reservation_acknowledged_units <= NEW.allocated_capacity_units
		       AND length(NEW.reservation_acknowledgements_sha256) = 64
		       AND NEW.reservation_acknowledgements_sha256 NOT GLOB '*[^0-9a-f]*')
		   );
		   SELECT RAISE(ABORT, 'Health reservation acknowledgements do not match the staged authority')
		   WHERE NEW.reservation_acknowledged_units IS NOT (
		     SELECT count(*) FROM execution_target_reservation_acknowledgements AS acknowledgement
		     WHERE acknowledgement.execution_target_id = NEW.execution_target_id
		       AND acknowledgement.health_version = NEW.version
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_capacity_admissions_insert`,
		`CREATE TRIGGER trg_execution_capacity_admissions_insert
		 BEFORE INSERT ON execution_capacity_admissions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution capacity admission scope is invalid')
		   WHERE NEW.active_reservation_units < 0
		      OR length(NEW.snapshot_sha256) <> 64
		      OR NEW.snapshot_sha256 GLOB '*[^0-9a-f]*'
		      OR NOT EXISTS (
		        SELECT 1 FROM agent_executions AS execution
		        WHERE execution.tenant_id = NEW.tenant_id
		          AND execution.id = NEW.execution_id
		          AND execution.execution_target_id = NEW.execution_target_id
		      );
		   SELECT RAISE(ABORT, 'Execution capacity admission shape is invalid')
		   WHERE NOT (
		     (NEW.admission_mode = 'fixed-unbounded-v1'
		       AND NEW.health_version IS NULL AND NEW.health_source IS NULL
		       AND NEW.health_observed_at IS NULL AND NEW.health_expires_at IS NULL
		       AND NEW.capacity_status IS NULL AND NEW.capacity_ceiling_units IS NULL
		       AND NEW.allocated_capacity_units IS NULL
		       AND NEW.reservation_authority_mode IS NULL
		       AND NEW.reservation_acknowledged_units IS NULL
		       AND NEW.reservation_acknowledgements_sha256 IS NULL
		       AND NEW.unacknowledged_reservation_units IS NULL
		       AND NEW.strict_capacity_used_units IS NULL)
		     OR
		     (NEW.admission_mode = 'publisher-health-v1'
		       AND NEW.health_version > 0 AND length(trim(NEW.health_source)) BETWEEN 1 AND 160
		       AND NEW.health_observed_at IS NOT NULL AND NEW.health_expires_at > NEW.health_observed_at
		       AND NEW.capacity_status IN ('available', 'saturated', 'unknown')
		       AND NEW.allocated_capacity_units >= 0
		       AND (NEW.capacity_ceiling_units IS NULL OR NEW.capacity_ceiling_units >= 0)
		       AND NEW.reservation_authority_mode IS NULL
		       AND NEW.reservation_acknowledged_units IS NULL
		       AND NEW.reservation_acknowledgements_sha256 IS NULL
		       AND NEW.unacknowledged_reservation_units IS NULL
		       AND NEW.strict_capacity_used_units IS NULL)
		     OR
		     (NEW.admission_mode = 'exact-active-v1'
		       AND NEW.health_version > 0 AND length(trim(NEW.health_source)) BETWEEN 1 AND 160
		       AND NEW.health_observed_at IS NOT NULL AND NEW.health_expires_at > NEW.health_observed_at
		       AND NEW.capacity_status IN ('available', 'saturated', 'unknown')
		       AND NEW.allocated_capacity_units >= 0
		       AND (NEW.capacity_ceiling_units IS NULL OR NEW.capacity_ceiling_units >= 0)
		       AND NEW.reservation_authority_mode = 'exact-active-v1'
		       AND NEW.reservation_acknowledged_units >= 0
		       AND length(NEW.reservation_acknowledgements_sha256) = 64
		       AND NEW.reservation_acknowledgements_sha256 NOT GLOB '*[^0-9a-f]*'
		       AND NEW.unacknowledged_reservation_units >= 0
		       AND NEW.strict_capacity_used_units = NEW.allocated_capacity_units + NEW.unacknowledged_reservation_units)
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_capacity_admissions_update`,
		`CREATE TRIGGER trg_execution_capacity_admissions_update
		 BEFORE UPDATE ON execution_capacity_admissions
		 BEGIN SELECT RAISE(ABORT, 'Execution capacity admission evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_capacity_admissions_delete`,
		`CREATE TRIGGER trg_execution_capacity_admissions_delete
		 BEFORE DELETE ON execution_capacity_admissions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution capacity admission is retained by its Execution')
		   WHERE EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = OLD.tenant_id AND execution.id = OLD.execution_id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_capacity_admission_delete`,
		`CREATE TRIGGER trg_agent_executions_capacity_admission_delete
		 AFTER DELETE ON agent_executions
		 BEGIN
		   DELETE FROM execution_capacity_admissions
		   WHERE tenant_id = OLD.tenant_id AND execution_id = OLD.id;
		   DELETE FROM execution_target_reservation_acknowledgements
		   WHERE execution_id = OLD.id;
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Execution capacity reservation safety migration: %w", err)
		}
	}
	return nil
}

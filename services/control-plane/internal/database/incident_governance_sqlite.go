package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateIncidentGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_stage6_incident_resolution_approvals_no_update`,
		`DROP TRIGGER IF EXISTS trg_stage6_incidents_update`,
		`UPDATE stage6_incident_resolution_approvals
		 SET superseded_at = COALESCE(superseded_at, CURRENT_TIMESTAMP),
		     superseded_reason = COALESCE(superseded_reason, 'Automatically superseded during Migration 000149 because Incident resolution approval evidence was not byte-bound.')
		 WHERE evidence_sha256 IS NULL AND superseded_at IS NULL`,
		`DROP INDEX IF EXISTS idx_stage6_incident_resolution_approvals_incident_id`,
		`DROP INDEX IF EXISTS uq_stage6_incident_resolution_approval_active`,
		`CREATE UNIQUE INDEX uq_stage6_incident_resolution_approval_active
		 ON stage6_incident_resolution_approvals (incident_id) WHERE superseded_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_incidents_state
		 ON stage6_incidents (state, severity, updated_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_incident_updates_published
		 ON stage6_incident_updates (incident_id, published_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_incident_updates_reference
		 ON stage6_incident_updates (incident_id, external_reference)`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_incident_updates_incident
		 ON stage6_incident_updates (incident_id, published_at, id)`,
		`DROP TRIGGER IF EXISTS trg_stage6_incidents_insert`,
		`CREATE TRIGGER trg_stage6_incidents_insert
		 BEFORE INSERT ON stage6_incidents
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 incident')
		   WHERE length(NEW.incident_key) NOT BETWEEN 3 AND 120
		      OR NEW.severity NOT IN ('SEV-0', 'SEV-1', 'SEV-2', 'SEV-3')
		      OR NEW.state <> 'investigating' OR NEW.version <> 1
		      OR length(trim(NEW.title)) NOT BETWEEN 10 AND 200
		      OR length(trim(NEW.customer_impact_summary)) NOT BETWEEN 20 AND 2000
		      OR unixepoch(NEW.started_at) > unixepoch(NEW.impact_confirmed_at)
		      OR unixepoch(NEW.impact_confirmed_at) > unixepoch(NEW.created_at) + 300
		      OR NEW.resolved_at IS NOT NULL OR NEW.cancelled_at IS NOT NULL
		      OR NEW.incident_commander_user_id = NEW.communications_lead_user_id
		      OR NEW.created_by <> NEW.incident_commander_user_id
		      OR (NEW.severity IN ('SEV-0', 'SEV-1') AND NEW.public_impact <> 1)
		      OR (NEW.security_privacy_impact = 1 AND (
		        NEW.security_privacy_lead_user_id IS NULL
		        OR NEW.security_privacy_lead_user_id = NEW.incident_commander_user_id
		      ))
		      OR (NEW.security_privacy_impact = 0 AND NEW.security_privacy_lead_user_id IS NOT NULL)
		      OR (NEW.public_impact = 1 AND (
		        NEW.status_page_origin IS NULL
		        OR substr(NEW.status_page_origin, 1, 8) <> 'https://'
		        OR instr(substr(NEW.status_page_origin, 9), '/') > 0
		        OR substr(NEW.status_page_origin, 9) GLOB '*[^A-Za-z0-9.:-]*'
		        OR NEW.first_public_update_due_at IS NULL OR NEW.next_public_update_due_at IS NULL
		      ))
		      OR (NEW.public_impact = 0 AND (
		        NEW.status_page_origin IS NOT NULL OR NEW.status_page_incident_reference IS NOT NULL
		        OR NEW.first_public_update_due_at IS NOT NULL OR NEW.next_public_update_due_at IS NOT NULL
		      ))
		      OR json_valid(NEW.affected_components) <> 1
		      OR json_array_length(NEW.affected_components) NOT BETWEEN 1 AND 6
		      OR EXISTS (
		        SELECT 1 FROM json_each(NEW.affected_components) WHERE value NOT IN (
		          'control-plane-api', 'authentication-sso', 'execution-scheduling',
		          'worker-runtime', 'artifact-service', 'web-application'
		        )
		      )
		      OR EXISTS (SELECT value FROM json_each(NEW.affected_components) GROUP BY value HAVING count(*) > 1)
		      OR json_valid(NEW.affected_regions) <> 1
		      OR json_array_length(NEW.affected_regions) NOT BETWEEN 1 AND 32
		      OR EXISTS (SELECT value FROM json_each(NEW.affected_regions) GROUP BY value HAVING count(*) > 1)
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships membership
		        JOIN tenants operator_tenant ON operator_tenant.id = membership.tenant_id
		        JOIN users operator_user ON operator_user.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id
		          AND membership.user_id = NEW.incident_commander_user_id
		          AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
		          AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		          AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
		      )
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships membership
		        JOIN users operator_user ON operator_user.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id
		          AND membership.user_id = NEW.communications_lead_user_id
		          AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
		          AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
		      )
		      OR (NEW.security_privacy_lead_user_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM tenant_memberships membership
		        JOIN users operator_user ON operator_user.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id
		          AND membership.user_id = NEW.security_privacy_lead_user_id
		          AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
		          AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incidents_update`,
		`CREATE TRIGGER trg_stage6_incidents_update
		 BEFORE UPDATE ON stage6_incidents
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 incident update')
		   WHERE NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		      OR NEW.incident_key <> OLD.incident_key OR NEW.severity <> OLD.severity
		      OR NEW.title <> OLD.title OR NEW.customer_impact_summary <> OLD.customer_impact_summary
		      OR NEW.public_impact <> OLD.public_impact OR NEW.security_privacy_impact <> OLD.security_privacy_impact
		      OR NEW.status_page_origin IS NOT OLD.status_page_origin
		      OR NEW.affected_components <> OLD.affected_components OR NEW.affected_regions <> OLD.affected_regions
		      OR NEW.incident_commander_user_id <> OLD.incident_commander_user_id
		      OR NEW.communications_lead_user_id <> OLD.communications_lead_user_id
		      OR NEW.security_privacy_lead_user_id IS NOT OLD.security_privacy_lead_user_id
		      OR NEW.started_at <> OLD.started_at OR NEW.impact_confirmed_at <> OLD.impact_confirmed_at
		      OR NEW.first_public_update_due_at IS NOT OLD.first_public_update_due_at
		      OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at
		      OR NEW.version <> OLD.version + 1 OR OLD.state IN ('resolved', 'cancelled')
		      OR (NEW.state = OLD.state AND NOT (
		        (OLD.status_page_incident_reference IS NULL
		          AND NEW.status_page_incident_reference IS NOT NULL
		          AND NEW.next_public_update_due_at IS OLD.next_public_update_due_at
		          AND NEW.resolved_at IS OLD.resolved_at AND NEW.cancelled_at IS OLD.cancelled_at)
		        OR
		        (NEW.status_page_incident_reference IS OLD.status_page_incident_reference
		          AND NEW.resolved_at IS OLD.resolved_at AND NEW.cancelled_at IS OLD.cancelled_at
		          AND EXISTS (
		            SELECT 1 FROM (
		              SELECT update_kind, published_at FROM stage6_incident_updates
		              WHERE incident_id = NEW.id
		              ORDER BY published_at DESC, id DESC LIMIT 1
		            ) latest
		            WHERE (
		                (latest.update_kind = 'resolved' AND NEW.next_public_update_due_at IS NULL)
		                OR (latest.update_kind <> 'resolved' AND unixepoch(NEW.next_public_update_due_at) =
		                  unixepoch(latest.published_at) + CASE
		                    WHEN NEW.severity IN ('SEV-0', 'SEV-1') THEN 1800 ELSE 3600
		                  END)
		              )
		          ))
		      ))
		      OR (NEW.state <> OLD.state AND (
		        NEW.status_page_incident_reference IS NOT OLD.status_page_incident_reference
		        OR (OLD.state = 'investigating' AND NEW.state NOT IN ('identified', 'cancelled'))
		        OR (OLD.state = 'identified' AND NEW.state NOT IN ('monitoring', 'cancelled'))
		        OR (OLD.state = 'monitoring' AND NEW.state <> 'resolved')
		      ))
		      OR (NEW.state = 'cancelled' AND (
		        NEW.cancelled_at IS NULL OR NEW.resolved_at IS NOT NULL
		        OR EXISTS (SELECT 1 FROM stage6_incident_updates WHERE incident_id = NEW.id)
		      ))
		      OR (NEW.state = 'resolved' AND (
		        NEW.resolved_at IS NULL OR NEW.cancelled_at IS NOT NULL OR NEW.next_public_update_due_at IS NOT NULL
		        OR (NEW.public_impact = 1 AND NOT EXISTS (
		          SELECT 1 FROM stage6_incident_updates WHERE incident_id = NEW.id AND update_kind = 'resolved'
		        ))
		        OR (NEW.security_privacy_impact = 1 AND NOT EXISTS (
		          SELECT 1 FROM stage6_incident_resolution_approvals
		          WHERE incident_id = NEW.id AND decision = 'approved'
		            AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
		            AND evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		        ))
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_updates_insert`,
		`CREATE TRIGGER trg_stage6_incident_updates_insert
		 BEFORE INSERT ON stage6_incident_updates
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 public incident update')
		   WHERE NEW.update_kind NOT IN ('initial', 'progress', 'resolved')
		      OR length(trim(NEW.summary)) NOT BETWEEN 20 AND 2000
		      OR length(trim(NEW.external_reference)) NOT BETWEEN 12 AND 2048
		      OR substr(NEW.external_reference, 1, 8) <> 'https://'
		      OR NOT EXISTS (
		        SELECT 1 FROM stage6_incidents incident
		        JOIN tenant_memberships membership
		          ON membership.tenant_id = incident.operator_tenant_id
		         AND membership.user_id = NEW.created_by
		         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
		        JOIN users operator_user ON operator_user.id = membership.user_id
		         AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
		        WHERE incident.id = NEW.incident_id AND incident.operator_tenant_id = NEW.operator_tenant_id
		          AND incident.public_impact = 1 AND incident.status_page_incident_reference IS NOT NULL
		          AND substr(NEW.external_reference, 1, length(incident.status_page_origin) + 1) =
		            incident.status_page_origin || '/'
		          AND incident.state NOT IN ('resolved', 'cancelled')
		          AND incident.communications_lead_user_id = NEW.created_by
		          AND unixepoch(NEW.published_at) >= unixepoch(incident.impact_confirmed_at)
		          AND unixepoch(NEW.published_at) <= unixepoch('now') + 300
		          AND (NEW.update_kind <> 'resolved' OR incident.state = 'monitoring')
		      )
		      OR EXISTS (
		        SELECT 1 FROM stage6_incident_updates previous
		        WHERE previous.incident_id = NEW.incident_id
		          AND (previous.update_kind = 'resolved' OR unixepoch(previous.published_at) >= unixepoch(NEW.published_at))
		      )
		      OR (NOT EXISTS (
		        SELECT 1 FROM stage6_incident_updates previous WHERE previous.incident_id = NEW.incident_id
		      ) AND NEW.update_kind <> 'initial')
		      OR (EXISTS (
		        SELECT 1 FROM stage6_incident_updates previous WHERE previous.incident_id = NEW.incident_id
		      ) AND NEW.update_kind = 'initial');
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_updates_no_update`,
		`CREATE TRIGGER trg_stage6_incident_updates_no_update
		 BEFORE UPDATE ON stage6_incident_updates
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 public incident updates are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_updates_no_delete`,
		`CREATE TRIGGER trg_stage6_incident_updates_no_delete
		 BEFORE DELETE ON stage6_incident_updates
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 public incident updates are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_resolution_approvals_insert`,
		`CREATE TRIGGER trg_stage6_incident_resolution_approvals_insert
		 BEFORE INSERT ON stage6_incident_resolution_approvals
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 incident resolution approval')
		   WHERE NEW.decision NOT IN ('approved', 'rejected')
		      OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000
		      OR length(trim(NEW.evidence_reference)) NOT BETWEEN 12 AND 2048
		      OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		      OR NEW.evidence_sha256 IS NULL OR length(NEW.evidence_sha256) <> 71
		      OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		      OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		      OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		      OR NOT EXISTS (
		        SELECT 1 FROM stage6_incidents incident
		        JOIN tenant_memberships membership
		          ON membership.tenant_id = incident.operator_tenant_id
		         AND membership.user_id = NEW.approver_user_id
		         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
		        JOIN users operator_user ON operator_user.id = membership.user_id
		         AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
		        WHERE incident.id = NEW.incident_id AND incident.operator_tenant_id = NEW.operator_tenant_id
		          AND incident.security_privacy_impact = 1 AND incident.state = 'monitoring'
		          AND incident.security_privacy_lead_user_id = NEW.approver_user_id
		          AND incident.incident_commander_user_id <> NEW.approver_user_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_resolution_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_incident_resolution_approvals_no_update
		 BEFORE UPDATE ON stage6_incident_resolution_approvals
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 incident resolution approvals are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incident_resolution_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_incident_resolution_approvals_no_delete
		 BEFORE DELETE ON stage6_incident_resolution_approvals
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 incident resolution approvals are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_incidents_no_delete`,
		`CREATE TRIGGER trg_stage6_incidents_no_delete
		 BEFORE DELETE ON stage6_incidents
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 incidents are retained as immutable operational history'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 incident governance safety migration: %w", err)
		}
	}
	return nil
}

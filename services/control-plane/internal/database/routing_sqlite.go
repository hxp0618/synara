package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateRoutingSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`UPDATE agent_sessions
		 SET requested_execution_target_id = execution_target_id
		 WHERE requested_execution_target_id IS NULL OR requested_execution_target_id = ''`,
		`UPDATE execution_interactions
		 SET source_execution_id = execution_id
		 WHERE source_execution_id IS NULL OR source_execution_id = ''`,
		`DROP TRIGGER IF EXISTS trg_execution_target_groups_route_insert`,
		`CREATE TRIGGER trg_execution_target_groups_route_insert
		 BEFORE INSERT ON execution_target_groups
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Target Group')
		   WHERE NEW.version <= 0
		      OR NEW.strategy NOT IN ('priority', 'balanced', 'latency')
		      OR NEW.status NOT IN ('active', 'draining', 'disabled')
		      OR NEW.max_failover_attempts NOT BETWEEN 0 AND 20
		      OR NEW.health_max_staleness_seconds NOT BETWEEN 10 AND 3600
		      OR length(trim(NEW.name)) NOT BETWEEN 1 AND 160
		      OR NOT json_valid(NEW.preferred_regions)
		      OR json_type(NEW.preferred_regions) <> 'array';
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_groups_route_update`,
		`CREATE TRIGGER trg_execution_target_groups_route_update
		 BEFORE UPDATE ON execution_target_groups
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Target Group ownership is immutable')
		   WHERE NEW.id IS NOT OLD.id OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.organization_id IS NOT OLD.organization_id;
		   SELECT RAISE(ABORT, 'Execution Target Group version must advance exactly once')
		   WHERE NEW.version <> OLD.version + 1;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_groups_route_delete`,
		`CREATE TRIGGER trg_execution_target_groups_route_delete
		 BEFORE DELETE ON execution_target_groups
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Target Groups cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_group_members_route_insert`,
		`CREATE TRIGGER trg_execution_target_group_members_route_insert
		 BEFORE INSERT ON execution_target_group_members
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Target Group member')
		   WHERE NEW.version <= 0 OR NEW.priority NOT BETWEEN 0 AND 1000000
		      OR NEW.weight NOT BETWEEN 1 AND 10000
		      OR NEW.status NOT IN ('active', 'draining', 'disabled')
		      OR length(trim(NEW.region)) NOT BETWEEN 1 AND 120
		      OR length(trim(NEW.cluster_id)) NOT BETWEEN 1 AND 200
		      OR NOT EXISTS (
		        SELECT 1
		        FROM execution_target_groups AS target_group
		        JOIN execution_targets AS target ON target.id = NEW.execution_target_id
		        WHERE target_group.tenant_id = NEW.tenant_id AND target_group.id = NEW.target_group_id
		          AND (target.tenant_id IS NULL OR target.tenant_id = NEW.tenant_id)
		          AND (target_group.organization_id IS NULL OR target.organization_id IS NULL
		            OR target_group.organization_id = target.organization_id)
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_group_members_route_update`,
		`CREATE TRIGGER trg_execution_target_group_members_route_update
		 BEFORE UPDATE ON execution_target_group_members
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Target Group member identity is immutable')
		   WHERE NEW.id IS NOT OLD.id OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.target_group_id IS NOT OLD.target_group_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id;
		   SELECT RAISE(ABORT, 'Execution Target Group member version must advance exactly once')
		   WHERE NEW.version <> OLD.version + 1;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_group_members_route_delete`,
		`CREATE TRIGGER trg_execution_target_group_members_route_delete
		 BEFORE DELETE ON execution_target_group_members
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Target Group members cannot be deleted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_health_route_insert`,
		`CREATE TRIGGER trg_execution_target_health_route_insert
		 BEFORE INSERT ON execution_target_health
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Target health observation')
		   WHERE NEW.status NOT IN ('healthy', 'degraded', 'unreachable', 'unknown')
		      OR NEW.capacity_status NOT IN ('available', 'saturated', 'unknown')
		      OR NEW.version <= 0 OR NEW.allocated_capacity_units < 0
		      OR (NEW.available_capacity_units IS NOT NULL AND NEW.available_capacity_units < 0)
		      OR NEW.expires_at <= NEW.observed_at
		      OR length(trim(NEW.source)) NOT BETWEEN 1 AND 160
		      OR (NEW.reason IS NOT NULL AND length(NEW.reason) > 2000);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_health_route_update`,
		`CREATE TRIGGER trg_execution_target_health_route_update
		 BEFORE UPDATE ON execution_target_health
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Target health update is stale or not monotonic')
		   WHERE NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.version <> OLD.version + 1 OR NEW.observed_at <= OLD.observed_at;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_health_route_delete`,
		`CREATE TRIGGER trg_execution_target_health_route_delete
			 BEFORE DELETE ON execution_target_health
			 BEGIN
			   SELECT RAISE(ABORT, 'Execution Target health observations cannot be deleted');
			 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_dr_readiness_route_insert`,
		`CREATE TRIGGER trg_execution_target_dr_readiness_route_insert
			 BEFORE INSERT ON execution_target_dr_readiness
			 BEGIN
			   SELECT RAISE(ABORT, 'invalid Execution Target DR readiness observation')
			   WHERE NEW.version <= 0
			      OR NEW.expires_at <= NEW.observed_at
			      OR NEW.replicated_through_at > NEW.observed_at
			      OR length(trim(NEW.source_dr_domain)) NOT BETWEEN 1 AND 200
			      OR length(trim(NEW.dr_domain)) NOT BETWEEN 1 AND 200
			      OR length(trim(NEW.publisher_identity)) NOT BETWEEN 1 AND 200
			      OR (NEW.reason IS NOT NULL AND length(NEW.reason) > 2000);
			 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_dr_readiness_route_update`,
		`CREATE TRIGGER trg_execution_target_dr_readiness_route_update
			 BEFORE UPDATE ON execution_target_dr_readiness
			 BEGIN
			   SELECT RAISE(ABORT, 'Execution Target DR readiness identity is immutable')
			   WHERE NEW.execution_target_id IS NOT OLD.execution_target_id
			      OR NEW.source_dr_domain IS NOT OLD.source_dr_domain;
			   SELECT RAISE(ABORT, 'Execution Target DR readiness update is stale or not monotonic')
			   WHERE NEW.version <> OLD.version + 1 OR NEW.observed_at <= OLD.observed_at;
			   SELECT RAISE(ABORT, 'invalid Execution Target DR readiness observation')
			   WHERE NEW.expires_at <= NEW.observed_at
			      OR NEW.replicated_through_at > NEW.observed_at
			      OR length(trim(NEW.source_dr_domain)) NOT BETWEEN 1 AND 200
			      OR length(trim(NEW.dr_domain)) NOT BETWEEN 1 AND 200
			      OR length(trim(NEW.publisher_identity)) NOT BETWEEN 1 AND 200
			      OR (NEW.reason IS NOT NULL AND length(NEW.reason) > 2000);
			 END`,
		`DROP TRIGGER IF EXISTS trg_execution_target_dr_readiness_route_delete`,
		`CREATE TRIGGER trg_execution_target_dr_readiness_route_delete
			 BEFORE DELETE ON execution_target_dr_readiness
			 BEGIN
			   SELECT RAISE(ABORT, 'Execution Target DR readiness observations cannot be deleted');
			 END`,
		`CREATE INDEX IF NOT EXISTS idx_execution_location_outages_route
		 ON execution_location_outages (tenant_id, region, cluster_id, expires_at)`,
		`DROP TRIGGER IF EXISTS trg_execution_location_outages_route_insert`,
		`CREATE TRIGGER trg_execution_location_outages_route_insert
			 BEFORE INSERT ON execution_location_outages
			 BEGIN
			   SELECT RAISE(ABORT, 'invalid location outage observation')
			   WHERE NEW.version <= 0
			      OR NEW.status NOT IN ('draining', 'unreachable')
			      OR NEW.expires_at <= NEW.observed_at
			      OR length(trim(NEW.region)) NOT BETWEEN 1 AND 120
			      OR (NEW.cluster_id <> '' AND length(trim(NEW.cluster_id)) NOT BETWEEN 1 AND 200)
			      OR length(trim(NEW.publisher_identity)) NOT BETWEEN 1 AND 200
			      OR (NEW.reason IS NOT NULL AND length(NEW.reason) > 2000);
			 END`,
		`DROP TRIGGER IF EXISTS trg_execution_location_outages_route_update`,
		`CREATE TRIGGER trg_execution_location_outages_route_update
			 BEFORE UPDATE ON execution_location_outages
			 BEGIN
			   SELECT RAISE(ABORT, 'Location outage authority identity is immutable')
			   WHERE NEW.tenant_id IS NOT OLD.tenant_id
			      OR NEW.region IS NOT OLD.region
			      OR NEW.cluster_id IS NOT OLD.cluster_id;
			   SELECT RAISE(ABORT, 'Location outage authority update is stale or not monotonic')
			   WHERE NEW.version <> OLD.version + 1 OR NEW.observed_at <= OLD.observed_at;
			   SELECT RAISE(ABORT, 'invalid location outage observation')
			   WHERE NEW.status NOT IN ('draining', 'unreachable')
			      OR NEW.expires_at <= NEW.observed_at
			      OR length(trim(NEW.region)) NOT BETWEEN 1 AND 120
			      OR (NEW.cluster_id <> '' AND length(trim(NEW.cluster_id)) NOT BETWEEN 1 AND 200)
			      OR length(trim(NEW.publisher_identity)) NOT BETWEEN 1 AND 200
			      OR (NEW.reason IS NOT NULL AND length(NEW.reason) > 2000);
			 END`,
		`DROP TRIGGER IF EXISTS trg_execution_location_outages_route_delete`,
		`CREATE TRIGGER trg_execution_location_outages_route_delete
			 BEFORE DELETE ON execution_location_outages
			 BEGIN
			   SELECT RAISE(ABORT, 'Location outage observations cannot be deleted');
			 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_target_routing_insert`,
		`CREATE TRIGGER trg_agent_sessions_target_routing_insert
			 BEFORE INSERT ON agent_sessions
			 BEGIN
		   SELECT RAISE(ABORT, 'invalid Session Target routing shape')
		   WHERE NEW.requested_execution_target_id IS NULL
		      OR ((NEW.execution_target_group_id IS NULL) <> (NEW.routing_policy_version IS NULL))
		      OR (NEW.routing_policy_version IS NOT NULL AND NEW.routing_policy_version <= 0)
		      OR (NEW.preferred_execution_region IS NOT NULL
		        AND length(trim(NEW.preferred_execution_region)) NOT BETWEEN 1 AND 120)
		      OR (NEW.execution_target_group_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1
		        FROM execution_target_groups AS target_group
		        JOIN execution_target_group_members AS member
		          ON member.tenant_id = target_group.tenant_id
		         AND member.target_group_id = target_group.id
		         AND member.execution_target_id = NEW.execution_target_id
		        WHERE target_group.tenant_id = NEW.tenant_id
		          AND target_group.id = NEW.execution_target_group_id
		          AND target_group.version = NEW.routing_policy_version
		          AND target_group.status = 'active' AND member.status = 'active'
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_sessions_target_routing_update`,
		`CREATE TRIGGER trg_agent_sessions_target_routing_update
		 BEFORE UPDATE OF requested_execution_target_id, execution_target_group_id,
		   routing_policy_version, preferred_execution_region ON agent_sessions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Session Target routing shape')
		   WHERE NEW.requested_execution_target_id IS NULL
		      OR ((NEW.execution_target_group_id IS NULL) <> (NEW.routing_policy_version IS NULL))
		      OR (NEW.routing_policy_version IS NOT NULL AND NEW.routing_policy_version <= 0)
		      OR (NEW.preferred_execution_region IS NOT NULL
		        AND length(trim(NEW.preferred_execution_region)) NOT BETWEEN 1 AND 120)
		      OR (NEW.execution_target_group_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1
		        FROM execution_target_groups AS target_group
		        JOIN execution_target_group_members AS member
		          ON member.tenant_id = target_group.tenant_id
		         AND member.target_group_id = target_group.id
		         AND member.execution_target_id = NEW.execution_target_id
		        WHERE target_group.tenant_id = NEW.tenant_id
		          AND target_group.id = NEW.execution_target_group_id
		          AND target_group.version = NEW.routing_policy_version
		          AND target_group.status = 'active' AND member.status = 'active'
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_target_routing_insert`,
		`CREATE TRIGGER trg_agent_executions_target_routing_insert
		 BEFORE INSERT ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Target routing snapshot')
		   WHERE NOT (
		     (NEW.target_group_id IS NULL AND NEW.target_group_version IS NULL
		       AND NEW.target_group_member_version IS NULL AND NEW.selected_region IS NULL
		       AND NEW.selected_cluster_id IS NULL AND NEW.routing_reason IS NULL)
		     OR
		     (NEW.target_group_id IS NOT NULL AND NEW.target_group_version > 0
		       AND NEW.target_group_member_version > 0
		       AND length(trim(NEW.selected_region)) BETWEEN 1 AND 120
		       AND length(trim(NEW.selected_cluster_id)) BETWEEN 1 AND 200
		       AND NEW.routing_reason IN ('preferred-target', 'priority', 'balanced', 'latency', 'disaster-recovery'))
		   ) OR (NEW.target_group_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1
		     FROM execution_target_groups AS target_group
		     JOIN execution_target_group_members AS member
		       ON member.tenant_id = target_group.tenant_id
		      AND member.target_group_id = target_group.id
		      AND member.execution_target_id = NEW.execution_target_id
		     WHERE target_group.tenant_id = NEW.tenant_id AND target_group.id = NEW.target_group_id
		       AND target_group.version = NEW.target_group_version
		       AND member.version = NEW.target_group_member_version
		       AND member.region = NEW.selected_region AND member.cluster_id = NEW.selected_cluster_id
		   )) OR (NEW.predecessor_execution_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1 FROM agent_executions AS predecessor
		     WHERE predecessor.tenant_id = NEW.tenant_id AND predecessor.id = NEW.predecessor_execution_id
		       AND predecessor.session_id = NEW.session_id AND predecessor.turn_id = NEW.turn_id
		       AND predecessor.attempt < NEW.attempt
		       AND predecessor.status IN ('completed', 'failed', 'cancelled', 'interrupted')
		       AND NOT EXISTS (
		         SELECT 1 FROM worker_leases AS lease
		         WHERE lease.tenant_id = NEW.tenant_id AND lease.execution_id = predecessor.id
		       )
		   ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_target_routing_update`,
		`CREATE TRIGGER trg_agent_executions_target_routing_update
		 BEFORE UPDATE OF target_group_id, target_group_version, target_group_member_version,
		   selected_region, selected_cluster_id, routing_reason, predecessor_execution_id ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution global routing snapshot is immutable')
		   WHERE OLD.target_group_id IS NOT NULL AND (
		     NEW.target_group_id IS NOT OLD.target_group_id
		     OR NEW.target_group_version IS NOT OLD.target_group_version
		     OR NEW.target_group_member_version IS NOT OLD.target_group_member_version
		     OR NEW.selected_region IS NOT OLD.selected_region
		     OR NEW.selected_cluster_id IS NOT OLD.selected_cluster_id
		     OR NEW.routing_reason IS NOT OLD.routing_reason
		     OR NEW.predecessor_execution_id IS NOT OLD.predecessor_execution_id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_failover_route_insert`,
		`CREATE TRIGGER trg_execution_interactions_failover_route_insert
		 BEFORE INSERT ON execution_interactions
		 BEGIN
		   SELECT RAISE(ABORT, 'Interaction source Execution is required')
		   WHERE NEW.source_execution_id IS NULL;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_interactions_failover_route_update`,
		`CREATE TRIGGER trg_execution_interactions_failover_route_update
		 BEFORE UPDATE OF execution_id, source_execution_id ON execution_interactions
		 BEGIN
		   SELECT RAISE(ABORT, 'Interaction source Execution is immutable')
		   WHERE NEW.source_execution_id IS NOT OLD.source_execution_id;
		   SELECT RAISE(ABORT, 'Only unbound Interaction state can move during failover')
		   WHERE NEW.execution_id IS NOT OLD.execution_id AND (
		     OLD.delivery_status NOT IN ('not-ready', 'resume-recorded')
		     OR NEW.delivery_status <> OLD.delivery_status OR NEW.resume_bundle_id IS NOT NULL
		     OR NOT EXISTS (
		       SELECT 1 FROM agent_executions AS destination
		       WHERE destination.tenant_id = NEW.tenant_id AND destination.id = NEW.execution_id
		         AND destination.session_id = NEW.session_id AND destination.turn_id = NEW.turn_id
		         AND destination.predecessor_execution_id = OLD.execution_id
		     )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_failover_attempts_route_update`,
		`CREATE TRIGGER trg_execution_failover_attempts_route_update
		 BEFORE UPDATE ON execution_failover_attempts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution failover attempt lineage is immutable')
		   WHERE OLD.status <> 'planned' OR NEW.id IS NOT OLD.id OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.session_id IS NOT OLD.session_id OR NEW.turn_id IS NOT OLD.turn_id
		      OR NEW.target_group_id IS NOT OLD.target_group_id
		      OR NEW.source_execution_id IS NOT OLD.source_execution_id
		      OR NEW.source_execution_target_id IS NOT OLD.source_execution_target_id
		      OR NEW.destination_execution_target_id IS NOT OLD.destination_execution_target_id
		      OR NEW.source_generation IS NOT OLD.source_generation
		      OR NEW.source_recovery_bundle_id IS NOT OLD.source_recovery_bundle_id
		      OR NEW.leader_fencing_token IS NOT OLD.leader_fencing_token
		      OR NEW.reason IS NOT OLD.reason OR NEW.requested_at IS NOT OLD.requested_at
		      OR NEW.status NOT IN ('committed', 'failed', 'aborted');
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_failover_attempts_route_delete`,
		`CREATE TRIGGER trg_execution_failover_attempts_route_delete
		 BEFORE DELETE ON execution_failover_attempts
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution failover attempts cannot be deleted');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Target routing safety migration: %w", err)
		}
	}
	return nil
}

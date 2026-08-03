// Package retentiongate contains the single SQL authority for applying active
// Legal Hold scopes to retention and deletion candidate queries.
package retentiongate

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const SweepLockKey = "synara:tenant-retention-sweeper"

var localTenantMutationLocks sync.Map

func tenantMutationLockKey(tenantID uuid.UUID) string {
	return "synara:legal-hold:" + tenantID.String()
}

// LockHoldMutation serializes a Hold create/release with the retention sweep,
// long-running object deletion, and Tenant deletion's existing row lock.
func LockHoldMutation(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID) error {
	if tx.Dialector.Name() == "postgres" {
		for _, key := range []string{SweepLockKey, tenantMutationLockKey(tenantID)} {
			if err := tx.WithContext(ctx).
				Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", key).Error; err != nil {
				return fmt.Errorf("acquire Legal Hold mutation lock: %w", err)
			}
		}
	}
	var tenant persistence.Tenant
	return persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Select("id").Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error
}

// AcquireResourceMutation spans external object-store work that cannot live in
// one SQL transaction. A Legal Hold create waits for this lock before commit.
func AcquireResourceMutation(
	ctx context.Context,
	db *gorm.DB,
	tenantID uuid.UUID,
) (func(), bool, error) {
	if db.Dialector.Name() != "postgres" {
		return acquireLocalMutation(tenantID), true, nil
	}
	return persistence.TryAdvisoryLock(ctx, db, tenantMutationLockKey(tenantID))
}

func AcquireHoldMutation(db *gorm.DB, tenantID uuid.UUID) func() {
	if db.Dialector.Name() == "postgres" {
		return func() {}
	}
	return acquireLocalMutation(tenantID)
}

func acquireLocalMutation(tenantID uuid.UUID) func() {
	lockValue, _ := localTenantMutationLocks.LoadOrStore(tenantID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func ExcludeSessions(db *gorm.DB, alias string) *gorm.DB {
	return db.Where(`NOT EXISTS (
		SELECT 1 FROM legal_holds AS legal_hold
		WHERE legal_hold.tenant_id = ` + alias + `.tenant_id
		  AND legal_hold.status = 'active'
		  AND (
		    legal_hold.scope_type = 'tenant'
		    OR (legal_hold.scope_type = 'organization' AND legal_hold.scope_id = ` + alias + `.organization_id)
		    OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = ` + alias + `.project_id)
		    OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = ` + alias + `.id)
		    OR (legal_hold.scope_type = 'user' AND (
		      legal_hold.scope_id = ` + alias + `.created_by
		      OR EXISTS (
		        SELECT 1 FROM agent_turns AS held_turn
		        WHERE held_turn.tenant_id = ` + alias + `.tenant_id
		          AND held_turn.session_id = ` + alias + `.id
		          AND held_turn.created_by = legal_hold.scope_id
		      )
		    ))
		  )
	)`)
}

func ExcludeArtifacts(db *gorm.DB, alias string) *gorm.DB {
	return db.Where(`NOT EXISTS (
		SELECT 1 FROM legal_holds AS legal_hold
		WHERE legal_hold.tenant_id = ` + alias + `.tenant_id
		  AND legal_hold.status = 'active'
		  AND (
		    legal_hold.scope_type = 'tenant'
		    OR (legal_hold.scope_type = 'organization' AND legal_hold.scope_id = ` + alias + `.organization_id)
		    OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = ` + alias + `.project_id)
		    OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = ` + alias + `.session_id)
		    OR (legal_hold.scope_type = 'user' AND (
		      legal_hold.scope_id = ` + alias + `.created_by_id
		      OR EXISTS (
		        SELECT 1 FROM agent_sessions AS held_session
		        WHERE held_session.tenant_id = ` + alias + `.tenant_id
		          AND held_session.id = ` + alias + `.session_id
		          AND held_session.created_by = legal_hold.scope_id
		      )
		    ))
		  )
	)`)
}

func ExcludeExecutions(db *gorm.DB, alias string) *gorm.DB {
	return db.Where(`NOT EXISTS (
		SELECT 1 FROM legal_holds AS legal_hold
		JOIN agent_sessions AS held_session
		  ON held_session.tenant_id = ` + alias + `.tenant_id
		 AND held_session.id = ` + alias + `.session_id
		WHERE legal_hold.tenant_id = ` + alias + `.tenant_id
		  AND legal_hold.status = 'active'
		  AND (
		    legal_hold.scope_type = 'tenant'
		    OR (legal_hold.scope_type = 'organization' AND legal_hold.scope_id = held_session.organization_id)
		    OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = held_session.project_id)
		    OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = held_session.id)
		    OR (legal_hold.scope_type = 'user' AND (
		      legal_hold.scope_id = held_session.created_by
		      OR legal_hold.scope_id = ` + alias + `.requested_by
		    ))
		  )
	)`)
}

func ExcludeCheckpoints(db *gorm.DB, alias string) *gorm.DB {
	return db.Where(`NOT EXISTS (
		SELECT 1 FROM legal_holds AS legal_hold
		JOIN agent_sessions AS held_session
		  ON held_session.tenant_id = ` + alias + `.tenant_id
		 AND held_session.id = ` + alias + `.session_id
		WHERE legal_hold.tenant_id = ` + alias + `.tenant_id
		  AND legal_hold.status = 'active'
		  AND (
		    legal_hold.scope_type = 'tenant'
		    OR (legal_hold.scope_type = 'organization' AND legal_hold.scope_id = held_session.organization_id)
		    OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = held_session.project_id)
		    OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = held_session.id)
		    OR (legal_hold.scope_type = 'user' AND legal_hold.scope_id = held_session.created_by)
		  )
	)`)
}

func ExcludeWorkspaces(db *gorm.DB, alias string) *gorm.DB {
	return db.Where(`NOT EXISTS (
		SELECT 1 FROM legal_holds AS legal_hold
		JOIN agent_sessions AS held_session
		  ON held_session.tenant_id = ` + alias + `.tenant_id
		 AND held_session.id = ` + alias + `.session_id
		WHERE legal_hold.tenant_id = ` + alias + `.tenant_id
		  AND legal_hold.status = 'active'
		  AND (
		    legal_hold.scope_type = 'tenant'
		    OR (legal_hold.scope_type = 'organization' AND legal_hold.scope_id = held_session.organization_id)
		    OR (legal_hold.scope_type = 'project' AND legal_hold.scope_id = held_session.project_id)
		    OR (legal_hold.scope_type = 'session' AND legal_hold.scope_id = held_session.id)
		    OR (legal_hold.scope_type = 'user' AND legal_hold.scope_id = held_session.created_by)
		  )
	)`)
}

func HasActiveTenantHold(db *gorm.DB, tenantID any) (bool, error) {
	var count int64
	err := db.Table("legal_holds").Where("tenant_id = ? AND status = ?", tenantID, "active").Count(&count).Error
	return count > 0, err
}

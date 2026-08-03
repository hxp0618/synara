package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateDesktopEnrollmentSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_desktop_devices_subject_key
		 ON desktop_devices (control_plane_origin, user_id, public_key_sha256)`,
		`CREATE INDEX IF NOT EXISTS idx_desktop_devices_subject_status
		 ON desktop_devices (user_id, status, last_seen_at DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_desktop_devices_tenant_status
		 ON desktop_devices (default_tenant_id, status, last_seen_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_desktop_enrollments_secret_hash
		 ON desktop_enrollments (secret_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_desktop_enrollments_pending_expiry
		 ON desktop_enrollments (expires_at, id) WHERE status = 'pending'`,
		`CREATE INDEX IF NOT EXISTS idx_desktop_enrollments_subject
		 ON desktop_enrollments (subject_user_id, tenant_id, created_at DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_desktop_enrollments_platform
		 ON desktop_enrollments (tenant_id, status, created_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_login_sessions_rotated_from
		 ON login_sessions (rotated_from_session_id) WHERE rotated_from_session_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_login_sessions_rotated_to
		 ON login_sessions (rotated_to_session_id) WHERE rotated_to_session_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_login_sessions_desktop_family_active
		 ON login_sessions (credential_family_id, expires_at DESC, id)
		 WHERE audience = 'desktop' AND revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_login_sessions_desktop_device_active
		 ON login_sessions (desktop_device_id, expires_at DESC, id)
		 WHERE audience = 'desktop' AND revoked_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_desktop_devices_insert`,
		`CREATE TRIGGER trg_desktop_devices_insert
		 BEFORE INSERT ON desktop_devices
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Desktop device')
		   WHERE length(NEW.control_plane_origin) NOT BETWEEN 8 AND 2048
		      OR length(NEW.public_key) <> 32 OR length(NEW.public_key_sha256) <> 32
		      OR NEW.platform NOT IN ('darwin', 'win32', 'linux')
		      OR length(NEW.app_version) NOT BETWEEN 1 AND 80
		      OR length(trim(NEW.device_label)) NOT BETWEEN 1 AND 160
		      OR NEW.status <> 'active' OR NEW.version <> 1
		      OR NEW.revoked_by IS NOT NULL OR NEW.revocation_reason IS NOT NULL OR NEW.revoked_at IS NOT NULL
		      OR NOT EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND status = 'active' AND deleted_at IS NULL)
		      OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = NEW.default_tenant_id AND deleted_at IS NULL)
		      OR (NEW.default_organization_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM organizations
		        WHERE tenant_id = NEW.default_tenant_id AND id = NEW.default_organization_id
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_desktop_devices_update`,
		`CREATE TRIGGER trg_desktop_devices_update
		 BEFORE UPDATE ON desktop_devices
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Desktop device update')
		   WHERE NEW.id IS NOT OLD.id OR NEW.control_plane_origin <> OLD.control_plane_origin
		      OR NEW.user_id IS NOT OLD.user_id OR NEW.public_key <> OLD.public_key
		      OR NEW.public_key_sha256 <> OLD.public_key_sha256 OR NEW.created_at <> OLD.created_at
		      OR NEW.version <> OLD.version + 1
		      OR NEW.status NOT IN ('active', 'revoked')
		      OR (NEW.status = 'active' AND (NEW.revoked_by IS NOT NULL OR NEW.revocation_reason IS NOT NULL OR NEW.revoked_at IS NOT NULL))
		      OR (NEW.status = 'revoked' AND (
		        OLD.status <> 'active' OR length(trim(ifnull(NEW.revocation_reason, ''))) NOT BETWEEN 10 AND 1000
		        OR NEW.revoked_at IS NULL
		      ))
		      OR (NEW.default_organization_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM organizations
		        WHERE tenant_id = NEW.default_tenant_id AND id = NEW.default_organization_id
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_login_sessions_desktop_insert`,
		`CREATE TRIGGER trg_login_sessions_desktop_insert
		 BEFORE INSERT ON login_sessions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid login session audience')
		   WHERE NEW.audience NOT IN ('web', 'desktop')
		      OR (NEW.audience = 'web' AND (
		        NOT ((NEW.auth_method = 'local' AND NEW.identity_connection_id IS NULL)
		          OR (NEW.auth_method = 'sso' AND NEW.identity_connection_id IS NOT NULL))
		        OR NEW.desktop_device_id IS NOT NULL
		        OR NEW.credential_family_id IS NOT NULL OR NEW.rotated_from_session_id IS NOT NULL
		        OR NEW.rotated_to_session_id IS NOT NULL OR NEW.rotated_at IS NOT NULL OR NEW.replay_detected_at IS NOT NULL
		      ))
		      OR (NEW.audience = 'desktop' AND (
		        NEW.auth_method <> 'desktop' OR NEW.identity_connection_id IS NOT NULL
		        OR NEW.desktop_device_id IS NULL OR NEW.credential_family_id IS NULL
		        OR NOT EXISTS (SELECT 1 FROM desktop_devices WHERE id = NEW.desktop_device_id AND user_id = NEW.user_id AND status = 'active')
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_login_sessions_desktop_update`,
		`CREATE TRIGGER trg_login_sessions_desktop_update
		 BEFORE UPDATE ON login_sessions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid login session audience update')
		   WHERE NEW.audience <> OLD.audience
		      OR (NEW.audience = 'web' AND (
		        NOT ((NEW.auth_method = 'local' AND NEW.identity_connection_id IS NULL)
		          OR (NEW.auth_method = 'sso' AND NEW.identity_connection_id IS NOT NULL))
		        OR NEW.desktop_device_id IS NOT NULL OR NEW.credential_family_id IS NOT NULL
		        OR NEW.rotated_from_session_id IS NOT NULL OR NEW.rotated_to_session_id IS NOT NULL
		        OR NEW.rotated_at IS NOT NULL OR NEW.replay_detected_at IS NOT NULL
		      ))
		      OR (NEW.audience = 'desktop' AND (
		        NEW.auth_method <> 'desktop' OR NEW.identity_connection_id IS NOT NULL
		        OR NEW.desktop_device_id IS NOT OLD.desktop_device_id
		        OR NEW.credential_family_id IS NOT OLD.credential_family_id
		        OR NEW.rotated_from_session_id IS NOT OLD.rotated_from_session_id
		      ))
		      OR (NEW.audience = 'desktop' AND (
		        NEW.rotated_to_session_id IS NOT NULL AND (NEW.rotated_at IS NULL OR NEW.revoked_at IS NULL)
		      ))
		      OR (NEW.replay_detected_at IS NOT NULL AND NEW.revoked_at IS NULL);
		 END`,
		`DROP TRIGGER IF EXISTS trg_desktop_enrollments_insert`,
		`CREATE TRIGGER trg_desktop_enrollments_insert
		 BEFORE INSERT ON desktop_enrollments
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Desktop Enrollment')
		   WHERE length(NEW.secret_hash) <> 32 OR NEW.status <> 'pending' OR NEW.version <> 1
		      OR NEW.mode NOT IN ('connect_existing', 'provisioned_then_connect') OR NEW.authority <> 'self'
		      OR NEW.issued_by_user_id <> NEW.subject_user_id
		      OR length(NEW.control_plane_origin) NOT BETWEEN 8 AND 2048
		      OR length(NEW.membership_version_snapshot) NOT BETWEEN 1 AND 200
		      OR length(trim(NEW.reason)) NOT BETWEEN 10 AND 1000
		      OR julianday(NEW.expires_at) <= julianday(NEW.created_at)
		      OR NEW.redeemed_at IS NOT NULL OR NEW.redeemed_device_id IS NOT NULL
		      OR NEW.redeemed_nonce_hash IS NOT NULL OR NEW.revoked_by IS NOT NULL
		      OR NEW.revoked_at IS NOT NULL OR NEW.terminal_reason IS NOT NULL
		      OR NEW.failed_attempts <> 0 OR NEW.last_failure_at IS NOT NULL
		      OR NOT EXISTS (SELECT 1 FROM login_sessions WHERE id = NEW.issued_by_session_id AND user_id = NEW.issued_by_user_id AND audience = 'web')
		      OR NOT EXISTS (SELECT 1 FROM tenant_memberships WHERE tenant_id = NEW.tenant_id AND user_id = NEW.subject_user_id AND status = 'active')
		      OR (NEW.organization_id IS NOT NULL AND NOT EXISTS (
		        SELECT 1 FROM organization_memberships
		        WHERE tenant_id = NEW.tenant_id AND organization_id = NEW.organization_id
		          AND user_id = NEW.subject_user_id AND status = 'active'
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_desktop_enrollments_update`,
		`CREATE TRIGGER trg_desktop_enrollments_update
		 BEFORE UPDATE ON desktop_enrollments
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Desktop Enrollment update')
		   WHERE NEW.id IS NOT OLD.id OR NEW.secret_hash <> OLD.secret_hash
		      OR NEW.mode <> OLD.mode OR NEW.authority <> OLD.authority
		      OR NEW.control_plane_origin <> OLD.control_plane_origin
		      OR NEW.issued_by_user_id IS NOT OLD.issued_by_user_id
		      OR NEW.issued_by_session_id IS NOT OLD.issued_by_session_id
		      OR NEW.subject_user_id IS NOT OLD.subject_user_id OR NEW.tenant_id IS NOT OLD.tenant_id
		      OR NEW.organization_id IS NOT OLD.organization_id
		      OR NEW.membership_version_snapshot <> OLD.membership_version_snapshot
		      OR NEW.reason <> OLD.reason OR NEW.expires_at <> OLD.expires_at OR NEW.created_at <> OLD.created_at
		      OR OLD.status <> 'pending' OR NEW.version <> OLD.version + 1
		      OR NEW.status NOT IN ('pending', 'redeemed', 'expired', 'revoked')
		      OR (NEW.status = 'pending' AND (
		        NEW.redeemed_at IS NOT NULL OR NEW.redeemed_device_id IS NOT NULL OR NEW.redeemed_nonce_hash IS NOT NULL
		        OR NEW.revoked_by IS NOT NULL OR NEW.revoked_at IS NOT NULL OR NEW.terminal_reason IS NOT NULL
		      ))
		      OR (NEW.status = 'redeemed' AND (
		        NEW.redeemed_at IS NULL OR NEW.redeemed_device_id IS NULL OR length(NEW.redeemed_nonce_hash) <> 32
		        OR NEW.revoked_by IS NOT NULL OR NEW.revoked_at IS NOT NULL OR NEW.terminal_reason IS NOT NULL
		      ))
		      OR (NEW.status = 'expired' AND (
		        NEW.redeemed_at IS NOT NULL OR NEW.redeemed_device_id IS NOT NULL OR NEW.redeemed_nonce_hash IS NOT NULL
		        OR NEW.revoked_by IS NOT NULL OR NEW.revoked_at IS NOT NULL OR NEW.terminal_reason <> 'expired'
		      ))
		      OR (NEW.status = 'revoked' AND (
		        NEW.redeemed_at IS NOT NULL OR NEW.redeemed_device_id IS NOT NULL OR NEW.redeemed_nonce_hash IS NOT NULL
		        OR NEW.revoked_at IS NULL OR length(trim(ifnull(NEW.terminal_reason, ''))) NOT BETWEEN 1 AND 160
		      ));
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Desktop Enrollment safety migration: %w", err)
		}
	}
	return nil
}

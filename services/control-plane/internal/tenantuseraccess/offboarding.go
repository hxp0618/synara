package tenantuseraccess

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// SuspensionResult records the mutable access that was revoked while the authoritative
// Tenant Membership was being suspended or removed. Immutable Grants remain fenced by
// their existing Membership and Credential revalidation paths.
type SuspensionResult struct {
	RevokedSessionCount           int64
	RevokedCredentialCount        int64
	RevokedDesktopEnrollmentCount int64
}

// Suspend revokes all mutable user-owned access for one Tenant inside the caller's transaction.
// The caller remains responsible for changing or deleting the Tenant Membership and recording Audit.
func Suspend(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, userID, revokedByUserID uuid.UUID,
	now time.Time,
) (SuspensionResult, error) {
	db := tx.WithContext(ctx)
	if err := db.Model(&persistence.OrganizationMembership{}).
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "active").
		Update("status", "suspended").Error; err != nil {
		return SuspensionResult{}, problem.Wrap(500, "tenant_user_organization_suspend_failed", "Organization memberships could not be suspended.", err)
	}
	sessionResult := db.Model(&persistence.LoginSession{}).
		Where("user_id = ? AND active_tenant_id = ? AND revoked_at IS NULL", userID, tenantID).
		Update("revoked_at", now)
	if sessionResult.Error != nil {
		return SuspensionResult{}, problem.Wrap(500, "tenant_user_session_revoke_failed", "Tenant Login Sessions could not be revoked.", sessionResult.Error)
	}
	credentialResult := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND scope = ? AND scope_user_id = ? AND revoked_at IS NULL", tenantID, "user", userID).
		Updates(map[string]any{
			"revoked_at": now, "revoked_by": revokedByUserID,
			"updated_by": revokedByUserID, "auto_select_enabled": false,
		})
	if credentialResult.Error != nil {
		return SuspensionResult{}, problem.Wrap(500, "tenant_user_credential_revoke_failed", "User Credentials could not be revoked.", credentialResult.Error)
	}
	enrollmentResult := db.Model(&persistence.DesktopEnrollment{}).
		Where("tenant_id = ? AND subject_user_id = ? AND status = ?", tenantID, userID, "pending").
		Updates(map[string]any{
			"status":          "revoked",
			"version":         gorm.Expr("version + 1"),
			"revoked_by":      revokedByUserID,
			"revoked_at":      now,
			"terminal_reason": "subject_offboarded",
		})
	if enrollmentResult.Error != nil {
		return SuspensionResult{}, problem.Wrap(500, "tenant_user_desktop_enrollment_revoke_failed", "Pending Desktop Enrollments could not be revoked.", enrollmentResult.Error)
	}
	// DesktopDevice is a user-global registration under one Control Plane origin and public key,
	// not a Tenant-owned credential. The revoked Desktop Login Sessions remove access; only
	// Platform administration or the user's authenticated Disconnect may revoke the Device itself.
	return SuspensionResult{
		RevokedSessionCount:           sessionResult.RowsAffected,
		RevokedCredentialCount:        credentialResult.RowsAffected,
		RevokedDesktopEnrollmentCount: enrollmentResult.RowsAffected,
	}, nil
}

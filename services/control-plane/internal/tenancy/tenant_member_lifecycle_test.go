package tenancy

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestManualTenantMemberSuspensionUsesSharedOffboardingClosure(t *testing.T) {
	db, tenantID, organizationID, ownerID := setupTenantMemberLifecycleTest(t)
	memberID := createTenantLifecycleMember(t, db, tenantID, organizationID, "member@example.com")
	now := time.Now().UTC()
	sessionID, desktopSessionID, desktopDeviceID, desktopEnrollmentID, credentialID :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	desktopFamilyID := uuid.New()
	if err := db.Create(&persistence.LoginSession{
		ID: sessionID, UserID: memberID, ActiveTenantID: &tenantID, AuthMethod: "local",
		RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.DesktopDevice{
		ID: desktopDeviceID, ControlPlaneOrigin: "https://control.example.com",
		UserID: memberID, DefaultTenantID: tenantID, DefaultOrganizationID: &organizationID,
		PublicKey: []byte(uuid.NewString()), PublicKeySHA256: []byte(uuid.NewString()),
		Platform: "darwin", AppVersion: "1.0.0", DeviceLabel: "Offboarded Mac",
		Status: "active", Version: 1, LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.LoginSession{
		ID: desktopSessionID, UserID: memberID, ActiveTenantID: &tenantID,
		AuthMethod: "desktop", Audience: "desktop", DesktopDeviceID: &desktopDeviceID,
		CredentialFamilyID: &desktopFamilyID, RefreshTokenHash: []byte(uuid.NewString()),
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.DesktopEnrollment{
		ID: desktopEnrollmentID, SecretHash: []byte(uuid.NewString()), Status: "pending", Version: 1,
		Mode: "connect_existing", Authority: "self", ControlPlaneOrigin: "https://control.example.com",
		IssuedByUserID: memberID, IssuedBySessionID: sessionID, SubjectUserID: memberID,
		TenantID: tenantID, OrganizationID: &organizationID, MembershipVersionSnapshot: "snapshot-v1",
		Reason: "Connect an approved Desktop device.", ExpiresAt: now.Add(time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ProviderCredential{
		ID: credentialID, TenantID: tenantID, Scope: "user", ScopeUserID: &memberID,
		Name: "Manual member BYOK", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted-payload"), EncryptedDataKey: []byte("encrypted-key"),
		KMSProvider: "local", KMSKeyID: "test", Version: 1, CreatedBy: memberID, UpdatedBy: memberID,
		AutoSelectEnabled: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	principal := identity.Principal{UserID: ownerID, ActiveTenantID: &tenantID}
	suspended := "suspended"
	updated, err := service.UpdateTenantMember(
		context.Background(), principal, tenantID, memberID,
		UpdateTenantMemberInput{Status: &suspended}, "manual-member-suspend", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "suspended" {
		t.Fatalf("membership status = %q, want suspended", updated.Status)
	}
	var organizationMembership persistence.OrganizationMembership
	if err := db.Where("tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, memberID).
		Take(&organizationMembership).Error; err != nil || organizationMembership.Status != "suspended" {
		t.Fatalf("Organization Membership was not suspended: %#v %v", organizationMembership, err)
	}
	var session persistence.LoginSession
	if err := db.Where("id = ?", sessionID).Take(&session).Error; err != nil || session.RevokedAt == nil {
		t.Fatalf("Login Session was not revoked: %#v %v", session, err)
	}
	var desktopSession persistence.LoginSession
	if err := db.Where("id = ?", desktopSessionID).Take(&desktopSession).Error; err != nil || desktopSession.RevokedAt == nil {
		t.Fatalf("Desktop Session was not revoked: %#v %v", desktopSession, err)
	}
	var credential persistence.ProviderCredential
	if err := db.Where("id = ?", credentialID).Take(&credential).Error; err != nil ||
		credential.RevokedAt == nil || credential.AutoSelectEnabled {
		t.Fatalf("user Credential was not revoked: %#v %v", credential, err)
	}
	var enrollment persistence.DesktopEnrollment
	if err := db.Where("id = ?", desktopEnrollmentID).Take(&enrollment).Error; err != nil ||
		enrollment.Status != "revoked" || enrollment.Version != 2 || enrollment.RevokedAt == nil ||
		enrollment.TerminalReason == nil || *enrollment.TerminalReason != "subject_offboarded" {
		t.Fatalf("pending Desktop Enrollment was not terminally revoked: %#v %v", enrollment, err)
	}
	var device persistence.DesktopDevice
	if err := db.Where("id = ?", desktopDeviceID).Take(&device).Error; err != nil ||
		device.Status != "active" || device.Version != 1 || device.RevokedAt != nil {
		t.Fatalf("Tenant offboarding mutated the user-global Desktop Device: %#v %v", device, err)
	}
	var offboardingAudit persistence.AuditLog
	if err := db.Where("request_id = ?", "manual-member-suspend").Take(&offboardingAudit).Error; err != nil {
		t.Fatal(err)
	}
	if auditInteger(offboardingAudit.Metadata["revokedSessionCount"]) != 2 ||
		auditInteger(offboardingAudit.Metadata["revokedCredentialCount"]) != 1 ||
		auditInteger(offboardingAudit.Metadata["revokedDesktopEnrollmentCount"]) != 1 {
		t.Fatalf("Desktop offboarding audit counts = %#v", offboardingAudit.Metadata)
	}

	active := "active"
	if _, err := service.UpdateTenantMember(
		context.Background(), principal, tenantID, memberID,
		UpdateTenantMemberInput{Status: &active}, "manual-member-reactivate", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, memberID).
		Take(&organizationMembership).Error; err != nil || organizationMembership.Status != "suspended" {
		t.Fatalf("reactivation silently restored Organization access: %#v %v", organizationMembership, err)
	}
	if err := db.Where("id = ?", credentialID).Take(&credential).Error; err != nil || credential.RevokedAt == nil {
		t.Fatalf("reactivation silently restored Credential: %#v %v", credential, err)
	}
	if err := db.Where("id = ?", desktopEnrollmentID).Take(&enrollment).Error; err != nil || enrollment.Status != "revoked" {
		t.Fatalf("reactivation silently restored Desktop Enrollment: %#v %v", enrollment, err)
	}
	if err := db.Where("id = ?", desktopDeviceID).Take(&device).Error; err != nil || device.Status != "active" {
		t.Fatalf("reactivation changed the user-global Desktop Device registration: %#v %v", device, err)
	}
}

func TestTenantMemberRemovalRevokesSessionBeforeRemovingMembership(t *testing.T) {
	db, tenantID, organizationID, ownerID := setupTenantMemberLifecycleTest(t)
	memberID := createTenantLifecycleMember(t, db, tenantID, organizationID, "remove@example.com")
	now := time.Now().UTC()
	sessionID := uuid.New()
	if err := db.Create(&persistence.LoginSession{
		ID: sessionID, UserID: memberID, ActiveTenantID: &tenantID, AuthMethod: "local",
		RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	principal := identity.Principal{UserID: ownerID, ActiveTenantID: &tenantID}
	if err := service.RemoveTenantMember(
		context.Background(), principal, tenantID, memberID, "manual-member-remove", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	var membershipCount int64
	if err := db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", tenantID, memberID).Count(&membershipCount).Error; err != nil {
		t.Fatal(err)
	}
	if membershipCount != 0 {
		t.Fatalf("Tenant Membership remained after removal: %d", membershipCount)
	}
	var session persistence.LoginSession
	if err := db.Where("id = ?", sessionID).Take(&session).Error; err != nil || session.RevokedAt == nil {
		t.Fatalf("removed member Login Session was not revoked: %#v %v", session, err)
	}
}

func TestJoinerMoverLeaverCredentialLifecycleHasCompleteAuditChain(t *testing.T) {
	db, tenantID, organizationID, ownerID := setupTenantMemberLifecycleTest(t)
	now := time.Now().UTC()
	joinerID := uuid.New()
	joinerEmail := "joiner-mover-leaver@example.com"
	if err := db.Create(&persistence.User{
		ID: joinerID, Email: joinerEmail, DisplayName: "Joiner Mover Leaver",
		Status: "active", EmailVerifiedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	owner := identity.Principal{UserID: ownerID, ActiveTenantID: &tenantID}
	invitation, err := service.InviteTenantMember(
		context.Background(), owner, tenantID,
		InviteTenantMemberInput{Email: joinerEmail, Role: "admin"},
		"member-joiner-invite", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.AcceptInvitation(
		context.Background(), identity.Principal{UserID: joinerID, Email: joinerEmail}, invitation.Token,
		"member-joiner-accept", "127.0.0.1",
	)
	if err != nil || accepted.ID != tenantID || accepted.Role != "admin" {
		t.Fatalf("accepted Tenant invitation = %#v, %v", accepted, err)
	}
	var rootMembership persistence.OrganizationMembership
	if err := db.Where(
		"tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, joinerID,
	).Take(&rootMembership).Error; err != nil || rootMembership.Role != "admin" || rootMembership.Status != "active" {
		t.Fatalf("automatic root Organization grant = %#v, %v", rootMembership, err)
	}

	costRole := "cost_admin"
	updated, err := service.UpdateTenantMember(
		context.Background(), owner, tenantID, joinerID,
		UpdateTenantMemberInput{Role: &costRole}, "member-mover-role", "127.0.0.1",
	)
	if err != nil || updated.Role != costRole {
		t.Fatalf("moved Tenant member = %#v, %v", updated, err)
	}
	var rootMembershipCount int64
	if err := db.Model(&persistence.OrganizationMembership{}).
		Where("tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, joinerID).
		Count(&rootMembershipCount).Error; err != nil || rootMembershipCount != 0 {
		t.Fatalf("Tenant role downgrade retained automatic root Organization authority: count=%d err=%v", rootMembershipCount, err)
	}

	sessionID, credentialID := uuid.New(), uuid.New()
	for _, model := range []any{
		&persistence.LoginSession{
			ID: sessionID, UserID: joinerID, ActiveTenantID: &tenantID, AuthMethod: "local",
			RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
		},
		&persistence.ProviderCredential{
			ID: credentialID, TenantID: tenantID, Scope: "user", ScopeUserID: &joinerID,
			Name: "Joiner BYOK", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-payload"), EncryptedDataKey: []byte("encrypted-key"),
			KMSProvider: "local", KMSKeyID: "test", Version: 1, CreatedBy: joinerID, UpdatedBy: joinerID,
			AutoSelectEnabled: true,
		},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	suspended := "suspended"
	if _, err := service.UpdateTenantMember(
		context.Background(), owner, tenantID, joinerID,
		UpdateTenantMemberInput{Status: &suspended}, "member-leaver-suspend", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveTenantMember(
		context.Background(), owner, tenantID, joinerID, "member-leaver-remove", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}

	for _, expected := range []struct {
		requestID string
		action    string
		actorID   uuid.UUID
	}{
		{requestID: "member-joiner-invite", action: "tenant.member_invited", actorID: ownerID},
		{requestID: "member-joiner-accept", action: "tenant.invitation_accepted", actorID: joinerID},
		{requestID: "member-mover-role", action: "tenant.member_updated", actorID: ownerID},
		{requestID: "member-leaver-suspend", action: "tenant.member_updated", actorID: ownerID},
		{requestID: "member-leaver-remove", action: "tenant.member_removed", actorID: ownerID},
	} {
		var entry persistence.AuditLog
		if err := db.Where("tenant_id = ? AND request_id = ? AND action = ?", tenantID, expected.requestID, expected.action).
			Take(&entry).Error; err != nil {
			t.Fatalf("load %s Audit: %v", expected.requestID, err)
		}
		if entry.ActorID == nil || *entry.ActorID != expected.actorID || entry.ResourceID == nil {
			t.Fatalf("%s Audit identity = %#v", expected.requestID, entry)
		}
		if expected.requestID == "member-mover-role" &&
			(entry.Metadata["fromRole"] != "admin" || entry.Metadata["toRole"] != costRole) {
			t.Fatalf("mover Audit role delta = %#v", entry.Metadata)
		}
		if expected.requestID == "member-leaver-suspend" &&
			(auditInteger(entry.Metadata["revokedSessionCount"]) != 1 ||
				auditInteger(entry.Metadata["revokedCredentialCount"]) != 1) {
			t.Fatalf("leaver Audit revocation counts = %#v", entry.Metadata)
		}
	}
	var credential persistence.ProviderCredential
	if err := db.Where("id = ?", credentialID).Take(&credential).Error; err != nil || credential.RevokedAt == nil || credential.AutoSelectEnabled {
		t.Fatalf("leaver Credential remained usable: %#v, %v", credential, err)
	}
	var login persistence.LoginSession
	if err := db.Where("id = ?", sessionID).Take(&login).Error; err != nil || login.RevokedAt == nil {
		t.Fatalf("leaver Login Session remained usable: %#v, %v", login, err)
	}
}

func TestTenantRoleDowngradePreservesIndependentlyChangedRootRole(t *testing.T) {
	db, tenantID, organizationID, ownerID := setupTenantMemberLifecycleTest(t)
	memberID := createTenantLifecycleMember(t, db, tenantID, organizationID, "independent-root-role@example.com")
	if err := db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", tenantID, memberID).
		Update("role", "admin").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.OrganizationMembership{}).
		Where("tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, memberID).
		Update("role", "viewer").Error; err != nil {
		t.Fatal(err)
	}
	costRole := "cost_admin"
	if _, err := NewService(db).UpdateTenantMember(
		context.Background(), identity.Principal{UserID: ownerID, ActiveTenantID: &tenantID}, tenantID, memberID,
		UpdateTenantMemberInput{Role: &costRole}, "independent-root-role-move", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	var membership persistence.OrganizationMembership
	if err := db.Where(
		"tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, memberID,
	).Take(&membership).Error; err != nil || membership.Role != "viewer" {
		t.Fatalf("independently changed root Organization role = %#v, %v", membership, err)
	}
}

func auditInteger(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return -1
	}
}

func setupTenantMemberLifecycleTest(t *testing.T) (*gorm.DB, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	tenantID, organizationID, ownerID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	models := []any{
		&persistence.User{ID: ownerID, Email: "owner@example.com", DisplayName: "Owner", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "member-lifecycle", Name: "Member lifecycle", Status: "active", LifecycleVersion: 1, PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: ownerID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: ownerID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: ownerID},
		&persistence.OrganizationMembership{TenantID: tenantID, OrganizationID: organizationID, UserID: ownerID, Role: "owner", Status: "active"},
	}
	for _, model := range models {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, tenantID, organizationID, ownerID
}

func createTenantLifecycleMember(
	t *testing.T,
	db *gorm.DB,
	tenantID, organizationID uuid.UUID,
	email string,
) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	now := time.Now().UTC()
	models := []any{
		&persistence.User{ID: userID, Email: email, DisplayName: email, Status: "active", EmailVerifiedAt: &now},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "member", Status: "active", JoinedAt: &now},
		&persistence.OrganizationMembership{TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "member", Status: "active"},
	}
	for _, model := range models {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	return userID
}

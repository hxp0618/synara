package scim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
)

func TestSCIMProvisioningStaysTenantScopedAndProtectsFinalOwner(t *testing.T) {
	db, principal, ownerID := setupSCIMTest(t)
	service := NewService(db)
	active := true
	inactive := false
	created, err := service.CreateUser(context.Background(), principal, UserInput{
		ExternalID: "directory-user-1", UserName: "member@example.com", DisplayName: "Directory Member", Active: &active,
	}, "scim-user-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.MustParse(created.ID)
	group, err := service.CreateGroup(context.Background(), principal, GroupInput{
		ExternalID: "directory-group-1", DisplayName: "Engineering", Members: []Member{{Value: memberID.String()}},
	}, "scim-group-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(group.Members) != 1 || group.Members[0].Value != memberID.String() {
		t.Fatalf("unexpected SCIM group membership: %#v", group)
	}
	var membership persistence.TenantMembership
	if err := db.Where("tenant_id = ? AND user_id = ?", principal.TenantID, memberID).Take(&membership).Error; err != nil || membership.Role != "member" || membership.Status != "active" {
		t.Fatalf("unexpected provisioned membership: %#v %v", membership, err)
	}
	now := time.Now().UTC()
	sessionID, desktopSessionID, desktopDeviceID, desktopEnrollmentID, credentialID :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	desktopFamilyID := uuid.New()
	if err := db.Create(&persistence.LoginSession{
		ID: sessionID, UserID: memberID, ActiveTenantID: &principal.TenantID, AuthMethod: "local",
		RefreshTokenHash: []byte(uuid.NewString()), ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ProviderCredential{
		ID: credentialID, TenantID: principal.TenantID, Scope: "user", ScopeUserID: &memberID,
		Name: "Member BYOK", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted-payload"), EncryptedDataKey: []byte("encrypted-key"),
		KMSProvider: "local", KMSKeyID: "test", Version: 1, CreatedBy: memberID, UpdatedBy: memberID,
		AutoSelectEnabled: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.DesktopDevice{
		ID: desktopDeviceID, ControlPlaneOrigin: "https://control.example.com",
		UserID: memberID, DefaultTenantID: principal.TenantID,
		PublicKey: []byte(uuid.NewString()), PublicKeySHA256: []byte(uuid.NewString()),
		Platform: "darwin", AppVersion: "1.0.0", DeviceLabel: "SCIM Mac",
		Status: "active", Version: 1, LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.LoginSession{
		ID: desktopSessionID, UserID: memberID, ActiveTenantID: &principal.TenantID,
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
		TenantID: principal.TenantID, MembershipVersionSnapshot: "snapshot-v1",
		Reason: "Connect an approved Desktop device.", ExpiresAt: now.Add(time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	inactive = false
	if _, err := service.ReplaceUser(context.Background(), principal, memberID, UserInput{
		ExternalID: "directory-user-1", UserName: "member@example.com", DisplayName: "Directory Member", Active: &inactive,
	}, "scim-member-suspend", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var revokedSession persistence.LoginSession
	if err := db.Where("id = ?", sessionID).Take(&revokedSession).Error; err != nil || revokedSession.RevokedAt == nil {
		t.Fatalf("SCIM did not revoke the Login Session: %#v %v", revokedSession, err)
	}
	var revokedDesktopSession persistence.LoginSession
	if err := db.Where("id = ?", desktopSessionID).Take(&revokedDesktopSession).Error; err != nil || revokedDesktopSession.RevokedAt == nil {
		t.Fatalf("SCIM did not revoke the Desktop Session: %#v %v", revokedDesktopSession, err)
	}
	var revokedCredential persistence.ProviderCredential
	if err := db.Where("id = ?", credentialID).Take(&revokedCredential).Error; err != nil || revokedCredential.RevokedAt == nil || revokedCredential.AutoSelectEnabled {
		t.Fatalf("SCIM did not revoke the user Credential: %#v %v", revokedCredential, err)
	}
	var enrollment persistence.DesktopEnrollment
	if err := db.Where("id = ?", desktopEnrollmentID).Take(&enrollment).Error; err != nil ||
		enrollment.Status != "revoked" || enrollment.RevokedAt == nil || enrollment.Version != 2 {
		t.Fatalf("SCIM did not revoke the pending Desktop Enrollment: %#v %v", enrollment, err)
	}
	var device persistence.DesktopDevice
	if err := db.Where("id = ?", desktopDeviceID).Take(&device).Error; err != nil ||
		device.Status != "active" || device.RevokedAt != nil || device.Version != 1 {
		t.Fatalf("SCIM Tenant offboarding mutated the user-global Desktop Device: %#v %v", device, err)
	}
	var offboardingAudit persistence.AuditLog
	if err := db.Where("request_id = ?", "scim-member-suspend").Take(&offboardingAudit).Error; err != nil {
		t.Fatal(err)
	}
	if auditMetadataInt64(offboardingAudit.Metadata["revokedSessionCount"]) != 2 ||
		auditMetadataInt64(offboardingAudit.Metadata["revokedCredentialCount"]) != 1 ||
		auditMetadataInt64(offboardingAudit.Metadata["revokedDesktopEnrollmentCount"]) != 1 {
		t.Fatalf("SCIM Desktop offboarding audit counts = %#v", offboardingAudit.Metadata)
	}
	inactive = false
	_, err = service.ReplaceUser(context.Background(), principal, ownerID, UserInput{UserName: "owner@example.com", DisplayName: "Owner", Active: &inactive}, "scim-owner-suspend", "127.0.0.1")
	if problemCode(err) != "last_tenant_owner" {
		t.Fatalf("SCIM suspended the final Tenant Owner: %v", err)
	}
}

func setupSCIMTest(t *testing.T) (*gorm.DB, serviceaccounts.Principal, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	ownerID, tenantID, serviceAccountID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	for _, model := range []any{
		&persistence.User{ID: ownerID, Email: "owner@example.com", DisplayName: "Owner", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "scim-test", Name: "SCIM Test", Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: ownerID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: ownerID, Role: "owner", Status: "active", JoinedAt: &now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, serviceaccounts.Principal{ID: serviceAccountID, TenantID: tenantID, Name: "SCIM", Scopes: map[string]struct{}{"scim.read": {}, "scim.write": {}}}, ownerID
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

func auditMetadataInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return -1
	}
}

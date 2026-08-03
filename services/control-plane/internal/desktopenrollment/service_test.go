package desktopenrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type allowPlatformAdmin struct{}

func (allowPlatformAdmin) RequirePlatformAdmin(_ context.Context, principal identity.Principal) (string, error) {
	if principal.Audience == "desktop" {
		return "", problem.New(403, "platform_web_session_required", "Web session required.")
	}
	return "admin", nil
}

type serviceFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	now              time.Time
	operatorTenantID uuid.UUID
	customerTenantID uuid.UUID
	userID           uuid.UUID
	webSessionID     uuid.UUID
	principal        identity.Principal
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "desktop-enrollment-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	customerTenantID := uuid.New()
	if err := store.DB().Create(&persistence.Tenant{
		ID: customerTenantID, Slug: "desktop-" + strings.ToLower(uuid.NewString()[:8]),
		Name: "Desktop Customer", Status: "active", LifecycleVersion: 1,
		PlanCode: "enterprise", Region: "default", Settings: map[string]any{},
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: customerTenantID, UserID: domain.UserID, Role: "owner", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	webSessionID := uuid.New()
	_, tokenHash, err := secret.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.LoginSession{
		ID: webSessionID, UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
		AuthMethod: "local", Audience: "web", RefreshTokenHash: tokenHash,
		ExpiresAt: now.Add(24 * time.Hour), LastSeenAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), allowPlatformAdmin{}, Config{
		ControlPlaneOrigin: "https://control.example.com", EnrollmentTTL: 3 * time.Minute,
		SessionTTL: 30 * 24 * time.Hour, SessionIdleTTL: 7 * 24 * time.Hour,
	})
	service.now = func() time.Time { return now }
	principal := identity.Principal{
		UserID: domain.UserID, SessionID: webSessionID, ActiveTenantID: &domain.TenantID,
		Audience: "web", Email: "owner@synara.local", DisplayName: "Owner",
	}
	return &serviceFixture{
		ctx: ctx, db: store.DB(), service: service, now: now,
		operatorTenantID: domain.TenantID, customerTenantID: customerTenantID,
		userID: domain.UserID, webSessionID: webSessionID, principal: principal,
	}
}

func (f *serviceFixture) issue(t *testing.T) IssuedEnrollment {
	t.Helper()
	issued, err := f.service.Issue(f.ctx, f.principal, f.customerTenantID, IssueInput{
		SubjectUserID: f.userID, Mode: "connect_existing",
		Reason: "Connect the customer owner desktop.",
	}, "issue-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func signedRedemption(t *testing.T, issued IssuedEnrollment) RedeemInput {
	input, _ := signedRedemptionWithKey(t, issued)
	return input
}

func signedRedemptionWithKey(t *testing.T, issued IssuedEnrollment) (RedeemInput, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	input := RedeemInput{
		Version: 1, ControlPlaneOrigin: issued.Enrollment.ControlPlaneOrigin,
		Enrollment: issued.Handle, DevicePublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), Platform: "darwin",
		AppVersion: "0.6.3", DeviceLabel: "Owner MacBook",
	}
	input.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(RedemptionProofPayload(input))))
	return input, privateKey
}

func TestSelfEnrollmentRedeemsExactlyOnceAndIssuesDesktopAudience(t *testing.T) {
	fixture := newServiceFixture(t)
	issued := fixture.issue(t)
	input := signedRedemption(t, issued)

	redeemed, err := fixture.service.Redeem(
		fixture.ctx, input, "redeem-request", "127.0.0.1", "Synara Desktop test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if redeemed.Audience != "desktop" || redeemed.DefaultTenantID != fixture.customerTenantID ||
		redeemed.Credential == "" || redeemed.SessionID == uuid.Nil || redeemed.Device.ID == uuid.Nil {
		t.Fatalf("redeemed Desktop session = %#v", redeemed)
	}

	identityService := identity.NewService(fixture.db, 30*24*time.Hour, 7*24*time.Hour)
	principal, err := identityService.AuthenticateDesktop(fixture.ctx, redeemed.Credential)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Audience != "desktop" || principal.DesktopDeviceID == nil ||
		*principal.DesktopDeviceID != redeemed.Device.ID || principal.ActiveTenantID == nil ||
		*principal.ActiveTenantID != fixture.customerTenantID {
		t.Fatalf("Desktop principal = %#v", principal)
	}

	if _, err := fixture.service.Redeem(fixture.ctx, input, "replay-request", "127.0.0.1", "Synara Desktop test"); problemCode(err) != "desktop_enrollment_invalid" {
		t.Fatalf("replayed Enrollment error = %v", err)
	}
	var enrollment persistence.DesktopEnrollment
	if err := fixture.db.Where("id = ?", issued.Enrollment.ID).Take(&enrollment).Error; err != nil {
		t.Fatal(err)
	}
	if enrollment.Status != "redeemed" || enrollment.RedeemedDeviceID == nil || enrollment.Version != 2 {
		t.Fatalf("redeemed Enrollment model = %#v", enrollment)
	}

	var auditRows []persistence.AuditLog
	if err := fixture.db.Where("resource_id IN ?", []uuid.UUID{issued.Enrollment.ID, redeemed.Device.ID}).Find(&auditRows).Error; err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(auditRows)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{issued.Handle, redeemed.Credential, input.Nonce, input.DevicePublicKey, input.Proof} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("audit payload contains secret-bearing value: %s", string(payload))
		}
	}
}

func TestConcurrentRedemptionHasOneWinner(t *testing.T) {
	fixture := newServiceFixture(t)
	issued := fixture.issue(t)
	input := signedRedemption(t, issued)

	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := fixture.service.Redeem(fixture.ctx, input, uuid.NewString(), "127.0.0.1", "Synara Desktop test")
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	invalid := 0
	for err := range results {
		if err == nil {
			successes++
		} else if problemCode(err) == "desktop_enrollment_invalid" {
			invalid++
		} else {
			t.Fatalf("unexpected concurrent redemption error = %v", err)
		}
	}
	if successes != 1 || invalid != 1 {
		t.Fatalf("concurrent redemption successes=%d invalid=%d", successes, invalid)
	}
	var sessionCount int64
	if err := fixture.db.Model(&persistence.LoginSession{}).
		Where("audience = ? AND desktop_device_id IS NOT NULL", "desktop").Count(&sessionCount).Error; err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 {
		t.Fatalf("Desktop session count = %d, want 1", sessionCount)
	}
}

func TestDesktopSessionRotationAndOldCredentialReplayRevokeFamily(t *testing.T) {
	fixture := newServiceFixture(t)
	issued := fixture.issue(t)
	redeemInput, privateKey := signedRedemptionWithKey(t, issued)
	redeemed, err := fixture.service.Redeem(
		fixture.ctx, redeemInput, "redeem-request", "127.0.0.1", "Synara Desktop test",
	)
	if err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(fixture.db, 30*24*time.Hour, 7*24*time.Hour)
	principal, err := identityService.AuthenticateDesktop(fixture.ctx, redeemed.Credential)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	rotation := RotateInput{Nonce: nonceText}
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(
		RotationProofPayload(
			redeemInput.ControlPlaneOrigin, redeemed.Device.ID.String(), redeemed.SessionID.String(), nonceText,
		),
	)))
	rotated, err := fixture.service.Rotate(
		fixture.ctx, principal, rotation, "rotate-request", "127.0.0.1", "Synara Desktop test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Credential == "" || rotated.SessionID == redeemed.SessionID ||
		rotated.CredentialFamilyID != redeemed.CredentialFamilyID {
		t.Fatalf("rotated Desktop session = %#v", rotated)
	}
	if _, err := identityService.AuthenticateDesktop(fixture.ctx, rotated.Credential); err != nil {
		t.Fatalf("rotated credential was not active: %v", err)
	}

	if _, err := identityService.AuthenticateDesktopRequest(
		fixture.ctx, redeemed.Credential, "replay-request", "127.0.0.1",
	); problemCode(err) != "invalid_desktop_session" {
		t.Fatalf("old credential replay error = %v", err)
	}
	if _, err := identityService.AuthenticateDesktop(fixture.ctx, rotated.Credential); problemCode(err) != "invalid_desktop_session" {
		t.Fatalf("credential family remained active after replay: %v", err)
	}
	var replayAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("action = ? AND resource_id = ?", "desktop.device_credential_replay_detected", redeemed.Device.ID).
		Count(&replayAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if replayAuditCount != 1 {
		t.Fatalf("credential replay audit count = %d, want 1", replayAuditCount)
	}
}

func TestMembershipDriftAndExpiryFailClosed(t *testing.T) {
	t.Run("membership drift", func(t *testing.T) {
		fixture := newServiceFixture(t)
		issued := fixture.issue(t)
		input := signedRedemption(t, issued)
		if err := fixture.db.Model(&persistence.TenantMembership{}).
			Where("tenant_id = ? AND user_id = ?", fixture.customerTenantID, fixture.userID).
			Updates(map[string]any{"role": "admin", "updated_at": fixture.now.Add(time.Second)}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Redeem(fixture.ctx, input, "drift-request", "127.0.0.1", "test"); problemCode(err) != "desktop_enrollment_invalid" {
			t.Fatalf("Membership drift error = %v", err)
		}
		assertEnrollmentTerminal(t, fixture.db, issued.Enrollment.ID, "revoked", "membership_drift")
	})

	t.Run("expiry", func(t *testing.T) {
		fixture := newServiceFixture(t)
		issued := fixture.issue(t)
		input := signedRedemption(t, issued)
		fixture.service.now = func() time.Time { return fixture.now.Add(4 * time.Minute) }
		if _, err := fixture.service.Redeem(fixture.ctx, input, "expiry-request", "127.0.0.1", "test"); problemCode(err) != "desktop_enrollment_invalid" {
			t.Fatalf("expired Enrollment error = %v", err)
		}
		assertEnrollmentTerminal(t, fixture.db, issued.Enrollment.ID, "expired", "expired")
	})
}

func TestCrossUserAndMalformedProofNeverConsumeEnrollment(t *testing.T) {
	fixture := newServiceFixture(t)
	other := persistence.User{
		ID: uuid.New(), Email: "other@example.com", DisplayName: "Other User", Status: "active",
		EmailVerifiedAt: &fixture.now, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	if err := fixture.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.TenantMembership{
		TenantID: fixture.customerTenantID, UserID: other.ID, Role: "member", Status: "active",
		JoinedAt: &fixture.now, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.Issue(fixture.ctx, fixture.principal, fixture.customerTenantID, IssueInput{
		SubjectUserID: other.ID, Reason: "Connect another user's unmanaged desktop.",
	}, "cross-user-request", "127.0.0.1")
	if problemCode(err) != "desktop_enrollment_subject_authentication_required" {
		t.Fatalf("cross-user issuance error = %v", err)
	}
	var crossUserCount int64
	if err := fixture.db.Model(&persistence.DesktopEnrollment{}).Where("subject_user_id = ?", other.ID).Count(&crossUserCount).Error; err != nil {
		t.Fatal(err)
	}
	if crossUserCount != 0 {
		t.Fatalf("cross-user Enrollment count = %d, want 0", crossUserCount)
	}

	issued := fixture.issue(t)
	input := signedRedemption(t, issued)
	input.Proof = strings.Repeat("x", 86)
	if _, err := fixture.service.Redeem(fixture.ctx, input, "bad-proof-request", "127.0.0.1", "test"); problemCode(err) != "desktop_enrollment_invalid" {
		t.Fatalf("malformed proof error = %v", err)
	}
	var enrollment persistence.DesktopEnrollment
	if err := fixture.db.Where("id = ?", issued.Enrollment.ID).Take(&enrollment).Error; err != nil {
		t.Fatal(err)
	}
	if enrollment.Status != "pending" || enrollment.Version != 1 || enrollment.FailedAttempts != 0 {
		t.Fatalf("malformed proof mutated Enrollment = %#v", enrollment)
	}
}

func TestDesktopEnrollmentOperationsRejectWrongSessionAudienceBeforeStorage(t *testing.T) {
	fixture := newServiceFixture(t)
	desktopPrincipal := fixture.principal
	desktopPrincipal.Audience = "desktop"
	webPrincipal := fixture.principal

	if _, err := fixture.service.ListPlatform(
		fixture.ctx, desktopPrincipal, fixture.customerTenantID, "list-request",
	); problemCode(err) != "platform_web_session_required" {
		t.Fatalf("Desktop ListPlatform error = %v", err)
	}
	if _, err := fixture.service.Issue(
		fixture.ctx, desktopPrincipal, fixture.customerTenantID,
		IssueInput{SubjectUserID: fixture.userID, Reason: "Reject a Desktop-audience issuer."},
		"issue-request", "127.0.0.1",
	); problemCode(err) != "platform_web_session_required" {
		t.Fatalf("Desktop Issue error = %v", err)
	}
	if _, err := fixture.service.MarkOpened(
		fixture.ctx, desktopPrincipal, uuid.New(), VersionInput{ExpectedVersion: 1},
		"opened-request", "127.0.0.1",
	); problemCode(err) != "platform_web_session_required" {
		t.Fatalf("Desktop MarkOpened error = %v", err)
	}
	if _, err := fixture.service.RevokeEnrollment(
		fixture.ctx, desktopPrincipal, uuid.New(),
		RevokeInput{ExpectedVersion: 1, Reason: "Reject a Desktop-audience revocation."},
		"revoke-enrollment-request", "127.0.0.1",
	); problemCode(err) != "platform_web_session_required" {
		t.Fatalf("Desktop RevokeEnrollment error = %v", err)
	}
	if _, err := fixture.service.RevokeDevice(
		fixture.ctx, desktopPrincipal, uuid.New(),
		RevokeInput{ExpectedVersion: 1, Reason: "Reject a Desktop-audience device revocation."},
		"revoke-device-request", "127.0.0.1",
	); problemCode(err) != "platform_web_session_required" {
		t.Fatalf("Desktop RevokeDevice error = %v", err)
	}
	if _, err := fixture.service.Rotate(
		fixture.ctx, webPrincipal, RotateInput{}, "rotate-request", "127.0.0.1", "test",
	); problemCode(err) != "desktop_session_required" {
		t.Fatalf("Web Rotate error = %v", err)
	}
	if err := fixture.service.Disconnect(
		fixture.ctx, webPrincipal, "disconnect-request", "127.0.0.1",
	); problemCode(err) != "desktop_session_required" {
		t.Fatalf("Web Disconnect error = %v", err)
	}

	for modelName, model := range map[string]any{
		"Enrollment": &persistence.DesktopEnrollment{},
		"device":     &persistence.DesktopDevice{},
	} {
		var count int64
		if err := fixture.db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows after rejected audience operations = %d, want 0", modelName, count)
		}
	}
}

func TestPlatformDeviceRevocationInvalidatesDesktopCredential(t *testing.T) {
	fixture := newServiceFixture(t)
	issued := fixture.issue(t)
	redeemed, err := fixture.service.Redeem(
		fixture.ctx, signedRedemption(t, issued), "redeem-request", "127.0.0.1", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	device, err := fixture.service.RevokeDevice(fixture.ctx, fixture.principal, redeemed.Device.ID, RevokeInput{
		ExpectedVersion: redeemed.Device.Version,
		Reason:          "The customer requested immediate device revocation.",
	}, "revoke-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if device.Status != "revoked" || device.Version != redeemed.Device.Version+1 {
		t.Fatalf("revoked device = %#v", device)
	}
	identityService := identity.NewService(fixture.db, 30*24*time.Hour, 7*24*time.Hour)
	if _, err := identityService.AuthenticateDesktop(fixture.ctx, redeemed.Credential); problemCode(err) != "invalid_desktop_session" {
		t.Fatalf("revoked Desktop credential error = %v", err)
	}
}

func TestDesktopDisconnectRevokesDeviceAndCredentialFamily(t *testing.T) {
	fixture := newServiceFixture(t)
	issued := fixture.issue(t)
	redeemed, err := fixture.service.Redeem(
		fixture.ctx, signedRedemption(t, issued), "redeem-request", "127.0.0.1", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(fixture.db, 30*24*time.Hour, 7*24*time.Hour)
	principal, err := identityService.AuthenticateDesktop(fixture.ctx, redeemed.Credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Disconnect(
		fixture.ctx, principal, "disconnect-request", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := identityService.AuthenticateDesktop(fixture.ctx, redeemed.Credential); problemCode(err) != "invalid_desktop_session" {
		t.Fatalf("disconnected Desktop credential error = %v", err)
	}
	var device persistence.DesktopDevice
	if err := fixture.db.Where("id = ?", redeemed.Device.ID).Take(&device).Error; err != nil {
		t.Fatal(err)
	}
	if device.Status != "revoked" || device.RevokedAt == nil || device.RevocationReason == nil {
		t.Fatalf("disconnected device = %#v", device)
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("action = ? AND resource_id = ?", "desktop.device_disconnected", redeemed.Device.ID).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("Desktop disconnect audit count = %d, want 1", auditCount)
	}
}

func assertEnrollmentTerminal(t *testing.T, db *gorm.DB, id uuid.UUID, status, reason string) {
	t.Helper()
	var enrollment persistence.DesktopEnrollment
	if err := db.Where("id = ?", id).Take(&enrollment).Error; err != nil {
		t.Fatal(err)
	}
	if enrollment.Status != status || enrollment.TerminalReason == nil || *enrollment.TerminalReason != reason {
		t.Fatalf("terminal Enrollment = %#v", enrollment)
	}
}

func problemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

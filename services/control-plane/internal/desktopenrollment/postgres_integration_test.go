package desktopenrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/tenantuseraccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func newPostgresServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfileEnterprise)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, databaseURL, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	userID := uuid.New()
	operatorTenantID := uuid.New()
	customerTenantID := uuid.New()
	user := persistence.User{
		ID: userID, Email: "desktop-postgres-" + uuid.NewString() + "@example.com",
		DisplayName: "PostgreSQL Desktop Owner", Status: "active", EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	tenants := []persistence.Tenant{
		{
			ID: operatorTenantID, Slug: "operator-" + uuid.NewString(), Name: "Operator",
			Status: "active", LifecycleVersion: 1, PlanCode: "enterprise", Region: "default",
			Settings: map[string]any{}, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: customerTenantID, Slug: "customer-" + uuid.NewString(), Name: "Customer",
			Status: "active", LifecycleVersion: 1, PlanCode: "enterprise", Region: "default",
			Settings: map[string]any{}, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		},
	}
	memberships := []persistence.TenantMembership{
		{TenantID: operatorTenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now},
		{TenantID: customerTenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now},
	}
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		for index := range tenants {
			if err := tx.Create(&tenants[index]).Error; err != nil {
				return err
			}
			if err := tx.Create(&memberships[index]).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	webSessionID := uuid.New()
	_, tokenHash, err := secret.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.LoginSession{
		ID: webSessionID, UserID: userID, ActiveTenantID: &operatorTenantID,
		AuthMethod: "local", Audience: "web", RefreshTokenHash: tokenHash,
		ExpiresAt: now.Add(24 * time.Hour), LastSeenAt: now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), allowPlatformAdmin{}, Config{
		ControlPlaneOrigin: "https://control.example.com/control-plane",
		EnrollmentTTL:      3 * time.Minute, SessionTTL: 30 * 24 * time.Hour,
		SessionIdleTTL: 7 * 24 * time.Hour,
	})
	service.now = func() time.Time { return now }
	principal := identity.Principal{
		UserID: userID, SessionID: webSessionID, ActiveTenantID: &operatorTenantID,
		Audience: "web", Email: user.Email, DisplayName: user.DisplayName,
	}
	return &serviceFixture{
		ctx: ctx, db: store.DB(), service: service, now: now,
		operatorTenantID: operatorTenantID, customerTenantID: customerTenantID,
		userID: userID, webSessionID: webSessionID, principal: principal,
	}
}

func TestPostgresConcurrentRedemptionAndRotationReplay(t *testing.T) {
	fixture := newPostgresServiceFixture(t)
	issued := fixture.issue(t)
	redeemInput, privateKey := signedRedemptionWithKey(t, issued)

	var redeemWait sync.WaitGroup
	redeemResults := make(chan struct {
		session RedeemedSession
		err     error
	}, 2)
	for range 2 {
		redeemWait.Add(1)
		go func() {
			defer redeemWait.Done()
			session, err := fixture.service.Redeem(
				fixture.ctx, redeemInput, uuid.NewString(), "127.0.0.1", "Synara Desktop PostgreSQL test",
			)
			redeemResults <- struct {
				session RedeemedSession
				err     error
			}{session: session, err: err}
		}()
	}
	redeemWait.Wait()
	close(redeemResults)
	var redeemed RedeemedSession
	redeemSuccesses := 0
	redeemRejected := 0
	for result := range redeemResults {
		if result.err == nil {
			redeemSuccesses++
			redeemed = result.session
		} else if problemCode(result.err) == "desktop_enrollment_invalid" {
			redeemRejected++
		} else {
			t.Fatalf("unexpected PostgreSQL concurrent redemption error: %v", result.err)
		}
	}
	if redeemSuccesses != 1 || redeemRejected != 1 {
		t.Fatalf("PostgreSQL redemption successes=%d rejected=%d", redeemSuccesses, redeemRejected)
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

	var rotateWait sync.WaitGroup
	rotateResults := make(chan struct {
		session RotatedSession
		err     error
	}, 2)
	for range 2 {
		rotateWait.Add(1)
		go func() {
			defer rotateWait.Done()
			session, err := fixture.service.Rotate(
				fixture.ctx, principal, rotation, uuid.NewString(), "127.0.0.1", "Synara Desktop PostgreSQL test",
			)
			rotateResults <- struct {
				session RotatedSession
				err     error
			}{session: session, err: err}
		}()
	}
	rotateWait.Wait()
	close(rotateResults)
	rotateSuccesses := 0
	rotateRejected := 0
	var rotated RotatedSession
	for result := range rotateResults {
		if result.err == nil {
			rotateSuccesses++
			rotated = result.session
		} else if problemCode(result.err) == "invalid_desktop_session" {
			rotateRejected++
		} else {
			t.Fatalf("unexpected PostgreSQL concurrent rotation error: %v", result.err)
		}
	}
	if rotateSuccesses != 1 || rotateRejected != 1 {
		t.Fatalf("PostgreSQL rotation successes=%d rejected=%d", rotateSuccesses, rotateRejected)
	}
	if _, err := identityService.AuthenticateDesktop(fixture.ctx, rotated.Credential); problemCode(err) != "invalid_desktop_session" {
		t.Fatalf("concurrent replay did not revoke the PostgreSQL credential family: %v", err)
	}
}

func TestPostgresOffboardingTerminallyRevokesDesktopIdentity(t *testing.T) {
	fixture := newPostgresServiceFixture(t)
	remainingOwnerID := uuid.New()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: remainingOwnerID, Email: "desktop-remaining-owner-" + uuid.NewString() + "@example.com",
			DisplayName: "Remaining Owner", Status: "active", EmailVerifiedAt: &fixture.now,
			CreatedAt: fixture.now, UpdatedAt: fixture.now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: fixture.customerTenantID, UserID: remainingOwnerID, Role: "owner", Status: "active",
			JoinedAt: &fixture.now, CreatedAt: fixture.now, UpdatedAt: fixture.now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	connectedEnrollment := fixture.issue(t)
	connected, err := fixture.service.Redeem(
		fixture.ctx, signedRedemption(t, connectedEnrollment),
		"postgres-offboarding-connect", "127.0.0.1", "Synara Desktop PostgreSQL test",
	)
	if err != nil {
		t.Fatal(err)
	}
	pendingEnrollment := fixture.issue(t)
	pendingInput := signedRedemption(t, pendingEnrollment)

	offboardedAt := fixture.now.Add(time.Minute)
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if _, err := tenantuseraccess.Suspend(
			fixture.ctx, tx, fixture.customerTenantID, fixture.userID, fixture.userID, offboardedAt,
		); err != nil {
			return err
		}
		return tx.Model(&persistence.TenantMembership{}).
			Where("tenant_id = ? AND user_id = ?", fixture.customerTenantID, fixture.userID).
			Update("status", "suspended").Error
	}); err != nil {
		t.Fatal(err)
	}

	identityService := identity.NewService(fixture.db, 30*24*time.Hour, 7*24*time.Hour)
	if _, err := identityService.AuthenticateDesktop(fixture.ctx, connected.Credential); problemCode(err) != "invalid_desktop_session" {
		t.Fatalf("offboarded Desktop credential error = %v", err)
	}
	if err := fixture.db.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", fixture.customerTenantID, fixture.userID).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Redeem(
		fixture.ctx, pendingInput, "postgres-offboarding-old-enrollment", "127.0.0.1", "Synara Desktop PostgreSQL test",
	); problemCode(err) != "desktop_enrollment_invalid" {
		t.Fatalf("reactivation reused a pre-offboarding Enrollment: %v", err)
	}

	var enrollment persistence.DesktopEnrollment
	if err := fixture.db.Where("id = ?", pendingEnrollment.Enrollment.ID).Take(&enrollment).Error; err != nil {
		t.Fatal(err)
	}
	if enrollment.Status != "revoked" || enrollment.Version != 2 || enrollment.RevokedAt == nil ||
		enrollment.TerminalReason == nil || *enrollment.TerminalReason != "subject_offboarded" {
		t.Fatalf("PostgreSQL pending Enrollment after offboarding = %#v", enrollment)
	}
	var device persistence.DesktopDevice
	if err := fixture.db.Where("id = ?", connected.Device.ID).Take(&device).Error; err != nil {
		t.Fatal(err)
	}
	if device.Status != "active" || device.Version != connected.Device.Version || device.RevokedAt != nil {
		t.Fatalf("PostgreSQL Desktop Device after offboarding = %#v", device)
	}
}

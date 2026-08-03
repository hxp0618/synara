package desktopenrollment

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const (
	minimumEnrollmentTTL = time.Minute
	maximumEnrollmentTTL = 5 * time.Minute
)

var appVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,79}$`)

type PlatformAdminAuthorizer interface {
	RequirePlatformAdmin(context.Context, identity.Principal) (string, error)
}

type Config struct {
	ControlPlaneOrigin string
	EnrollmentTTL      time.Duration
	SessionTTL         time.Duration
	SessionIdleTTL     time.Duration
}

type Service struct {
	db       *gorm.DB
	platform PlatformAdminAuthorizer
	config   Config
	now      func() time.Time
}

type Subject struct {
	UserID           uuid.UUID `json:"userId"`
	Email            string    `json:"email"`
	DisplayName      string    `json:"displayName"`
	Role             string    `json:"role"`
	MembershipStatus string    `json:"membershipStatus"`
	SelfEnrollable   bool      `json:"selfEnrollable"`
}

type Enrollment struct {
	ID                 uuid.UUID  `json:"id"`
	Status             string     `json:"status"`
	Version            int64      `json:"version"`
	Mode               string     `json:"mode"`
	Authority          string     `json:"authority"`
	ControlPlaneOrigin string     `json:"controlPlaneOrigin"`
	IssuedByUserID     uuid.UUID  `json:"issuedByUserId"`
	IssuerEmail        string     `json:"issuerEmail"`
	SubjectUserID      uuid.UUID  `json:"subjectUserId"`
	SubjectEmail       string     `json:"subjectEmail"`
	SubjectDisplayName string     `json:"subjectDisplayName"`
	TenantID           uuid.UUID  `json:"tenantId"`
	OrganizationID     *uuid.UUID `json:"organizationId"`
	Reason             string     `json:"reason"`
	ExpiresAt          time.Time  `json:"expiresAt"`
	OpenedAt           *time.Time `json:"openedAt"`
	RedeemedAt         *time.Time `json:"redeemedAt"`
	RedeemedDeviceID   *uuid.UUID `json:"redeemedDeviceId"`
	RevokedAt          *time.Time `json:"revokedAt"`
	TerminalReason     *string    `json:"terminalReason"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

type Device struct {
	ID                    uuid.UUID  `json:"id"`
	UserID                uuid.UUID  `json:"userId"`
	UserEmail             string     `json:"userEmail"`
	UserDisplayName       string     `json:"userDisplayName"`
	DefaultTenantID       uuid.UUID  `json:"defaultTenantId"`
	DefaultOrganizationID *uuid.UUID `json:"defaultOrganizationId"`
	Platform              string     `json:"platform"`
	AppVersion            string     `json:"appVersion"`
	DeviceLabel           string     `json:"deviceLabel"`
	Status                string     `json:"status"`
	Version               int64      `json:"version"`
	LastSeenAt            time.Time  `json:"lastSeenAt"`
	RevokedAt             *time.Time `json:"revokedAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type PlatformAccess struct {
	ControlPlaneOrigin   string       `json:"controlPlaneOrigin"`
	EnrollmentTTLSeconds int          `json:"enrollmentTtlSeconds"`
	Subjects             []Subject    `json:"subjects"`
	Enrollments          []Enrollment `json:"enrollments"`
	Devices              []Device     `json:"devices"`
}

type IssueInput struct {
	SubjectUserID  uuid.UUID  `json:"subjectUserId"`
	OrganizationID *uuid.UUID `json:"organizationId"`
	Mode           string     `json:"mode"`
	Reason         string     `json:"reason"`
}

type IssuedEnrollment struct {
	Enrollment Enrollment `json:"enrollment"`
	Handle     string     `json:"handle"`
}

type VersionInput struct {
	ExpectedVersion int64 `json:"expectedVersion"`
}

type RevokeInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	Reason          string `json:"reason"`
}

type RedeemInput struct {
	Version            int    `json:"version"`
	ControlPlaneOrigin string `json:"controlPlaneOrigin"`
	Enrollment         string `json:"enrollment"`
	DevicePublicKey    string `json:"devicePublicKey"`
	Nonce              string `json:"nonce"`
	Proof              string `json:"proof"`
	Platform           string `json:"platform"`
	AppVersion         string `json:"appVersion"`
	DeviceLabel        string `json:"deviceLabel"`
}

type RedeemedSession struct {
	Audience              string     `json:"audience"`
	Credential            string     `json:"credential"`
	CredentialExpiresAt   time.Time  `json:"credentialExpiresAt"`
	SessionID             uuid.UUID  `json:"sessionId"`
	CredentialFamilyID    uuid.UUID  `json:"credentialFamilyId"`
	Device                Device     `json:"device"`
	DefaultTenantID       uuid.UUID  `json:"defaultTenantId"`
	DefaultOrganizationID *uuid.UUID `json:"defaultOrganizationId"`
}

func NewService(db *gorm.DB, platform PlatformAdminAuthorizer, config Config) *Service {
	config.ControlPlaneOrigin = strings.TrimRight(strings.TrimSpace(config.ControlPlaneOrigin), "/")
	return &Service{
		db: db, platform: platform, config: config,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) ListPlatform(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	requestID string,
) (PlatformAccess, error) {
	if err := s.requirePlatformAdmin(ctx, principal); err != nil {
		return PlatformAccess{}, err
	}
	if tenantID == uuid.Nil {
		return PlatformAccess{}, problem.New(400, "invalid_tenant_id", "A customer Tenant is required.")
	}
	if err := s.expirePending(ctx, tenantID, requestID); err != nil {
		return PlatformAccess{}, err
	}

	var subjects []Subject
	if err := s.db.WithContext(ctx).Table("tenant_memberships AS membership").
		Select(`membership.user_id, user.email, user.display_name, membership.role,
			membership.status AS membership_status, membership.user_id = ? AS self_enrollable`, principal.UserID).
		Joins("JOIN users AS user ON user.id = membership.user_id").
		Where("membership.tenant_id = ? AND user.deleted_at IS NULL", tenantID).
		Order("LOWER(user.email), membership.user_id").Scan(&subjects).Error; err != nil {
		return PlatformAccess{}, problem.Wrap(500, "desktop_access_subjects_load_failed", "Desktop access subjects could not be loaded.", err)
	}

	var enrollmentRows []enrollmentRow
	if err := enrollmentViewQuery(ctx, s.db).
		Where("enrollment.tenant_id = ?", tenantID).
		Order("enrollment.created_at DESC, enrollment.id").Scan(&enrollmentRows).Error; err != nil {
		return PlatformAccess{}, problem.Wrap(500, "desktop_enrollments_load_failed", "Desktop Enrollments could not be loaded.", err)
	}

	var deviceRows []deviceRow
	if err := deviceViewQuery(ctx, s.db).
		Where("device.default_tenant_id = ?", tenantID).
		Order("device.last_seen_at DESC, device.id").Scan(&deviceRows).Error; err != nil {
		return PlatformAccess{}, problem.Wrap(500, "desktop_devices_load_failed", "Desktop devices could not be loaded.", err)
	}

	return PlatformAccess{
		ControlPlaneOrigin: s.config.ControlPlaneOrigin, EnrollmentTTLSeconds: int(s.config.EnrollmentTTL.Seconds()),
		Subjects: subjects, Enrollments: enrollmentViews(enrollmentRows), Devices: deviceViews(deviceRows),
	}, nil
}

func (s *Service) Issue(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input IssueInput,
	requestID, ipAddress string,
) (IssuedEnrollment, error) {
	if err := s.requirePlatformAdmin(ctx, principal); err != nil {
		return IssuedEnrollment{}, err
	}
	if s.config.ControlPlaneOrigin == "" {
		return IssuedEnrollment{}, problem.New(503, "desktop_enrollment_unavailable", "Desktop Enrollment requires SYNARA_PUBLIC_CONTROL_PLANE_URL.")
	}
	if s.config.EnrollmentTTL < minimumEnrollmentTTL || s.config.EnrollmentTTL > maximumEnrollmentTTL || s.config.SessionTTL <= 0 {
		return IssuedEnrollment{}, problem.New(503, "desktop_enrollment_unavailable", "Desktop Enrollment timing is not configured safely.")
	}
	if tenantID == uuid.Nil || input.SubjectUserID == uuid.Nil {
		return IssuedEnrollment{}, problem.New(400, "invalid_desktop_enrollment_subject", "A customer Tenant and subject user are required.")
	}
	if input.SubjectUserID != principal.UserID {
		return IssuedEnrollment{}, problem.New(409, "desktop_enrollment_subject_authentication_required", "This subject must authenticate with the configured identity provider before Desktop access can be issued.")
	}
	mode := strings.TrimSpace(input.Mode)
	if mode == "" {
		mode = "connect_existing"
	}
	if mode != "connect_existing" && mode != "provisioned_then_connect" {
		return IssuedEnrollment{}, problem.New(400, "invalid_desktop_enrollment_mode", "Desktop Enrollment mode is invalid.")
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return IssuedEnrollment{}, err
	}
	handle, handleHash, err := secret.NewToken()
	if err != nil {
		return IssuedEnrollment{}, problem.Wrap(500, "desktop_enrollment_issue_failed", "Desktop Enrollment could not be created.", err)
	}
	now := s.now()
	model := persistence.DesktopEnrollment{
		ID: uuid.New(), SecretHash: handleHash, Status: "pending", Version: 1,
		Mode: mode, Authority: "self", ControlPlaneOrigin: s.config.ControlPlaneOrigin,
		IssuedByUserID: principal.UserID, IssuedBySessionID: principal.SessionID,
		SubjectUserID: input.SubjectUserID, TenantID: tenantID, OrganizationID: input.OrganizationID,
		Reason: reason, ExpiresAt: now.Add(s.config.EnrollmentTTL), CreatedAt: now, UpdatedAt: now,
	}
	var subject persistence.User
	var membership persistence.TenantMembership
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireIssuingSession(ctx, tx, principal, now, s.config.SessionIdleTTL); err != nil {
			return err
		}
		if err := requireEnrollableTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("id = ? AND status = ? AND deleted_at IS NULL", input.SubjectUserID, "active").
			Take(&subject).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "desktop_enrollment_subject_unavailable", "The selected Desktop subject is unavailable.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_enrollment_subject_load_failed", "The selected Desktop subject could not be validated.", err)
		}
		if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, input.SubjectUserID, "active").
			Take(&membership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "desktop_enrollment_membership_required", "The subject needs an active Tenant Membership before Desktop Enrollment.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_enrollment_membership_load_failed", "The subject Membership could not be validated.", err)
		}
		if err := requireOrganizationMembership(ctx, tx, tenantID, input.SubjectUserID, input.OrganizationID); err != nil {
			return err
		}
		model.MembershipVersionSnapshot = membershipSnapshot(membership)
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "desktop_enrollment_issue_conflict", "Desktop Enrollment could not be created.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "desktop.enrollment_created", ResourceType: "desktop_enrollment", ResourceID: &model.ID,
			OrganizationID: input.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"subjectUserId": input.SubjectUserID, "mode": mode, "authority": "self",
				"expiresAt": model.ExpiresAt, "reason": reason,
			},
		})
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return IssuedEnrollment{}, err
	}
	view := enrollmentView(enrollmentRow{
		DesktopEnrollment: model, IssuerEmail: principal.Email,
		SubjectEmail: subject.Email, SubjectDisplayName: subject.DisplayName,
	})
	return IssuedEnrollment{Enrollment: view, Handle: handle}, nil
}

func (s *Service) MarkOpened(
	ctx context.Context,
	principal identity.Principal,
	enrollmentID uuid.UUID,
	input VersionInput,
	requestID, ipAddress string,
) (Enrollment, error) {
	if err := s.requirePlatformAdmin(ctx, principal); err != nil {
		return Enrollment{}, err
	}
	now := s.now()
	var model persistence.DesktopEnrollment
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", enrollmentID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "desktop_enrollment_not_found", "Desktop Enrollment not found.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_enrollment_load_failed", "Desktop Enrollment could not be loaded.", err)
		}
		if model.Status != "pending" || model.Version != input.ExpectedVersion || !model.ExpiresAt.After(now) {
			return problem.New(409, "desktop_enrollment_version_conflict", "Desktop Enrollment changed or expired; create a new Enrollment.")
		}
		updated := tx.WithContext(ctx).Model(&persistence.DesktopEnrollment{}).
			Where("id = ? AND status = ? AND version = ?", model.ID, "pending", model.Version).
			Updates(map[string]any{"opened_at": now, "version": model.Version + 1})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "desktop_enrollment_version_conflict", "Desktop Enrollment changed; reload it before opening.", updated.Error)
		}
		model.OpenedAt = &now
		model.Version++
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: model.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "desktop.enrollment_opened", ResourceType: "desktop_enrollment", ResourceID: &model.ID,
			OrganizationID: model.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"subjectUserId": model.SubjectUserID, "version": model.Version},
		})
	})
	if err != nil {
		return Enrollment{}, err
	}
	return s.getEnrollment(ctx, model.ID)
}

func (s *Service) RevokeEnrollment(
	ctx context.Context,
	principal identity.Principal,
	enrollmentID uuid.UUID,
	input RevokeInput,
	requestID, ipAddress string,
) (Enrollment, error) {
	if err := s.requirePlatformAdmin(ctx, principal); err != nil {
		return Enrollment{}, err
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Enrollment{}, err
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.DesktopEnrollment
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", enrollmentID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "desktop_enrollment_not_found", "Desktop Enrollment not found.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_enrollment_load_failed", "Desktop Enrollment could not be loaded.", err)
		}
		if model.Status != "pending" || model.Version != input.ExpectedVersion {
			return problem.New(409, "desktop_enrollment_version_conflict", "Desktop Enrollment changed; reload it before revoking.")
		}
		updated := tx.WithContext(ctx).Model(&persistence.DesktopEnrollment{}).
			Where("id = ? AND status = ? AND version = ?", model.ID, "pending", model.Version).
			Updates(map[string]any{
				"status": "revoked", "version": model.Version + 1, "revoked_by": principal.UserID,
				"revoked_at": now, "terminal_reason": reason,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "desktop_enrollment_version_conflict", "Desktop Enrollment changed; reload it before revoking.", updated.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: model.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "desktop.enrollment_revoked", ResourceType: "desktop_enrollment", ResourceID: &model.ID,
			OrganizationID: model.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"subjectUserId": model.SubjectUserID, "reason": reason},
		})
	})
	if err != nil {
		return Enrollment{}, err
	}
	return s.getEnrollment(ctx, enrollmentID)
}

func (s *Service) RevokeDevice(
	ctx context.Context,
	principal identity.Principal,
	deviceID uuid.UUID,
	input RevokeInput,
	requestID, ipAddress string,
) (Device, error) {
	if err := s.requirePlatformAdmin(ctx, principal); err != nil {
		return Device{}, err
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Device{}, err
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.DesktopDevice
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", deviceID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "desktop_device_not_found", "Desktop device not found.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_device_load_failed", "Desktop device could not be loaded.", err)
		}
		if model.Status != "active" || model.Version != input.ExpectedVersion {
			return problem.New(409, "desktop_device_version_conflict", "Desktop device changed; reload it before revoking.")
		}
		updated := tx.WithContext(ctx).Model(&persistence.DesktopDevice{}).
			Where("id = ? AND status = ? AND version = ?", model.ID, "active", model.Version).
			Updates(map[string]any{
				"status": "revoked", "version": model.Version + 1, "revoked_by": principal.UserID,
				"revocation_reason": reason, "revoked_at": now,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "desktop_device_version_conflict", "Desktop device changed; reload it before revoking.", updated.Error)
		}
		if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
			Where("desktop_device_id = ? AND audience = ? AND revoked_at IS NULL", model.ID, "desktop").
			Update("revoked_at", now).Error; err != nil {
			return problem.Wrap(500, "desktop_device_session_revoke_failed", "Desktop device sessions could not be revoked.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: model.DefaultTenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "desktop.device_revoked", ResourceType: "desktop_device", ResourceID: &model.ID,
			OrganizationID: model.DefaultOrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"subjectUserId": model.UserID, "reason": reason},
		})
	})
	if err != nil {
		return Device{}, err
	}
	return s.getDevice(ctx, deviceID)
}

func (s *Service) Redeem(
	ctx context.Context,
	input RedeemInput,
	requestID, ipAddress, userAgent string,
) (RedeemedSession, error) {
	normalized, proof, err := s.validateRedemptionInput(input)
	if err != nil {
		return RedeemedSession{}, err
	}
	handleHash := secret.HashToken(normalized.Enrollment)
	now := s.now()
	var result RedeemedSession
	var terminalFailure bool
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var enrollment persistence.DesktopEnrollment
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("secret_hash = ?", handleHash).Take(&enrollment).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			terminalFailure = true
			return nil
		}
		if loadErr != nil {
			return problem.Wrap(500, "desktop_enrollment_redeem_failed", "Desktop Enrollment could not be redeemed.", loadErr)
		}
		if enrollment.Status != "pending" {
			terminalFailure = true
			return nil
		}
		if !enrollment.ExpiresAt.After(now) {
			if err := s.failEnrollment(ctx, tx, enrollment, now, "expired", requestID, ipAddress); err != nil {
				return err
			}
			terminalFailure = true
			return nil
		}
		failureClass, validationErr := s.revalidateEnrollment(ctx, tx, enrollment, now)
		if validationErr != nil {
			return validationErr
		}
		if failureClass != "" {
			if err := s.failEnrollment(ctx, tx, enrollment, now, failureClass, requestID, ipAddress); err != nil {
				return err
			}
			terminalFailure = true
			return nil
		}

		device, err := s.connectDevice(ctx, tx, enrollment, normalized, proof, now, requestID, ipAddress)
		if err != nil {
			return err
		}
		if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
			Where("desktop_device_id = ? AND audience = ? AND revoked_at IS NULL", device.ID, "desktop").
			Update("revoked_at", now).Error; err != nil {
			return problem.Wrap(500, "desktop_session_supersede_failed", "The previous Desktop session could not be replaced.", err)
		}
		credential, credentialHash, err := secret.NewToken()
		if err != nil {
			return problem.Wrap(500, "desktop_session_issue_failed", "The Desktop session could not be issued.", err)
		}
		sessionID := uuid.New()
		familyID := uuid.New()
		expiresAt := now.Add(s.config.SessionTTL)
		if err := tx.WithContext(ctx).Create(&persistence.LoginSession{
			ID: sessionID, UserID: enrollment.SubjectUserID, ActiveTenantID: &enrollment.TenantID,
			AuthMethod: "desktop", Audience: "desktop", DesktopDeviceID: &device.ID,
			CredentialFamilyID: &familyID, RefreshTokenHash: credentialHash,
			IPAddress: optionalString(ipAddress), UserAgent: optionalString(userAgent),
			ExpiresAt: expiresAt, LastSeenAt: now, CreatedAt: now,
		}).Error; err != nil {
			return problem.Wrap(500, "desktop_session_issue_failed", "The Desktop session could not be issued.", err)
		}
		updated := tx.WithContext(ctx).Model(&persistence.DesktopEnrollment{}).
			Where("id = ? AND status = ? AND version = ?", enrollment.ID, "pending", enrollment.Version).
			Updates(map[string]any{
				"status": "redeemed", "version": enrollment.Version + 1, "redeemed_at": now,
				"redeemed_device_id": device.ID, "redeemed_nonce_hash": proof.NonceSHA256,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "desktop_enrollment_redeem_conflict", "Desktop Enrollment could not be redeemed.", updated.Error)
		}
		for _, entry := range []audit.Entry{
			{
				TenantID: enrollment.TenantID, ActorType: "user", ActorID: &enrollment.SubjectUserID,
				Action: "desktop.enrollment_redeemed", ResourceType: "desktop_enrollment", ResourceID: &enrollment.ID,
				OrganizationID: enrollment.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
				Metadata: map[string]any{"deviceId": device.ID, "platform": normalized.Platform, "appVersion": normalized.AppVersion},
			},
			{
				TenantID: enrollment.TenantID, ActorType: "user", ActorID: &enrollment.SubjectUserID,
				Action: "desktop.device_session_issued", ResourceType: "desktop_device", ResourceID: &device.ID,
				OrganizationID: enrollment.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
				Metadata: map[string]any{"sessionId": sessionID, "credentialFamilyId": familyID, "audience": "desktop"},
			},
		} {
			if err := audit.Record(ctx, tx, entry); err != nil {
				return err
			}
		}
		result = RedeemedSession{
			Audience: "desktop", Credential: credential, CredentialExpiresAt: expiresAt,
			SessionID: sessionID, CredentialFamilyID: familyID,
			Device:          deviceView(deviceRow{DesktopDevice: device}),
			DefaultTenantID: enrollment.TenantID, DefaultOrganizationID: enrollment.OrganizationID,
		}
		return nil
		// The Enrollment row is the serialization authority. READ COMMITTED lets a waiter that
		// acquires FOR UPDATE after the winner commit observe the terminal status and return the
		// frozen non-enumerating failure instead of leaking PostgreSQL SQLSTATE 40001.
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return RedeemedSession{}, err
	}
	if terminalFailure {
		return RedeemedSession{}, invalidEnrollment()
	}
	return result, nil
}

func (s *Service) validateRedemptionInput(input RedeemInput) (RedeemInput, verifiedRedemptionProof, error) {
	input.ControlPlaneOrigin = strings.TrimRight(strings.TrimSpace(input.ControlPlaneOrigin), "/")
	input.Enrollment = strings.TrimSpace(input.Enrollment)
	input.DevicePublicKey = strings.TrimSpace(input.DevicePublicKey)
	input.Nonce = strings.TrimSpace(input.Nonce)
	input.Proof = strings.TrimSpace(input.Proof)
	input.Platform = strings.TrimSpace(input.Platform)
	input.AppVersion = strings.TrimSpace(input.AppVersion)
	input.DeviceLabel = strings.TrimSpace(input.DeviceLabel)
	if input.Version != 1 || input.ControlPlaneOrigin != s.config.ControlPlaneOrigin || !validControlPlaneBaseURL(input.ControlPlaneOrigin) {
		return RedeemInput{}, verifiedRedemptionProof{}, invalidEnrollment()
	}
	decodedHandle, decodeErr := base64.RawURLEncoding.DecodeString(input.Enrollment)
	if decodeErr != nil || len(decodedHandle) != 32 {
		return RedeemInput{}, verifiedRedemptionProof{}, invalidEnrollment()
	}
	if input.Platform != "darwin" && input.Platform != "win32" && input.Platform != "linux" {
		return RedeemInput{}, verifiedRedemptionProof{}, problem.New(400, "invalid_desktop_platform", "Desktop platform is invalid.")
	}
	if !appVersionPattern.MatchString(input.AppVersion) {
		return RedeemInput{}, verifiedRedemptionProof{}, problem.New(400, "invalid_desktop_app_version", "Desktop application version is invalid.")
	}
	if len(input.DeviceLabel) < 1 || len(input.DeviceLabel) > 160 || strings.ContainsAny(input.DeviceLabel, "\r\n\x00") {
		return RedeemInput{}, verifiedRedemptionProof{}, problem.New(400, "invalid_desktop_device_label", "Desktop device label is invalid.")
	}
	proof, ok := verifyRedemptionProof(input)
	if !ok {
		return RedeemInput{}, verifiedRedemptionProof{}, invalidEnrollment()
	}
	return input, proof, nil
}

func (s *Service) revalidateEnrollment(
	ctx context.Context,
	tx *gorm.DB,
	enrollment persistence.DesktopEnrollment,
	now time.Time,
) (string, error) {
	var issuerSession persistence.LoginSession
	if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("id = ? AND user_id = ? AND audience = ? AND revoked_at IS NULL AND expires_at > ? AND last_seen_at > ?",
			enrollment.IssuedBySessionID, enrollment.IssuedByUserID, "web", now, now.Add(-s.config.SessionIdleTTL)).
		Take(&issuerSession).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return "issuer_session_invalid", nil
	} else if err != nil {
		return "", problem.Wrap(500, "desktop_enrollment_issuer_check_failed", "Desktop Enrollment could not be redeemed.", err)
	}
	if enrollment.Authority != "self" || enrollment.IssuedByUserID != enrollment.SubjectUserID {
		return "authority_invalid", nil
	}
	var subject persistence.User
	if err := tx.WithContext(ctx).Where("id = ? AND status = ? AND deleted_at IS NULL", enrollment.SubjectUserID, "active").Take(&subject).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return "subject_unavailable", nil
	} else if err != nil {
		return "", problem.Wrap(500, "desktop_enrollment_subject_check_failed", "Desktop Enrollment could not be redeemed.", err)
	}
	if err := requireEnrollableTenant(ctx, tx, enrollment.TenantID); err != nil {
		var apiError *problem.Error
		if errors.As(err, &apiError) && apiError.Status < 500 {
			return "tenant_unavailable", nil
		}
		return "", err
	}
	var membership persistence.TenantMembership
	if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND user_id = ? AND status = ?", enrollment.TenantID, enrollment.SubjectUserID, "active").
		Take(&membership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return "membership_unavailable", nil
	} else if err != nil {
		return "", problem.Wrap(500, "desktop_enrollment_membership_check_failed", "Desktop Enrollment could not be redeemed.", err)
	}
	if membershipSnapshot(membership) != enrollment.MembershipVersionSnapshot {
		return "membership_drift", nil
	}
	if err := requireOrganizationMembership(ctx, tx, enrollment.TenantID, enrollment.SubjectUserID, enrollment.OrganizationID); err != nil {
		var apiError *problem.Error
		if errors.As(err, &apiError) && apiError.Status < 500 {
			return "organization_unavailable", nil
		}
		return "", err
	}
	return "", nil
}

func (s *Service) connectDevice(
	ctx context.Context,
	tx *gorm.DB,
	enrollment persistence.DesktopEnrollment,
	input RedeemInput,
	proof verifiedRedemptionProof,
	now time.Time,
	requestID, ipAddress string,
) (persistence.DesktopDevice, error) {
	var device persistence.DesktopDevice
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("control_plane_origin = ? AND user_id = ? AND public_key_sha256 = ?",
			enrollment.ControlPlaneOrigin, enrollment.SubjectUserID, proof.PublicKeySHA256).
		Take(&device).Error
	if err == nil {
		if device.Status != "active" || !bytes.Equal(device.PublicKey, proof.PublicKey) {
			return persistence.DesktopDevice{}, invalidEnrollment()
		}
		updated := tx.WithContext(ctx).Model(&persistence.DesktopDevice{}).
			Where("id = ? AND status = ? AND version = ?", device.ID, "active", device.Version).
			Updates(map[string]any{
				"default_tenant_id": enrollment.TenantID, "default_organization_id": enrollment.OrganizationID,
				"platform": input.Platform, "app_version": input.AppVersion, "device_label": input.DeviceLabel,
				"last_seen_at": now, "version": device.Version + 1,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return persistence.DesktopDevice{}, problem.Wrap(409, "desktop_device_connect_conflict", "Desktop device changed during connection.", updated.Error)
		}
		device.DefaultTenantID = enrollment.TenantID
		device.DefaultOrganizationID = enrollment.OrganizationID
		device.Platform = input.Platform
		device.AppVersion = input.AppVersion
		device.DeviceLabel = input.DeviceLabel
		device.LastSeenAt = now
		device.Version++
		return device, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.DesktopDevice{}, problem.Wrap(500, "desktop_device_load_failed", "Desktop device could not be connected.", err)
	}
	device = persistence.DesktopDevice{
		ID: uuid.New(), ControlPlaneOrigin: enrollment.ControlPlaneOrigin,
		UserID: enrollment.SubjectUserID, DefaultTenantID: enrollment.TenantID,
		DefaultOrganizationID: enrollment.OrganizationID,
		PublicKey:             append([]byte(nil), proof.PublicKey...), PublicKeySHA256: append([]byte(nil), proof.PublicKeySHA256...),
		Platform: input.Platform, AppVersion: input.AppVersion, DeviceLabel: input.DeviceLabel,
		Status: "active", Version: 1, LastSeenAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.WithContext(ctx).Create(&device).Error; err != nil {
		return persistence.DesktopDevice{}, problem.Wrap(409, "desktop_device_connect_conflict", "Desktop device could not be connected.", err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: enrollment.TenantID, ActorType: "user", ActorID: &enrollment.SubjectUserID,
		Action: "desktop.device_connected", ResourceType: "desktop_device", ResourceID: &device.ID,
		OrganizationID: enrollment.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{"platform": input.Platform, "appVersion": input.AppVersion},
	}); err != nil {
		return persistence.DesktopDevice{}, err
	}
	return device, nil
}

func (s *Service) failEnrollment(
	ctx context.Context,
	tx *gorm.DB,
	enrollment persistence.DesktopEnrollment,
	now time.Time,
	failureClass, requestID, ipAddress string,
) error {
	status := "revoked"
	action := "desktop.enrollment_failed"
	updates := map[string]any{
		"status": status, "version": enrollment.Version + 1, "terminal_reason": failureClass,
		"failed_attempts": enrollment.FailedAttempts + 1, "last_failure_at": now,
	}
	if failureClass == "expired" {
		status = "expired"
		action = "desktop.enrollment_expired"
		updates["status"] = status
	} else {
		updates["revoked_at"] = now
	}
	updated := tx.WithContext(ctx).Model(&persistence.DesktopEnrollment{}).
		Where("id = ? AND status = ? AND version = ?", enrollment.ID, "pending", enrollment.Version).
		Updates(updates)
	if updated.Error != nil || updated.RowsAffected != 1 {
		return problem.Wrap(409, "desktop_enrollment_redeem_conflict", "Desktop Enrollment could not be redeemed.", updated.Error)
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: enrollment.TenantID, ActorType: "system",
		Action: action, ResourceType: "desktop_enrollment", ResourceID: &enrollment.ID,
		OrganizationID: enrollment.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{"subjectUserId": enrollment.SubjectUserID, "failureClass": failureClass},
	})
}

func (s *Service) expirePending(ctx context.Context, tenantID uuid.UUID, requestID string) error {
	now := s.now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pending []persistence.DesktopEnrollment
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
			Where("tenant_id = ? AND status = ? AND expires_at <= ?", tenantID, "pending", now).
			Order("expires_at, id").Find(&pending).Error; err != nil {
			return problem.Wrap(500, "desktop_enrollment_expiry_failed", "Expired Desktop Enrollments could not be reconciled.", err)
		}
		for _, enrollment := range pending {
			if err := s.failEnrollment(ctx, tx, enrollment, now, "expired", requestID, ""); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) requirePlatformAdmin(ctx context.Context, principal identity.Principal) error {
	if principal.Audience == "desktop" || principal.SupportAccessGrantID != nil {
		return problem.New(403, "platform_web_session_required", "Desktop Enrollment administration requires a Web session without Support Access.")
	}
	if s.platform == nil {
		return problem.New(503, "desktop_enrollment_unavailable", "Desktop Enrollment administration is unavailable.")
	}
	_, err := s.platform.RequirePlatformAdmin(ctx, principal)
	return err
}

func requireIssuingSession(
	ctx context.Context,
	tx *gorm.DB,
	principal identity.Principal,
	now time.Time,
	idleTTL time.Duration,
) error {
	if idleTTL <= 0 {
		return problem.New(503, "desktop_enrollment_unavailable", "Desktop Enrollment session policy is unavailable.")
	}
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
		Where("id = ? AND user_id = ? AND audience = ? AND revoked_at IS NULL AND expires_at > ? AND last_seen_at > ?",
			principal.SessionID, principal.UserID, "web", now, now.Add(-idleTTL)).Count(&count).Error; err != nil {
		return problem.Wrap(500, "desktop_enrollment_issuer_check_failed", "The issuing Web session could not be validated.", err)
	}
	if count != 1 {
		return problem.New(401, "desktop_enrollment_issuer_session_invalid", "The issuing Web session is no longer valid.")
	}
	return nil
}

func requireEnrollableTenant(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID) error {
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.Tenant{}).
		Where("id = ? AND status IN ? AND deleted_at IS NULL", tenantID, []string{"trialing", "active"}).
		Count(&count).Error; err != nil {
		return problem.Wrap(500, "desktop_enrollment_tenant_check_failed", "The Desktop Enrollment Tenant could not be validated.", err)
	}
	if count != 1 {
		return problem.New(409, "desktop_enrollment_tenant_unavailable", "Desktop Enrollment requires an active customer Tenant.")
	}
	return nil
}

func requireOrganizationMembership(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, userID uuid.UUID,
	organizationID *uuid.UUID,
) error {
	if organizationID == nil {
		return nil
	}
	var count int64
	if err := tx.WithContext(ctx).Table("organization_memberships AS membership").
		Joins("JOIN organizations AS organization ON organization.tenant_id = membership.tenant_id AND organization.id = membership.organization_id").
		Where("membership.tenant_id = ? AND membership.organization_id = ? AND membership.user_id = ? AND membership.status = ?",
			tenantID, *organizationID, userID, "active").
		Where("organization.status = ? AND organization.archived_at IS NULL", "active").Count(&count).Error; err != nil {
		return problem.Wrap(500, "desktop_enrollment_organization_check_failed", "The Desktop Enrollment Organization could not be validated.", err)
	}
	if count != 1 {
		return problem.New(409, "desktop_enrollment_organization_unavailable", "The subject needs active access to the selected Organization.")
	}
	return nil
}

func membershipSnapshot(membership persistence.TenantMembership) string {
	return fmt.Sprintf("%s:%s:%s", membership.Role, membership.Status, membership.UpdatedAt.UTC().Format(time.RFC3339Nano))
}

func normalizeReason(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 10 || len(value) > 1000 || strings.ContainsAny(value, "\x00") {
		return "", problem.New(400, "invalid_desktop_enrollment_reason", "Desktop Enrollment reason must be between 10 and 1000 characters.")
	}
	return value, nil
}

func validControlPlaneBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func invalidEnrollment() error {
	return problem.New(401, "desktop_enrollment_invalid", "Desktop Enrollment could not be redeemed.")
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

type enrollmentRow struct {
	persistence.DesktopEnrollment
	IssuerEmail        string `gorm:"column:issuer_email"`
	SubjectEmail       string `gorm:"column:subject_email"`
	SubjectDisplayName string `gorm:"column:subject_display_name"`
}

func enrollmentViewQuery(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.WithContext(ctx).Table("desktop_enrollments AS enrollment").
		Select("enrollment.*, issuer.email AS issuer_email, subject.email AS subject_email, subject.display_name AS subject_display_name").
		Joins("JOIN users AS issuer ON issuer.id = enrollment.issued_by_user_id").
		Joins("JOIN users AS subject ON subject.id = enrollment.subject_user_id")
}

func enrollmentView(row enrollmentRow) Enrollment {
	return Enrollment{
		ID: row.ID, Status: row.Status, Version: row.Version, Mode: row.Mode, Authority: row.Authority,
		ControlPlaneOrigin: row.ControlPlaneOrigin, IssuedByUserID: row.IssuedByUserID,
		IssuerEmail: row.IssuerEmail, SubjectUserID: row.SubjectUserID, SubjectEmail: row.SubjectEmail,
		SubjectDisplayName: row.SubjectDisplayName, TenantID: row.TenantID, OrganizationID: row.OrganizationID,
		Reason: row.Reason, ExpiresAt: row.ExpiresAt, OpenedAt: row.OpenedAt, RedeemedAt: row.RedeemedAt,
		RedeemedDeviceID: row.RedeemedDeviceID, RevokedAt: row.RevokedAt, TerminalReason: row.TerminalReason,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func enrollmentViews(rows []enrollmentRow) []Enrollment {
	items := make([]Enrollment, 0, len(rows))
	for _, row := range rows {
		items = append(items, enrollmentView(row))
	}
	return items
}

func (s *Service) getEnrollment(ctx context.Context, id uuid.UUID) (Enrollment, error) {
	var row enrollmentRow
	if err := enrollmentViewQuery(ctx, s.db).Where("enrollment.id = ?", id).Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Enrollment{}, problem.New(404, "desktop_enrollment_not_found", "Desktop Enrollment not found.")
	} else if err != nil {
		return Enrollment{}, problem.Wrap(500, "desktop_enrollment_load_failed", "Desktop Enrollment could not be loaded.", err)
	}
	return enrollmentView(row), nil
}

type deviceRow struct {
	persistence.DesktopDevice
	UserEmail       string `gorm:"column:user_email"`
	UserDisplayName string `gorm:"column:user_display_name"`
}

func deviceViewQuery(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.WithContext(ctx).Table("desktop_devices AS device").
		Select("device.*, user.email AS user_email, user.display_name AS user_display_name").
		Joins("JOIN users AS user ON user.id = device.user_id")
}

func deviceView(row deviceRow) Device {
	return Device{
		ID: row.ID, UserID: row.UserID, UserEmail: row.UserEmail, UserDisplayName: row.UserDisplayName,
		DefaultTenantID: row.DefaultTenantID, DefaultOrganizationID: row.DefaultOrganizationID,
		Platform: row.Platform, AppVersion: row.AppVersion, DeviceLabel: row.DeviceLabel,
		Status: row.Status, Version: row.Version, LastSeenAt: row.LastSeenAt,
		RevokedAt: row.RevokedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func deviceViews(rows []deviceRow) []Device {
	items := make([]Device, 0, len(rows))
	for _, row := range rows {
		items = append(items, deviceView(row))
	}
	return items
}

func (s *Service) getDevice(ctx context.Context, id uuid.UUID) (Device, error) {
	var row deviceRow
	if err := deviceViewQuery(ctx, s.db).Where("device.id = ?", id).Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Device{}, problem.New(404, "desktop_device_not_found", "Desktop device not found.")
	} else if err != nil {
		return Device{}, problem.Wrap(500, "desktop_device_load_failed", "Desktop device could not be loaded.", err)
	}
	return deviceView(row), nil
}

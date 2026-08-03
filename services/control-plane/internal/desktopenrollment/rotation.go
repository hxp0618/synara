package desktopenrollment

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
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

type RotateInput struct {
	Nonce string `json:"nonce"`
	Proof string `json:"proof"`
}

type RotatedSession struct {
	Audience            string    `json:"audience"`
	Credential          string    `json:"credential"`
	CredentialExpiresAt time.Time `json:"credentialExpiresAt"`
	SessionID           uuid.UUID `json:"sessionId"`
	CredentialFamilyID  uuid.UUID `json:"credentialFamilyId"`
}

func (s *Service) Rotate(
	ctx context.Context,
	principal identity.Principal,
	input RotateInput,
	requestID, ipAddress, userAgent string,
) (RotatedSession, error) {
	if principal.Audience != "desktop" || principal.DesktopDeviceID == nil || principal.ActiveTenantID == nil {
		return RotatedSession{}, problem.New(403, "desktop_session_required", "A Desktop session is required.")
	}
	nonceText := strings.TrimSpace(input.Nonce)
	nonce, err := base64.RawURLEncoding.DecodeString(nonceText)
	if err != nil || len(nonce) != 32 {
		return RotatedSession{}, problem.New(400, "invalid_desktop_rotation_proof", "Desktop session rotation proof is invalid.")
	}
	proof, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(input.Proof))
	if err != nil || len(proof) != ed25519.SignatureSize {
		return RotatedSession{}, problem.New(400, "invalid_desktop_rotation_proof", "Desktop session rotation proof is invalid.")
	}
	if s.config.ControlPlaneOrigin == "" || s.config.SessionTTL <= 0 || s.config.SessionIdleTTL <= 0 {
		return RotatedSession{}, problem.New(503, "desktop_session_rotation_unavailable", "Desktop session rotation is unavailable.")
	}
	now := s.now()
	var result RotatedSession
	var replayDetected bool
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current persistence.LoginSession
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", principal.SessionID).Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_session_load_failed", "The Desktop session could not be rotated.", err)
		}
		if current.Audience != "desktop" || current.UserID != principal.UserID || current.DesktopDeviceID == nil ||
			*current.DesktopDeviceID != *principal.DesktopDeviceID || current.CredentialFamilyID == nil {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		}
		if current.RotatedToSessionID != nil {
			if err := revokeCredentialFamilyForReplay(ctx, tx, current, now, requestID, ipAddress); err != nil {
				return err
			}
			replayDetected = true
			return nil
		}
		if current.RevokedAt != nil || !current.ExpiresAt.After(now) || !current.LastSeenAt.After(now.Add(-s.config.SessionIdleTTL)) {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		}
		var device persistence.DesktopDevice
		if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("id = ? AND user_id = ? AND status = ?", *current.DesktopDeviceID, current.UserID, "active").
			Take(&device).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_device_load_failed", "The Desktop device could not be validated.", err)
		}
		payload := rotationProofPayload(
			s.config.ControlPlaneOrigin, device.ID.String(), current.ID.String(), nonceText,
		)
		if len(device.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(device.PublicKey), []byte(payload), proof) {
			return problem.New(400, "invalid_desktop_rotation_proof", "Desktop session rotation proof is invalid.")
		}
		credential, credentialHash, err := secret.NewToken()
		if err != nil {
			return problem.Wrap(500, "desktop_session_rotation_failed", "The Desktop session could not be rotated.", err)
		}
		nextID := uuid.New()
		expiresAt := now.Add(s.config.SessionTTL)
		next := persistence.LoginSession{
			ID: nextID, UserID: current.UserID, ActiveTenantID: current.ActiveTenantID,
			AuthMethod: "desktop", Audience: "desktop", DesktopDeviceID: current.DesktopDeviceID,
			CredentialFamilyID: current.CredentialFamilyID, RotatedFromSessionID: &current.ID,
			RefreshTokenHash: credentialHash, IPAddress: optionalString(ipAddress), UserAgent: optionalString(userAgent),
			ExpiresAt: expiresAt, LastSeenAt: now, CreatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&next).Error; err != nil {
			return problem.Wrap(409, "desktop_session_rotation_conflict", "The Desktop session changed during rotation.", err)
		}
		updated := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
			Where("id = ? AND revoked_at IS NULL AND rotated_to_session_id IS NULL", current.ID).
			Updates(map[string]any{"rotated_to_session_id": next.ID, "rotated_at": now, "revoked_at": now})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "desktop_session_rotation_conflict", "The Desktop session changed during rotation.", updated.Error)
		}
		if err := tx.WithContext(ctx).Model(&persistence.DesktopDevice{}).
			Where("id = ? AND status = ?", device.ID, "active").
			Updates(map[string]any{"last_seen_at": now, "version": device.Version + 1}).Error; err != nil {
			return problem.Wrap(500, "desktop_device_update_failed", "Desktop device activity could not be updated.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: *current.ActiveTenantID, ActorType: "user", ActorID: &current.UserID,
			Action: "desktop.device_session_rotated", ResourceType: "desktop_device", ResourceID: current.DesktopDeviceID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"previousSessionId": current.ID, "sessionId": next.ID,
				"credentialFamilyId": *current.CredentialFamilyID,
			},
		}); err != nil {
			return err
		}
		result = RotatedSession{
			Audience: "desktop", Credential: credential, CredentialExpiresAt: expiresAt,
			SessionID: next.ID, CredentialFamilyID: *current.CredentialFamilyID,
		}
		return nil
		// The current LoginSession row is the serialization authority. A concurrent waiter must
		// observe rotated_to_session_id and execute family-wide replay revocation, not surface a
		// database serialization error before the replay boundary runs.
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return RotatedSession{}, err
	}
	if replayDetected {
		return RotatedSession{}, problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
	}
	return result, nil
}

func revokeCredentialFamilyForReplay(
	ctx context.Context,
	tx *gorm.DB,
	current persistence.LoginSession,
	now time.Time,
	requestID, ipAddress string,
) error {
	if current.CredentialFamilyID == nil || current.DesktopDeviceID == nil || current.ActiveTenantID == nil {
		return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
	}
	if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
		Where("credential_family_id = ? AND audience = ? AND revoked_at IS NULL", *current.CredentialFamilyID, "desktop").
		Update("revoked_at", now).Error; err != nil {
		return problem.Wrap(500, "desktop_credential_replay_revoke_failed", "The Desktop credential replay boundary could not be enforced.", err)
	}
	if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
		Where("id = ? AND replay_detected_at IS NULL", current.ID).
		Update("replay_detected_at", now).Error; err != nil {
		return problem.Wrap(500, "desktop_credential_replay_revoke_failed", "The Desktop credential replay boundary could not be enforced.", err)
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: *current.ActiveTenantID, ActorType: "user", ActorID: &current.UserID,
		Action: "desktop.device_credential_replay_detected", ResourceType: "desktop_device", ResourceID: current.DesktopDeviceID,
		RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{"sessionId": current.ID, "credentialFamilyId": *current.CredentialFamilyID},
	})
}

func (s *Service) Disconnect(
	ctx context.Context,
	principal identity.Principal,
	requestID, ipAddress string,
) error {
	if principal.Audience != "desktop" || principal.DesktopDeviceID == nil || principal.ActiveTenantID == nil {
		return problem.New(403, "desktop_session_required", "A Desktop session is required.")
	}
	now := s.now()
	const reason = "User disconnected this Desktop from Cloud Panel."
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current persistence.LoginSession
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND user_id = ? AND audience = ? AND desktop_device_id = ? AND revoked_at IS NULL",
				principal.SessionID, principal.UserID, "desktop", *principal.DesktopDeviceID).
			Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_session_load_failed", "The Desktop session could not be disconnected.", err)
		}
		if current.CredentialFamilyID == nil {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		}
		var device persistence.DesktopDevice
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND user_id = ? AND status = ?", *principal.DesktopDeviceID, principal.UserID, "active").
			Take(&device).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(401, "invalid_desktop_session", "The Desktop session is invalid or expired.")
		} else if err != nil {
			return problem.Wrap(500, "desktop_device_load_failed", "The Desktop device could not be disconnected.", err)
		}
		if err := tx.WithContext(ctx).Model(&persistence.LoginSession{}).
			Where("credential_family_id = ? AND audience = ? AND revoked_at IS NULL", *current.CredentialFamilyID, "desktop").
			Update("revoked_at", now).Error; err != nil {
			return problem.Wrap(500, "desktop_session_disconnect_failed", "The Desktop session could not be disconnected.", err)
		}
		updated := tx.WithContext(ctx).Model(&persistence.DesktopDevice{}).
			Where("id = ? AND status = ? AND version = ?", device.ID, "active", device.Version).
			Updates(map[string]any{
				"status": "revoked", "version": device.Version + 1, "revoked_by": principal.UserID,
				"revocation_reason": reason, "revoked_at": now,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "desktop_device_disconnect_conflict", "The Desktop device changed during disconnect.", updated.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *principal.ActiveTenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "desktop.device_disconnected", ResourceType: "desktop_device", ResourceID: principal.DesktopDeviceID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"sessionId": principal.SessionID, "credentialFamilyId": *current.CredentialFamilyID},
		})
	})
}

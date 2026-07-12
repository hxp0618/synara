package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const installationRowKey = "control-plane"

var bootstrapNamespace = uuid.MustParse("ac09582b-c9bb-50bd-83f5-ec0fdbe4ba0d")

type Result struct {
	InstallationID    string
	UserID            uuid.UUID
	TenantID          uuid.UUID
	OrganizationID    uuid.UUID
	ExecutionTargetID uuid.UUID
	Personal          bool
}

func Ensure(ctx context.Context, db *gorm.DB, profile platform.DeploymentProfile, configuredInstallationID string) (Result, error) {
	var result Result
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		installationID, err := ensureInstallation(ctx, tx, profile, configuredInstallationID)
		if err != nil {
			return err
		}
		result.InstallationID = installationID
		if profile == platform.ProfilePersonal {
			result = personalResult(installationID)
			if err := ensurePersonalDomain(ctx, tx, result); err != nil {
				return err
			}
			return nil
		}
		result.ExecutionTargetID = deterministicID(installationID, "platform-local-target")
		return createDoNothing(tx, &persistence.ExecutionTarget{
			ID: result.ExecutionTargetID, Kind: string(platform.TargetLocal), Name: "platform-local",
			Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: map[string]any{"workspaceModes": []string{"local", "worktree"}},
		})
	})
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap control-plane installation: %w", err)
	}
	return result, nil
}

func ensureInstallation(ctx context.Context, tx *gorm.DB, profile platform.DeploymentProfile, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	var installation persistence.PlatformInstallation
	err := tx.WithContext(ctx).Where("key = ?", installationRowKey).Take(&installation).Error
	if err == nil {
		if configured != "" && configured != installation.InstallationID {
			return "", fmt.Errorf("configured installation id does not match the persisted installation")
		}
		if installation.Profile != string(profile) {
			return "", fmt.Errorf("persisted deployment profile is %s; v1 profile changes require explicit export/import", installation.Profile)
		}
		return installation.InstallationID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	if configured == "" {
		configured = uuid.NewString()
	}
	now := time.Now().UTC()
	installation = persistence.PlatformInstallation{
		Key: installationRowKey, InstallationID: configured, Profile: string(profile), CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.WithContext(ctx).Create(&installation).Error; err != nil {
		return "", err
	}
	return configured, nil
}

func personalResult(installationID string) Result {
	return Result{
		InstallationID:    installationID,
		UserID:            deterministicID(installationID, "local-owner-user"),
		TenantID:          deterministicID(installationID, "personal-tenant"),
		OrganizationID:    deterministicID(installationID, "personal-root-organization"),
		ExecutionTargetID: deterministicID(installationID, "personal-local-default-target"),
		Personal:          true,
	}
}

func ensurePersonalDomain(ctx context.Context, tx *gorm.DB, result Result) error {
	now := time.Now().UTC()
	suffix := strings.ReplaceAll(result.TenantID.String(), "-", "")[:12]
	models := []any{
		&persistence.User{
			ID: result.UserID, Email: "local-owner@localhost.invalid", DisplayName: "Local Owner",
			Status: "active", EmailVerifiedAt: &now,
		},
		&persistence.Tenant{
			ID: result.TenantID, Slug: "personal-" + suffix, Name: "Personal", Status: "active",
			PlanCode: "personal", Region: "local", Settings: map[string]any{"deploymentProfile": "personal"},
			CreatedBy: result.UserID,
		},
		&persistence.TenantMembership{
			TenantID: result.TenantID, UserID: result.UserID, Role: "owner", Status: "active", JoinedAt: &now,
		},
		&persistence.Organization{
			ID: result.OrganizationID, TenantID: result.TenantID, Slug: "personal", Name: "Personal",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: result.UserID,
		},
		&persistence.OrganizationMembership{
			TenantID: result.TenantID, OrganizationID: result.OrganizationID, UserID: result.UserID,
			Role: "owner", Status: "active",
		},
		&persistence.ExecutionTarget{
			ID: result.ExecutionTargetID, TenantID: &result.TenantID, OrganizationID: &result.OrganizationID,
			Kind: string(platform.TargetLocal), Name: "local-default", Status: "active",
			ConfigurationEncrypted: []byte{},
			Capabilities:           map[string]any{"workspaceModes": []string{"local", "worktree"}},
		},
		&persistence.AuditLog{
			EventID:  deterministicID(result.InstallationID, "personal-bootstrap-audit"),
			TenantID: result.TenantID, ActorType: "system", Action: "installation.personal_bootstrapped",
			ResourceType: "tenant", ResourceID: &result.TenantID, OrganizationID: &result.OrganizationID,
			RequestID: "bootstrap:" + result.InstallationID, Metadata: map[string]any{"profile": "personal"},
			OccurredAt: now,
		},
	}
	for _, model := range models {
		if err := createDoNothing(tx.WithContext(ctx), model); err != nil {
			return err
		}
	}
	return validatePersonalDomain(ctx, tx, result)
}

func validatePersonalDomain(ctx context.Context, tx *gorm.DB, result Result) error {
	var unownedTargets int64
	if err := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
		Where("tenant_id IS NULL OR organization_id IS NULL").Count(&unownedTargets).Error; err != nil {
		return err
	}
	if unownedTargets != 0 {
		return fmt.Errorf("personal profile contains %d execution targets without tenant and organization ownership", unownedTargets)
	}
	checks := []struct {
		model any
		where string
		args  []any
	}{
		{&persistence.User{}, "id = ? AND status = ? AND deleted_at IS NULL", []any{result.UserID, "active"}},
		{&persistence.Tenant{}, "id = ? AND created_by = ? AND status = ? AND deleted_at IS NULL", []any{result.TenantID, result.UserID, "active"}},
		{&persistence.TenantMembership{}, "tenant_id = ? AND user_id = ? AND role = ? AND status = ?", []any{result.TenantID, result.UserID, "owner", "active"}},
		{&persistence.Organization{}, "id = ? AND tenant_id = ? AND created_by = ? AND status = ?", []any{result.OrganizationID, result.TenantID, result.UserID, "active"}},
		{&persistence.OrganizationMembership{}, "tenant_id = ? AND organization_id = ? AND user_id = ? AND role = ? AND status = ?", []any{result.TenantID, result.OrganizationID, result.UserID, "owner", "active"}},
		{&persistence.ExecutionTarget{}, "id = ? AND tenant_id = ? AND organization_id = ? AND kind = ? AND status = ?", []any{result.ExecutionTargetID, result.TenantID, result.OrganizationID, "local", "active"}},
	}
	for _, check := range checks {
		if err := tx.WithContext(ctx).Where(check.where, check.args...).Take(check.model).Error; err != nil {
			return fmt.Errorf("personal bootstrap invariant failed for %T: %w", check.model, err)
		}
	}
	return nil
}

func createDoNothing(tx *gorm.DB, value any) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(value).Error
}

func deterministicID(installationID, resource string) uuid.UUID {
	return uuid.NewSHA1(bootstrapNamespace, []byte(installationID+":"+resource))
}

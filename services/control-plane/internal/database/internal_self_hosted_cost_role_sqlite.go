package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateInternalSelfHostedCostRoleSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`UPDATE tenant_memberships SET role = 'cost_admin' WHERE role = 'billing_admin'`,
		`UPDATE tenant_invitations SET role = 'cost_admin' WHERE role = 'billing_admin'`,
		`UPDATE identity_group_mappings SET tenant_role = 'cost_admin' WHERE tenant_role = 'billing_admin'`,
		`DROP TRIGGER IF EXISTS trg_tenant_memberships_internal_role_insert`,
		`CREATE TRIGGER trg_tenant_memberships_internal_role_insert
		 BEFORE INSERT ON tenant_memberships
		 WHEN NEW.role NOT IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid internal self-hosted Tenant role');
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_memberships_internal_role_update`,
		`CREATE TRIGGER trg_tenant_memberships_internal_role_update
		 BEFORE UPDATE OF role ON tenant_memberships
		 WHEN NEW.role NOT IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid internal self-hosted Tenant role');
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_invitations_internal_role_insert`,
		`CREATE TRIGGER trg_tenant_invitations_internal_role_insert
		 BEFORE INSERT ON tenant_invitations
		 WHEN NEW.role NOT IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid internal self-hosted Tenant invitation role');
		 END`,
		`DROP TRIGGER IF EXISTS trg_tenant_invitations_internal_role_update`,
		`CREATE TRIGGER trg_tenant_invitations_internal_role_update
		 BEFORE UPDATE OF role ON tenant_invitations
		 WHEN NEW.role NOT IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid internal self-hosted Tenant invitation role');
		 END`,
		`DROP TRIGGER IF EXISTS trg_identity_group_mappings_internal_role_insert`,
		`CREATE TRIGGER trg_identity_group_mappings_internal_role_insert
		 BEFORE INSERT ON identity_group_mappings
		 WHEN NEW.tenant_role IS NOT NULL
		  AND NEW.tenant_role NOT IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid internal self-hosted identity mapping role');
		 END`,
		`DROP TRIGGER IF EXISTS trg_identity_group_mappings_internal_role_update`,
		`CREATE TRIGGER trg_identity_group_mappings_internal_role_update
		 BEFORE UPDATE OF tenant_role ON identity_group_mappings
		 WHEN NEW.tenant_role IS NOT NULL
		  AND NEW.tenant_role NOT IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid internal self-hosted identity mapping role');
		 END`,
	}

	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("migrate internal self-hosted cost role SQLite safety: %w", err)
		}
	}
	return nil
}

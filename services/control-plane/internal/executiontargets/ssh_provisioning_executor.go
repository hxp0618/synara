package executiontargets

import (
	"context"
	"errors"
	"slices"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type SSHProvisioningOperationExecutor struct {
	targets     *Service
	provisioner *SSHProvisioner
}

func NewSSHProvisioningOperationExecutor(targets *Service, provisioner *SSHProvisioner) (*SSHProvisioningOperationExecutor, error) {
	if targets == nil || provisioner == nil {
		return nil, errors.New("SSH provisioning operation executor requires targets and provisioner")
	}
	return &SSHProvisioningOperationExecutor{targets: targets, provisioner: provisioner}, nil
}

func (e *SSHProvisioningOperationExecutor) Execute(ctx context.Context, claim ProvisioningClaim) (SSHProvisionResult, error) {
	principal, authorizedContext, err := e.resolvePrincipal(ctx, claim)
	if err != nil {
		return SSHProvisionResult{}, err
	}
	return e.provisioner.ApplyProvisioningOperation(
		authorizedContext, principal, claim.TenantID, claim.Operation.TargetID,
		claim.Operation.Action, claim.Operation.ID, claim.RequestID, claim.IPAddress,
	)
}

func (e *SSHProvisioningOperationExecutor) resolvePrincipal(
	ctx context.Context, claim ProvisioningClaim,
) (identity.Principal, context.Context, error) {
	activeTenantID := claim.TenantID
	if claim.ActorType == "user" {
		return identity.Principal{UserID: claim.ActorID, ActiveTenantID: &activeTenantID}, ctx, nil
	}
	if claim.ActorType != "service_account" {
		return identity.Principal{}, ctx, problem.New(403, "provisioning_actor_revoked", "Provisioning actor is no longer authorized.")
	}
	var account persistence.ServiceAccount
	if err := e.targets.db.WithContext(ctx).Where(
		"id = ? AND tenant_id = ? AND status = ? AND revoked_at IS NULL", claim.ActorID, claim.TenantID, "active",
	).Take(&account).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return identity.Principal{}, ctx, problem.New(403, "provisioning_actor_revoked", "Provisioning Service Account is no longer active.")
	} else if err != nil {
		return identity.Principal{}, ctx, problem.Wrap(500, "provisioning_actor_lookup_failed", "Provisioning actor could not be authorized.", err)
	}
	organizationMismatch := account.OrganizationID != nil &&
		(claim.OrganizationID == nil || *account.OrganizationID != *claim.OrganizationID)
	if !slices.Contains(account.Scopes, "api.access") || organizationMismatch {
		return identity.Principal{}, ctx, problem.New(403, "provisioning_actor_revoked", "Provisioning Service Account is no longer authorized.")
	}
	serviceAccountID := account.ID
	principal := identity.Principal{
		UserID: account.CreatedBy, ActiveTenantID: &activeTenantID, Audience: "service-account",
		DisplayName: account.Name, ServiceAccountID: &serviceAccountID,
	}
	authorizedContext := authorization.WithMachinePrincipal(ctx, authorization.MachinePrincipal{
		ActorID: account.ID, TenantID: account.TenantID, OrganizationID: account.OrganizationID, Role: account.Role,
	})
	return principal, authorizedContext, nil
}

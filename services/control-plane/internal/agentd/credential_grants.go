package agentd

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

type ResolvedWorkspaceCredential struct {
	GrantID        uuid.UUID      `json:"grantId"`
	BindingKind    string         `json:"bindingKind"`
	Purpose        string         `json:"purpose"`
	Provider       string         `json:"provider"`
	CredentialType string         `json:"credentialType"`
	Selector       string         `json:"selector"`
	Payload        map[string]any `json:"payload"`
}

// ResolveCredentialStage resolves at most the one immutable Grant assigned to
// the requested controlled stage. Callers must invoke it immediately before
// that stage and clear the returned payload immediately afterwards; it never
// walks or eagerly resolves the rest of the Workload descriptors.
func (c *Client) ResolveCredentialStage(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	grants []executions.CredentialGrantDescriptor,
	bindingKind string,
) (*ResolvedWorkspaceCredential, error) {
	grant, err := credentialGrantForStage(grants, bindingKind)
	if err != nil || grant == nil {
		return nil, err
	}
	if err := validateCredentialGrantDescriptor(*grant); err != nil {
		return nil, err
	}
	resolved, err := c.ResolveCredentialGrant(ctx, executionID, grant.GrantID, lease)
	if err != nil {
		return nil, err
	}
	if grant.GrantID != resolved.GrantID || grant.BindingKind != resolved.BindingKind ||
		grant.Purpose != resolved.Purpose || grant.Provider != resolved.Provider ||
		grant.CredentialType != resolved.CredentialType || grant.Selector != resolved.Selector {
		clearResolvedWorkspaceCredential(&resolved)
		return nil, errors.New("resolved Credential Grant metadata does not match its Workload descriptor")
	}
	return &resolved, nil
}

func validateCredentialGrantDescriptor(grant executions.CredentialGrantDescriptor) error {
	if grant.GrantID == uuid.Nil || strings.TrimSpace(grant.Selector) == "" {
		return errors.New("Credential Grant descriptor is invalid")
	}
	valid := false
	switch grant.BindingKind {
	case "git_fetch", "git_push":
		valid = grant.Purpose == "git" && grant.Provider == "git" &&
			(grant.CredentialType == "https_token" || grant.CredentialType == "ssh_key")
	case "registry_pull", "registry_push":
		valid = grant.Purpose == "registry" && grant.Provider == "oci" &&
			(grant.CredentialType == "basic" || grant.CredentialType == "bearer_token")
	case "package_read", "package_publish":
		valid = grant.Purpose == "package" &&
			((grant.Provider == "npm" && grant.CredentialType == "npm_token") ||
				(grant.Provider == "pypi" && grant.CredentialType == "pypi_token"))
	}
	if !valid {
		return errors.New("Credential Grant descriptor stage, purpose, provider, or type is invalid")
	}
	return nil
}

func (c *Client) ResolveCredentialGrant(
	ctx context.Context,
	executionID, grantID uuid.UUID,
	lease executions.Lease,
) (ResolvedWorkspaceCredential, error) {
	var output ResolvedWorkspaceCredential
	err := c.doJSON(
		ctx,
		http.MethodPost,
		executionPath(executionID, "credential-grants/"+grantID.String()+"/resolve"),
		c.workerToken,
		"",
		executions.LeaseInput{
			TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		&output,
	)
	return output, err
}

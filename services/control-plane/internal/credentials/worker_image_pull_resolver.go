package credentials

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const workerImagePullBindingKind = "worker_image_pull"

// WorkerImagePullCredential is a short-lived server-side projection consumed
// only by managed Execution Target provisioners. It must never be included in
// a Worker Workload, agentd environment, API response, audit entry, or log.
type WorkerImagePullCredential struct {
	BindingID         uuid.UUID
	CredentialID      uuid.UUID
	CredentialVersion int
	Host              string
	Username          string
	Password          string
	RegistryToken     string
}

type WorkerImagePullResolution struct {
	Credential    *WorkerImagePullCredential
	Authoritative bool
}

// ResolveWorkerImagePullForTarget resolves the one active target-scoped image
// pull Binding. Database ownership and version checks complete before KMS
// decryption so no transaction or row lock is held across external KMS I/O.
func (s *Service) ResolveWorkerImagePullForTarget(
	ctx context.Context,
	tenantID, targetID uuid.UUID,
	registrySelector string,
) (WorkerImagePullResolution, error) {
	registrySelector, err := normalizeWorkerImagePullSelector(registrySelector)
	if err != nil {
		return WorkerImagePullResolution{Authoritative: true}, problem.New(
			400,
			"worker_image_pull_selector_invalid",
			"Worker image pull registry selector is invalid.",
		)
	}
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, found, authoritative, err := s.loadWorkerImagePullSnapshot(ctx, tenantID, targetID, registrySelector)
		if err != nil {
			return WorkerImagePullResolution{Authoritative: authoritative}, err
		}
		if !found {
			return WorkerImagePullResolution{Authoritative: true}, nil
		}
		if s.cipher == nil {
			return WorkerImagePullResolution{}, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
		}
		payload, err := s.resolveModel(ctx, snapshot.credential)
		if err != nil {
			return WorkerImagePullResolution{Authoritative: authoritativeWorkerImagePullPayloadError(err)}, err
		}
		normalized, err := normalizeRegistryPayload(
			snapshot.credential.Provider,
			snapshot.credential.CredentialType,
			payload,
		)
		if err != nil {
			return WorkerImagePullResolution{Authoritative: true}, problem.New(500, "worker_image_pull_credential_invalid", "Worker image pull Credential payload is invalid.")
		}
		host, _ := normalized["host"].(string)
		if host == "" || host != strings.TrimSpace(snapshot.binding.SelectorValue) {
			return WorkerImagePullResolution{Authoritative: true}, problem.New(409, "worker_image_pull_binding_selector_mismatch", "Worker image pull Credential no longer matches its immutable Binding selector.")
		}

		current, currentFound, currentAuthoritative, err := s.loadWorkerImagePullSnapshot(ctx, tenantID, targetID, registrySelector)
		if err != nil {
			return WorkerImagePullResolution{Authoritative: currentAuthoritative}, err
		}
		if !currentFound {
			return WorkerImagePullResolution{Authoritative: true}, nil
		}
		if current.binding.ID != snapshot.binding.ID ||
			current.credential.ID != snapshot.credential.ID ||
			current.credential.Version != snapshot.credential.Version {
			continue
		}

		resolved := &WorkerImagePullCredential{
			BindingID: snapshot.binding.ID, CredentialID: snapshot.credential.ID,
			CredentialVersion: snapshot.credential.Version, Host: host,
		}
		if snapshot.credential.CredentialType == RegistryBasicCredentialType {
			resolved.Username, _ = normalized["username"].(string)
			resolved.Password, _ = normalized["password"].(string)
		} else {
			resolved.RegistryToken, _ = normalized["token"].(string)
		}
		return WorkerImagePullResolution{Credential: resolved, Authoritative: true}, nil
	}
	return WorkerImagePullResolution{}, problem.New(
		409,
		"worker_image_pull_resolution_conflict",
		"Worker image pull Credential changed repeatedly during resolution.",
	)
}

type workerImagePullSnapshot struct {
	binding    persistence.CredentialBinding
	credential persistence.ProviderCredential
}

func (s *Service) loadWorkerImagePullSnapshot(
	ctx context.Context,
	tenantID, targetID uuid.UUID,
	registrySelector string,
) (workerImagePullSnapshot, bool, bool, error) {
	var snapshot workerImagePullSnapshot
	found := false
	authoritative := false
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var target persistence.ExecutionTarget
		err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", tenantID, targetID).
			Take(&target).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			authoritative = true
			return problem.New(404, "execution_target_not_found", "Execution Target not found.")
		}
		if err != nil {
			return problem.Wrap(500, "execution_target_load_failed", "Execution Target could not be loaded.", err)
		}

		bindings := make([]persistence.CredentialBinding, 0)
		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where(
				"tenant_id = ? AND execution_target_id = ? AND binding_kind = ? AND disabled_at IS NULL",
				tenantID, targetID, workerImagePullBindingKind,
			).
			Order("id").Limit(2).Find(&bindings).Error
		if err != nil {
			return problem.Wrap(500, "worker_image_pull_binding_load_failed", "Worker image pull Bindings could not be loaded.", err)
		}
		if len(bindings) == 0 {
			authoritative = true
			return nil
		}
		if len(bindings) != 1 {
			authoritative = true
			return problem.New(409, "worker_image_pull_binding_ambiguous", "Multiple active Worker image pull Bindings exist for the Execution Target.")
		}
		selector, selectorErr := normalizeWorkerImagePullSelector(bindings[0].SelectorValue)
		if selectorErr != nil {
			authoritative = true
			return problem.New(409, "worker_image_pull_binding_invalid", "Worker image pull Binding selector is invalid.")
		}
		if selector != registrySelector {
			authoritative = true
			return problem.New(409, "worker_image_pull_binding_selector_mismatch", "No active Worker image pull Binding matches the configured image registry.")
		}
		snapshot.binding = bindings[0]
		found = true
		if snapshot.binding.ProjectID != nil || snapshot.binding.ExecutionTargetID == nil || *snapshot.binding.ExecutionTargetID != targetID {
			authoritative = true
			return problem.New(409, "worker_image_pull_binding_invalid", "Worker image pull Binding ownership is invalid.")
		}
		if !sameOptionalUUID(snapshot.binding.OrganizationID, target.OrganizationID) {
			authoritative = true
			return problem.New(409, "worker_image_pull_binding_scope_mismatch", "Worker image pull Binding no longer matches the Execution Target scope.")
		}

		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", tenantID, snapshot.binding.CredentialID).
			Take(&snapshot.credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			authoritative = true
			return problem.New(409, "worker_image_pull_credential_unavailable", "Worker image pull Credential is unavailable.")
		}
		if err != nil {
			return problem.Wrap(500, "worker_image_pull_credential_load_failed", "Worker image pull Credential could not be loaded.", err)
		}
		if snapshot.credential.RevokedAt != nil || (snapshot.credential.ExpiresAt != nil && !snapshot.credential.ExpiresAt.After(s.now())) {
			authoritative = true
			return problem.New(409, "worker_image_pull_credential_unavailable", "Worker image pull Credential is revoked or expired.")
		}
		if snapshot.credential.Purpose != PurposeRegistry || snapshot.credential.Provider != RegistryProviderOci ||
			(snapshot.credential.CredentialType != RegistryBasicCredentialType && snapshot.credential.CredentialType != RegistryBearerCredentialType) {
			authoritative = true
			return problem.New(409, "worker_image_pull_credential_invalid", "Worker image pull Binding does not reference a supported OCI Registry Credential.")
		}
		if snapshot.credential.Scope != credentialscope.ScopeTenant && snapshot.credential.Scope != credentialscope.ScopeOrganization {
			authoritative = true
			return problem.New(409, "worker_image_pull_credential_scope_mismatch", "Worker image pull Credential scope is invalid.")
		}
		if snapshot.credential.Scope == credentialscope.ScopeOrganization &&
			(snapshot.credential.OrganizationID == nil || target.OrganizationID == nil || *snapshot.credential.OrganizationID != *target.OrganizationID) {
			authoritative = true
			return problem.New(409, "worker_image_pull_credential_scope_mismatch", "Worker image pull Credential no longer matches the Execution Target Organization.")
		}
		return nil
	})
	if err != nil {
		return workerImagePullSnapshot{}, false, authoritative, err
	}
	if !found {
		return workerImagePullSnapshot{}, false, true, nil
	}
	return snapshot, true, true, nil
}

func authoritativeWorkerImagePullPayloadError(err error) bool {
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		return false
	}
	return apiError.Code == "credential_payload_invalid" || apiError.Code == "credential_purpose_invalid"
}

func normalizeWorkerImagePullSelector(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if value == "" || len(value) > 320 || strings.ContainsAny(value, "/\\@?#\r\n\t\x00") {
		return "", errors.New("invalid Worker image pull selector")
	}
	for _, character := range value {
		if character == ' ' {
			return "", errors.New("invalid Worker image pull selector")
		}
	}
	if value == "index.docker.io" {
		return "docker.io", nil
	}
	return value, nil
}

func sameOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

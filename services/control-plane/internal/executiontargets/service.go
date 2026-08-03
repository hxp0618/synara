package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Target struct {
	ID                     uuid.UUID                                `json:"id"`
	TenantID               *uuid.UUID                               `json:"tenantId"`
	OrganizationID         *uuid.UUID                               `json:"organizationId"`
	Kind                   string                                   `json:"kind"`
	Name                   string                                   `json:"name"`
	Status                 string                                   `json:"status"`
	Capabilities           map[string]any                           `json:"capabilities"`
	IsolationProfile       platform.ExecutionTargetIsolationProfile `json:"isolationProfile"`
	PlatformSharedEligible bool                                     `json:"platformSharedEligible"`
	ProductBoundary        string                                   `json:"productBoundary"`
	RuntimeIsolationPolicy *RuntimeIsolationPolicyView              `json:"runtimeIsolationPolicy,omitempty"`
	RuntimeIsolationStatus *RuntimeIsolationStatusView              `json:"runtimeIsolationStatus,omitempty"`
	CreatedAt              time.Time                                `json:"createdAt"`
	UpdatedAt              time.Time                                `json:"updatedAt"`
}

type RuntimeIsolationPolicyView struct {
	Mode                      string                                   `json:"mode"`
	RequestedRuntime          string                                   `json:"requestedRuntime"`
	Preferred                 []string                                 `json:"preferred"`
	MinimumProfile            platform.ExecutionTargetIsolationProfile `json:"minimumProfile"`
	FallbackPolicy            string                                   `json:"fallbackPolicy"`
	RuntimeClassName          string                                   `json:"runtimeClassName,omitempty"`
	GVisorCompatibleProviders []string                                 `json:"gvisorCompatibleProviders"`
}

type RuntimeIsolationStatusView struct {
	State             string                         `json:"state"`
	DetectedRuntimes  []string                       `json:"detectedRuntimes"`
	DetectedProfiles  []string                       `json:"detectedProfiles"`
	ReasonCode        *string                        `json:"reasonCode"`
	ObservedAt        time.Time                      `json:"observedAt"`
	ExpiresAt         time.Time                      `json:"expiresAt"`
	RunningGeneration *RuntimeIsolationEffectiveView `json:"runningGeneration,omitempty"`
}

type RuntimeIsolationEffectiveView struct {
	Generation       int64                                    `json:"generation"`
	EffectiveRuntime string                                   `json:"effectiveRuntime"`
	EffectiveProfile platform.ExecutionTargetIsolationProfile `json:"effectiveProfile"`
	Decision         string                                   `json:"decision"`
	PolicySource     string                                   `json:"policySource"`
}

type CreateInput struct {
	OrganizationID *uuid.UUID     `json:"organizationId"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Configuration  map[string]any `json:"configuration"`
	Capabilities   map[string]any `json:"capabilities"`
}

type Binding struct {
	ID   uuid.UUID
	Kind platform.ExecutionTargetKind
}

type Service struct {
	db                          *gorm.DB
	authorizer                  *authorization.Authorizer
	platform                    platform.Config
	cipher                      *secret.CursorCipher
	kubernetesReconcilerLocalMu sync.Mutex
	dockerReconcilerLocalMu     sync.Mutex
}

func NewService(db *gorm.DB, platformConfig platform.Config, cipher *secret.CursorCipher) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), platform: platformConfig, cipher: cipher}
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Target, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return nil, err
	}
	models := make([]persistence.ExecutionTarget, 0)
	err := s.db.WithContext(ctx).
		Where("status <> ? AND (tenant_id = ? OR (tenant_id IS NULL AND kind = ?))", "disabled", tenantID, platform.TargetKubernetes).
		Order("CASE WHEN tenant_id IS NULL THEN 1 ELSE 0 END, LOWER(name), id").
		Find(&models).Error
	if err != nil {
		return nil, problem.Wrap(500, "execution_targets_load_failed", "Failed to load execution targets.", err)
	}
	items := make([]Target, 0, len(models))
	for _, model := range models {
		items = append(items, s.projectTarget(model))
	}
	return s.projectTargetRuntimeIsolationStatuses(ctx, tenantID, items)
}

func (s *Service) Get(ctx context.Context, principal identity.Principal, tenantID, targetID uuid.UUID) (Target, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return Target{}, err
	}
	model, err := s.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return Target{}, err
	}
	return s.projectTargetRuntimeIsolationStatus(ctx, tenantID, s.projectTarget(model))
}

func (s *Service) Create(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, input CreateInput) (Target, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, err
	}
	kind, err := platform.ParseExecutionTargetKind(input.Kind)
	if err != nil {
		return Target{}, problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	if platform.IsRemoteTarget(kind) && (!s.platform.LeaseEnabled || !s.platform.FencingEnabled) {
		return Target{}, problem.New(409, "remote_target_protocol_unsupported", "Remote execution targets require lease and fencing support.")
	}
	name, err := validation.Name(input.Name, "invalid_execution_target_name", "Execution target name", 160)
	if err != nil {
		return Target{}, err
	}
	if s.platform.Profile == platform.ProfilePersonal && input.OrganizationID == nil {
		return Target{}, problem.New(400, "personal_execution_target_organization_required", "Personal execution targets must belong to the Personal organization.")
	}
	if input.OrganizationID != nil {
		if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, *input.OrganizationID, authorization.OrganizationRead); err != nil {
			return Target{}, err
		}
	}
	input.Configuration = defaultRuntimeIsolationForNewTarget(kind, s.platform.Profile, input.Configuration)
	if kind == platform.TargetKubernetes || kind == platform.TargetDocker {
		rawPolicy, ok := input.Configuration["runtimeIsolation"].(map[string]any)
		if !ok {
			return Target{}, invalidRuntimeIsolationConfiguration("runtimeIsolation must be a JSON object.")
		}
		normalizedPolicy, err := normalizeRuntimeIsolationPolicyUpdate(
			string(kind), input.Configuration, rawPolicy,
		)
		if err != nil {
			return Target{}, err
		}
		input.Configuration["runtimeIsolation"] = normalizedPolicy
	}
	configuration, err := encryptConfiguration(s.cipher, input.Configuration)
	if err != nil {
		return Target{}, err
	}
	capabilities := input.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	capabilities, err = normalizeExecutionTargetCapabilities(capabilities)
	if err != nil {
		return Target{}, err
	}
	if err := validatePublicCapabilities(capabilities); err != nil {
		return Target{}, err
	}
	status := "active"
	if kind == platform.TargetSSH {
		status = "offline"
	}
	model := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenantID, OrganizationID: input.OrganizationID,
		Kind: string(kind), Name: name, Status: status, ConfigurationEncrypted: configuration,
		ConfigurationKeyID: runtimeSecretKeyID(s.cipher, configuration), Capabilities: capabilities,
	}
	if err := s.db.WithContext(ctx).Create(&model).Error; err != nil {
		return Target{}, problem.Wrap(409, "execution_target_create_rejected", "Execution target creation was rejected.", err)
	}
	return s.projectTarget(model), nil
}

func (s *Service) UpdateProviderPolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	rawPolicy map[string]any,
) (Target, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, err
	}

	var updated persistence.ExecutionTarget
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.ExecutionTarget
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND (tenant_id = ? OR tenant_id IS NULL)", targetID, tenantID).
			Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "execution_target_not_found", "Execution target not found.")
		}
		if err != nil {
			return problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", err)
		}
		if model.TenantID == nil {
			return problem.New(403, "shared_execution_target_provider_policy_immutable", "Platform-shared execution target Provider Policy cannot be changed by a tenant.")
		}

		capabilities := make(map[string]any, len(model.Capabilities)+1)
		for key, value := range model.Capabilities {
			capabilities[key] = value
		}
		capabilities["providerPolicy"] = rawPolicy
		normalized, err := normalizeExecutionTargetCapabilities(capabilities)
		if err != nil {
			return err
		}
		requestedPolicy, err := ParseProviderPolicy(normalized)
		if err != nil {
			return err
		}
		currentPolicy, currentErr := ParseProviderPolicy(model.Capabilities)
		if currentErr == nil && currentPolicy.Equal(requestedPolicy) {
			updated = model
			return nil
		}
		workerCompatibilityChanged := currentErr != nil ||
			!currentPolicy.WorkerCompatibilityEqual(requestedPolicy)

		now := time.Now().UTC()
		model.Capabilities = normalized
		model.UpdatedAt = now
		if err := tx.WithContext(ctx).Model(&model).
			Where("id = ? AND tenant_id = ?", targetID, tenantID).
			Select("capabilities", "updated_at").Updates(&model).Error; err != nil {
			return problem.Wrap(500, "execution_target_provider_policy_update_failed", "Failed to update the execution target Provider Policy.", err)
		}
		if workerCompatibilityChanged {
			if err := invalidateWorkerManifestsForCapabilityPolicyChange(
				ctx, tx, targetID, now, "Execution Target Provider Policy changed; re-register the Worker before claiming more executions.",
			); err != nil {
				return problem.Wrap(500, "worker_manifest_invalidation_failed", "Failed to invalidate Workers after the Provider Policy changed.", err)
			}
		}
		updated = model
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	return s.projectTarget(updated), nil
}

func (s *Service) UpdateProcessContainmentPolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	rawPolicy map[string]any,
) (Target, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, err
	}

	var updated persistence.ExecutionTarget
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model persistence.ExecutionTarget
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND (tenant_id = ? OR tenant_id IS NULL)", targetID, tenantID).
			Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "execution_target_not_found", "Execution target not found.")
		}
		if err != nil {
			return problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", err)
		}
		if model.TenantID == nil {
			return problem.New(403, "shared_execution_target_process_containment_policy_immutable", "Platform-shared execution target process containment policy cannot be changed by a tenant.")
		}

		capabilities := make(map[string]any, len(model.Capabilities)+1)
		for key, value := range model.Capabilities {
			capabilities[key] = value
		}
		capabilities["processContainmentPolicy"] = rawPolicy
		normalized, err := normalizeExecutionTargetCapabilities(capabilities)
		if err != nil {
			return err
		}
		requestedPolicy, err := ParseProcessContainmentPolicy(normalized)
		if err != nil {
			return err
		}
		currentPolicy, currentErr := ParseProcessContainmentPolicy(model.Capabilities)
		if currentErr == nil &&
			currentPolicy.TrustMode == requestedPolicy.TrustMode &&
			currentPolicy.KeyID == requestedPolicy.KeyID &&
			bytes.Equal(currentPolicy.PublicKey, requestedPolicy.PublicKey) {
			updated = model
			return nil
		}

		now := time.Now().UTC()
		model.Capabilities = normalized
		model.UpdatedAt = now
		if err := tx.WithContext(ctx).Model(&model).
			Where("id = ? AND tenant_id = ?", targetID, tenantID).
			Select("capabilities", "updated_at").Updates(&model).Error; err != nil {
			return problem.Wrap(500, "execution_target_process_containment_policy_update_failed", "Failed to update the execution target process containment policy.", err)
		}
		if err := invalidateWorkerManifestsForCapabilityPolicyChange(
			ctx, tx, targetID, now, "Execution Target process containment policy changed; re-register the Worker before claiming more executions.",
		); err != nil {
			return problem.Wrap(500, "worker_manifest_invalidation_failed", "Failed to invalidate Workers after the process containment policy changed.", err)
		}
		updated = model
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	return s.projectTarget(updated), nil
}

func (s *Service) UpdateRuntimeIsolationPolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID, targetID uuid.UUID,
	rawPolicy map[string]any,
	requestID, ipAddress string,
) (Target, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Target{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerManage); err != nil {
		return Target{}, err
	}
	current, err := s.loadAccessible(ctx, tenantID, targetID, false)
	if err != nil {
		return Target{}, err
	}
	if current.TenantID == nil {
		return Target{}, problem.New(403, "shared_execution_target_runtime_isolation_policy_immutable", "Platform-shared execution target runtime isolation policy cannot be changed by a tenant.")
	}
	if current.Kind != string(platform.TargetKubernetes) && current.Kind != string(platform.TargetDocker) {
		return Target{}, problem.New(409, "runtime_isolation_target_kind_unsupported", "Only managed Kubernetes and Docker Targets support runtime isolation policy updates.")
	}

	releaseReconcilerLock := func() {}
	var acquired bool
	switch current.Kind {
	case string(platform.TargetKubernetes):
		releaseReconcilerLock, acquired, err = s.tryKubernetesReconcilerLock(ctx)
	case string(platform.TargetDocker):
		releaseReconcilerLock, acquired, err = s.tryDockerReconcilerLock(ctx)
	}
	if err != nil {
		return Target{}, problem.Wrap(500, "runtime_isolation_policy_coordination_failed", "Runtime isolation policy update coordination failed.", err)
	}
	if !acquired {
		return Target{}, problem.New(409, "execution_target_reconciler_busy", "The managed Target Reconciler is active; retry the runtime isolation policy update after the current cycle finishes.")
	}
	defer releaseReconcilerLock()

	var updated persistence.ExecutionTarget
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND tenant_id = ?", targetID, tenantID).
			Take(&updated).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "execution_target_not_found", "Execution target not found.")
		}
		if loadErr != nil {
			return problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", loadErr)
		}
		if updated.Status == "disabled" {
			return problem.New(409, "runtime_isolation_policy_target_disabled", "A disabled Execution Target cannot change runtime isolation policy.")
		}
		configuration, err := decryptExecutionTargetConfiguration(s.cipher, updated.ConfigurationEncrypted)
		if err != nil {
			return err
		}
		normalizedPolicy, err := normalizeRuntimeIsolationPolicyUpdate(updated.Kind, configuration, rawPolicy)
		if err != nil {
			return err
		}
		currentPolicy, currentErr := normalizeRuntimeIsolationPolicyUpdate(
			updated.Kind, configuration, runtimeIsolationPolicyMap(configuration["runtimeIsolation"]),
		)
		if currentErr == nil && sameRuntimeIsolationPolicy(currentPolicy, normalizedPolicy) {
			return nil
		}
		if runtimeIsolationRequiresGVisor(normalizedPolicy) {
			if err := gvisorCompatibilityAccepted(ctx, tx, updated, normalizedPolicy); err != nil {
				return err
			}
		}
		if err := requireTargetRuntimeIsolationTransitionDrained(ctx, tx, targetID); err != nil {
			return err
		}
		configuration["runtimeIsolation"] = normalizedPolicy
		encrypted, err := encryptConfiguration(s.cipher, configuration)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		updated.ConfigurationEncrypted = encrypted
		updated.Status = "offline"
		updated.UpdatedAt = now
		result := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where("id = ? AND tenant_id = ? AND status <> ?", targetID, tenantID, "disabled").
			Updates(map[string]any{
				"configuration_encrypted": encrypted,
				"status":                  "offline",
				"updated_at":              now,
			})
		if result.Error != nil {
			return problem.Wrap(500, "runtime_isolation_policy_update_failed", "Failed to update the runtime isolation policy.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "runtime_isolation_policy_update_conflict", "The Execution Target changed while its runtime isolation policy was being updated.")
		}
		if err := tx.WithContext(ctx).
			Delete(&persistence.ExecutionTargetRuntimeIsolationObservation{}, "execution_target_id = ?", targetID).Error; err != nil {
			return problem.Wrap(500, "runtime_isolation_observation_invalidation_failed", "Failed to invalidate the previous runtime isolation observation.", err)
		}
		if err := invalidateWorkerManifestsForCapabilityPolicyChange(
			ctx, tx, targetID, now,
			"Execution Target runtime isolation policy changed; re-register the Worker before claiming more executions.",
		); err != nil {
			return problem.Wrap(500, "worker_manifest_invalidation_failed", "Failed to invalidate Workers after the runtime isolation policy changed.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "execution_target.runtime_isolation_policy_updated", ResourceType: "execution_target", ResourceID: &targetID,
			OrganizationID: updated.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"targetKind": updated.Kind, "status": "offline"},
		})
	})
	if err != nil {
		return Target{}, err
	}
	return s.projectTargetRuntimeIsolationStatus(ctx, tenantID, s.projectTarget(updated))
}

func (s *Service) ResolveForSession(ctx context.Context, tenantID, organizationID uuid.UUID, requested *uuid.UUID) (Binding, error) {
	var model persistence.ExecutionTarget
	query := s.db.WithContext(ctx).
		Where("status = ?", "active").
		Where("(tenant_id = ? OR (tenant_id IS NULL AND kind = ?)) AND (organization_id IS NULL OR organization_id = ?)", tenantID, platform.TargetKubernetes, organizationID)
	if requested != nil && *requested != uuid.Nil {
		query = query.Where("id = ?", *requested)
	} else {
		query = query.Order("CASE WHEN tenant_id IS NOT NULL AND organization_id IS NOT NULL THEN 0 WHEN tenant_id IS NOT NULL THEN 1 ELSE 2 END").
			Order("CASE WHEN name = 'local-default' OR name = 'platform-local' THEN 0 ELSE 1 END, LOWER(name), id")
	}
	if err := query.Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Binding{}, problem.New(409, "execution_target_required", "Create or select an active execution target before creating this session.")
	} else if err != nil {
		return Binding{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to resolve the execution target.", err)
	}
	kind, err := platform.ParseExecutionTargetKind(model.Kind)
	if err != nil {
		return Binding{}, problem.Wrap(500, "invalid_persisted_execution_target", "The persisted execution target kind is invalid.", err)
	}
	return Binding{ID: model.ID, Kind: kind}, nil
}

func (s *Service) ResolveWorkerTarget(ctx context.Context, targetID uuid.UUID, targetKind string) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	return resolveWorkerTarget(ctx, s.db, targetID, targetKind)
}

// ResolveWorkerBootstrapTarget is intentionally active-only. Offline SSH
// bootstrap authority requires the operation generation and exact instance UID
// to be validated while holding the Target lock in the caller transaction.
func (s *Service) ResolveWorkerBootstrapTarget(
	ctx context.Context,
	targetID uuid.UUID,
	targetKind string,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	return resolveWorkerTarget(ctx, s.db, targetID, targetKind)
}

func (s *Service) ResolveWorkerTargetInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	targetKind string,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	return resolveWorkerTargetLocked(ctx, tx, targetID, targetKind)
}

func (s *Service) ResolveWorkerBootstrapTargetInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	targetKind string,
	instanceUID string,
	sshBootstrapGeneration *int64,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	return resolveWorkerBootstrapTargetLocked(
		ctx, tx, targetID, targetKind, instanceUID, sshBootstrapGeneration, false,
	)
}

func (s *Service) ResolveWorkerRegistrationTargetInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	targetKind string,
	instanceUID string,
	sshBootstrapGeneration *int64,
	verifiedKubernetesPodBound bool,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	return resolveWorkerBootstrapTargetLocked(
		ctx, tx, targetID, targetKind, instanceUID, sshBootstrapGeneration, verifiedKubernetesPodBound,
	)
}

func resolveWorkerBootstrapTargetLocked(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
	targetKind string,
	instanceUID string,
	sshBootstrapGeneration *int64,
	allowOfflineKubernetes bool,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	kind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		return persistence.ExecutionTarget{}, "", problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	var model persistence.ExecutionTarget
	if err := persistence.WithLocking(db.WithContext(ctx), "UPDATE", "").
		Where("id = ?", targetID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	} else if err != nil {
		return persistence.ExecutionTarget{}, "", problem.Wrap(500, "execution_target_lookup_failed", "Failed to resolve the execution target.", err)
	}
	if model.Kind != string(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(409, "execution_target_kind_mismatch", "targetKind does not match the persisted execution target.")
	}
	if model.TenantID == nil && !platform.IsPlatformSharedTargetEligible(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	if kind != platform.TargetSSH {
		if model.Status != "active" && !(allowOfflineKubernetes && kind == platform.TargetKubernetes && model.Status == "offline") {
			return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
		}
		return model, kind, nil
	}
	if model.Status == "active" {
		return model, kind, nil
	}
	if model.Status != "offline" || model.SSHOperationKind == nil ||
		(*model.SSHOperationKind != "install" && *model.SSHOperationKind != "upgrade") ||
		model.SSHExpectedInstanceUID == nil || sshBootstrapGeneration == nil ||
		*sshBootstrapGeneration != model.SSHOperationGeneration ||
		strings.TrimSpace(instanceUID) != model.SSHExpectedInstanceUID.String() {
		return persistence.ExecutionTarget{}, "", problem.New(
			409,
			"ssh_bootstrap_authority_invalid",
			"SSH Worker bootstrap authority is not valid for the current Target operation.",
		)
	}
	return model, kind, nil
}

func resolveWorkerTarget(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
	targetKind string,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	kind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		return persistence.ExecutionTarget{}, "", problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	var model persistence.ExecutionTarget
	if err := db.WithContext(ctx).Where("id = ? AND status = ?", targetID, "active").Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	} else if err != nil {
		return persistence.ExecutionTarget{}, "", problem.Wrap(500, "execution_target_lookup_failed", "Failed to resolve the execution target.", err)
	}
	if model.Kind != string(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(409, "execution_target_kind_mismatch", "targetKind does not match the persisted execution target.")
	}
	if model.TenantID == nil && !platform.IsPlatformSharedTargetEligible(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	return model, kind, nil
}

func resolveWorkerTargetLocked(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
	targetKind string,
) (persistence.ExecutionTarget, platform.ExecutionTargetKind, error) {
	kind, err := platform.ParseExecutionTargetKind(targetKind)
	if err != nil {
		return persistence.ExecutionTarget{}, "", problem.New(400, "invalid_execution_target_kind", err.Error()+".")
	}
	var model persistence.ExecutionTarget
	if err := persistence.WithLocking(db.WithContext(ctx), "UPDATE", "").
		Where("id = ? AND status = ?", targetID, "active").
		Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	} else if err != nil {
		return persistence.ExecutionTarget{}, "", problem.Wrap(500, "execution_target_lookup_failed", "Failed to resolve the execution target.", err)
	}
	if model.Kind != string(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(409, "execution_target_kind_mismatch", "targetKind does not match the persisted execution target.")
	}
	if model.TenantID == nil && !platform.IsPlatformSharedTargetEligible(kind) {
		return persistence.ExecutionTarget{}, "", problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	return model, kind, nil
}

func (s *Service) loadAccessible(ctx context.Context, tenantID, targetID uuid.UUID, activeOnly bool) (persistence.ExecutionTarget, error) {
	var model persistence.ExecutionTarget
	query := s.db.WithContext(ctx).Where(
		"id = ? AND (tenant_id = ? OR (tenant_id IS NULL AND kind = ?))",
		targetID, tenantID, platform.TargetKubernetes,
	)
	if activeOnly {
		query = query.Where("status = ?", "active")
	}
	if err := query.Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	} else if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the execution target.", err)
	}
	return model, nil
}

func encryptConfiguration(cipher *secret.CursorCipher, configuration map[string]any) ([]byte, error) {
	if len(configuration) == 0 {
		return []byte{}, nil
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return nil, problem.Wrap(400, "invalid_execution_target_configuration", "Execution target configuration is not valid JSON.", err)
	}
	if cipher == nil {
		return nil, problem.New(503, "execution_target_encryption_unavailable", "Execution target configuration encryption is not configured.")
	}
	encrypted, err := cipher.Encrypt(string(encoded))
	if err != nil {
		return nil, problem.Wrap(503, "execution_target_encryption_unavailable", "Execution target configuration encryption is unavailable.", err)
	}
	return encrypted, nil
}

func decryptExecutionTargetConfiguration(
	cipher *secret.CursorCipher,
	encrypted []byte,
) (map[string]any, error) {
	if len(encrypted) == 0 {
		return nil, problem.New(409, "execution_target_configuration_missing", "Execution target configuration is missing.")
	}
	if cipher == nil {
		return nil, problem.New(503, "execution_target_encryption_unavailable", "Execution target configuration encryption is not configured.")
	}
	decoded, err := cipher.Decrypt(encrypted)
	if err != nil {
		return nil, problem.Wrap(503, "execution_target_configuration_unavailable", "Execution target configuration could not be decrypted.", err)
	}
	configuration := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(decoded))
	decoder.UseNumber()
	if err := decoder.Decode(&configuration); err != nil {
		return nil, problem.New(400, "invalid_execution_target_configuration", "Execution target configuration is invalid.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, problem.New(400, "invalid_execution_target_configuration", "Execution target configuration is invalid.")
	}
	return configuration, nil
}

func normalizeRuntimeIsolationPolicyUpdate(
	targetKind string,
	configuration map[string]any,
	rawPolicy map[string]any,
) (*runtimeIsolationConfiguration, error) {
	encoded, err := json.Marshal(rawPolicy)
	if err != nil {
		return nil, invalidRuntimeIsolationConfiguration("runtimeIsolation must be a JSON object.")
	}
	var policy runtimeIsolationConfiguration
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, invalidRuntimeIsolationConfiguration("runtimeIsolation contains an invalid or unknown field.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, invalidRuntimeIsolationConfiguration("runtimeIsolation must contain exactly one JSON object.")
	}
	switch targetKind {
	case string(platform.TargetKubernetes):
		backend, _ := configuration["allocationBackend"].(string)
		if strings.TrimSpace(backend) == "" {
			backend = string(kubernetesAllocationBackendNativePod)
		}
		candidate := kubernetesTargetConfiguration{AllocationBackend: backend, RuntimeIsolation: &policy}
		if err := normalizeKubernetesRuntimeIsolationConfiguration(&candidate); err != nil {
			return nil, err
		}
		return candidate.RuntimeIsolation, nil
	case string(platform.TargetDocker):
		candidate := dockerTargetConfiguration{RuntimeIsolation: &policy}
		if err := normalizeDockerRuntimeIsolationConfiguration(&candidate); err != nil {
			return nil, err
		}
		return candidate.RuntimeIsolation, nil
	default:
		return nil, problem.New(409, "runtime_isolation_target_kind_unsupported", "Only managed Kubernetes and Docker Targets support runtime isolation policy updates.")
	}
}

func runtimeIsolationPolicyMap(value any) map[string]any {
	policy, _ := value.(map[string]any)
	return policy
}

func sameRuntimeIsolationPolicy(left, right *runtimeIsolationConfiguration) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func requireTargetRuntimeIsolationTransitionDrained(ctx context.Context, tx *gorm.DB, targetID uuid.UUID) error {
	var activeExecutions int64
	if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ? AND status IN ?", targetID, kubernetesTargetNonterminalExecutionStatuses).
		Count(&activeExecutions).Error; err != nil {
		return problem.Wrap(500, "runtime_isolation_policy_drain_probe_failed", "Runtime isolation policy transition safety could not be evaluated.", err)
	}
	if activeExecutions != 0 {
		return &problem.Error{
			Status:  409,
			Code:    "runtime_isolation_policy_execution_active",
			Message: "Drain or terminalize every active Execution before changing runtime isolation policy.",
			Details: map[string]any{"activeExecutionCount": activeExecutions},
		}
	}
	return nil
}

func runtimeSecretKeyID(cipher *secret.CursorCipher, encrypted []byte) *string {
	if cipher == nil || len(encrypted) == 0 || cipher.PrimaryKeyID() == "" {
		return nil
	}
	keyID := cipher.PrimaryKeyID()
	return &keyID
}

func toTarget(model persistence.ExecutionTarget) Target {
	capabilities := model.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	kind, _ := platform.ParseExecutionTargetKind(model.Kind)
	isolation := platform.IsolationDeclaration(kind)
	return Target{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Kind: model.Kind, Name: model.Name, Status: model.Status, Capabilities: capabilities,
		IsolationProfile: isolation.Profile, PlatformSharedEligible: isolation.PlatformSharedEligible,
		ProductBoundary: isolation.ProductBoundary,
		CreatedAt:       model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func (s *Service) projectTarget(model persistence.ExecutionTarget) Target {
	target := toTarget(model)
	if (model.Kind != string(platform.TargetKubernetes) && model.Kind != string(platform.TargetDocker)) ||
		len(model.ConfigurationEncrypted) == 0 || s.cipher == nil {
		return target
	}
	decoded, err := s.cipher.Decrypt(model.ConfigurationEncrypted)
	if err != nil {
		return target
	}
	var configuration struct {
		AllocationBackend string                         `json:"allocationBackend"`
		RuntimeIsolation  *runtimeIsolationConfiguration `json:"runtimeIsolation"`
	}
	if err := json.Unmarshal([]byte(decoded), &configuration); err != nil {
		return target
	}
	if model.Kind == string(platform.TargetDocker) {
		dockerConfiguration := dockerTargetConfiguration{RuntimeIsolation: configuration.RuntimeIsolation}
		if err := normalizeDockerRuntimeIsolationConfiguration(&dockerConfiguration); err != nil {
			return target
		}
		target.RuntimeIsolationPolicy = runtimeIsolationPolicyView(dockerConfiguration.RuntimeIsolation)
		return target
	}
	full := kubernetesTargetConfiguration{
		AllocationBackend: configuration.AllocationBackend,
		RuntimeIsolation:  configuration.RuntimeIsolation,
	}
	if full.AllocationBackend == "" {
		full.AllocationBackend = string(kubernetesAllocationBackendNativePod)
	}
	if err := normalizeKubernetesRuntimeIsolationConfiguration(&full); err != nil {
		return target
	}
	target.RuntimeIsolationPolicy = runtimeIsolationPolicyView(full.RuntimeIsolation)
	return target
}

func (s *Service) projectTargetRuntimeIsolationStatus(
	ctx context.Context,
	tenantID uuid.UUID,
	target Target,
) (Target, error) {
	targets, err := s.projectTargetRuntimeIsolationStatuses(ctx, tenantID, []Target{target})
	if err != nil {
		return Target{}, err
	}
	return targets[0], nil
}

func (s *Service) projectTargetRuntimeIsolationStatuses(
	ctx context.Context,
	tenantID uuid.UUID,
	targets []Target,
) ([]Target, error) {
	indices := make(map[uuid.UUID]int, len(targets))
	ids := make([]uuid.UUID, 0, len(targets))
	for index := range targets {
		if targets[index].Kind != string(platform.TargetKubernetes) && targets[index].Kind != string(platform.TargetDocker) {
			continue
		}
		indices[targets[index].ID] = index
		ids = append(ids, targets[index].ID)
	}
	if len(ids) == 0 {
		return targets, nil
	}
	var observations []persistence.ExecutionTargetRuntimeIsolationObservation
	if err := s.db.WithContext(ctx).Where("execution_target_id IN ?", ids).Find(&observations).Error; err != nil {
		return nil, problem.Wrap(500, "runtime_isolation_observation_load_failed", "Runtime isolation availability could not be loaded.", err)
	}
	now := time.Now().UTC()
	for _, observation := range observations {
		index, found := indices[observation.ExecutionTargetID]
		if !found {
			continue
		}
		state := observation.State
		reasonCode := observation.ReasonCode
		if !observation.ExpiresAt.After(now) {
			state = "stale"
			reasonCode = runtimeIsolationStringPointer("runtime_isolation_observation_expired")
		}
		targets[index].RuntimeIsolationStatus = &RuntimeIsolationStatusView{
			State: state, DetectedRuntimes: append([]string(nil), observation.DetectedRuntimes...),
			DetectedProfiles: append([]string(nil), observation.DetectedProfiles...), ReasonCode: reasonCode,
			ObservedAt: observation.ObservedAt, ExpiresAt: observation.ExpiresAt,
		}
	}

	type runningRuntimeIsolation struct {
		ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id"`
		Generation        int64     `gorm:"column:generation"`
		EffectiveRuntime  string    `gorm:"column:effective_runtime"`
		EffectiveProfile  string    `gorm:"column:effective_profile"`
		Decision          string    `gorm:"column:decision"`
		PolicySource      string    `gorm:"column:policy_source"`
	}
	var runningItems []runningRuntimeIsolation
	if err := s.db.WithContext(ctx).
		Table("execution_runtime_isolation_decisions AS isolation").
		Select("isolation.execution_target_id, isolation.generation, isolation.effective_runtime, isolation.effective_profile, isolation.decision, isolation.policy_source").
		Joins(`JOIN agent_executions AS execution
			ON execution.tenant_id = isolation.tenant_id
			AND execution.id = isolation.execution_id
			AND execution.generation = isolation.generation`).
		Where("isolation.tenant_id = ? AND isolation.execution_target_id IN ?", tenantID, ids).
		Where("execution.status IN ?", []string{"queued", "recovering", "leased", "running", "waiting-for-approval"}).
		Order("isolation.created_at DESC").
		Find(&runningItems).Error; err != nil {
		return nil, problem.Wrap(500, "runtime_isolation_decision_load_failed", "Running runtime isolation state could not be loaded.", err)
	}
	seenRunning := make(map[uuid.UUID]struct{}, len(runningItems))
	for _, running := range runningItems {
		if _, seen := seenRunning[running.ExecutionTargetID]; seen {
			continue
		}
		index, found := indices[running.ExecutionTargetID]
		if !found {
			continue
		}
		seenRunning[running.ExecutionTargetID] = struct{}{}
		if targets[index].RuntimeIsolationStatus == nil {
			targets[index].RuntimeIsolationStatus = &RuntimeIsolationStatusView{
				State: "unattested", DetectedRuntimes: []string{}, DetectedProfiles: []string{},
			}
		}
		targets[index].RuntimeIsolationStatus.RunningGeneration = &RuntimeIsolationEffectiveView{
			Generation: running.Generation, EffectiveRuntime: running.EffectiveRuntime,
			EffectiveProfile: platform.ExecutionTargetIsolationProfile(running.EffectiveProfile),
			Decision:         running.Decision, PolicySource: running.PolicySource,
		}
	}
	return targets, nil
}

func runtimeIsolationPolicyView(policy *runtimeIsolationConfiguration) *RuntimeIsolationPolicyView {
	if policy == nil {
		return nil
	}
	requestedRuntime := policy.Runtime
	if policy.Mode == runtimeIsolationModeAuto {
		requestedRuntime = "auto"
	}
	return &RuntimeIsolationPolicyView{
		Mode: policy.Mode, RequestedRuntime: requestedRuntime,
		Preferred: append([]string{}, policy.Preferred...), MinimumProfile: policy.MinimumProfile,
		FallbackPolicy: policy.FallbackPolicy, RuntimeClassName: policy.RuntimeClassName,
		GVisorCompatibleProviders: append([]string{}, policy.GVisorCompatibleProviders...),
	}
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}
func normalizeExecutionTargetCapabilities(capabilities map[string]any) (map[string]any, error) {
	normalized, err := normalizeProviderPolicyCapabilities(capabilities)
	if err != nil {
		return nil, err
	}
	return normalizeProcessContainmentPolicyCapabilities(normalized)
}

func invalidateWorkerManifestsForCapabilityPolicyChange(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	now time.Time,
	reason string,
) error {
	return tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where("execution_target_id = ? AND current_manifest_id IS NOT NULL AND administrative_status <> ? AND terminated_at IS NULL", targetID, "revoked").
		Updates(map[string]any{
			"compatibility_status":     "incompatible",
			"compatibility_reason":     reason,
			"compatibility_checked_at": now,
		}).Error
}

func validatePublicCapabilities(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			for _, sensitive := range []string{"secret", "password", "token", "credential", "privatekey", "private_key"} {
				if strings.Contains(normalized, sensitive) {
					return problem.New(400, "unsafe_execution_target_capability", "Secrets belong in configuration, not public capabilities.")
				}
			}
			if err := validatePublicCapabilities(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validatePublicCapabilities(child); err != nil {
				return err
			}
		}
	}
	return nil
}

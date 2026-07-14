package executions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

const (
	providerHostProtocolMajor          = 2
	providerHostProtocolMinimumMinor   = 1
	workerManifestStorageSchemaVersion = 2
)

var stage3ProviderNames = providercatalog.ProviderNames()

var stage3ProviderCapabilityIDs = providercatalog.CapabilityIDs()

type workerRuntimeCapability struct {
	WorkerBuildVersion    string  `json:"workerBuildVersion"`
	WorkerBuildGitSHA     *string `json:"workerBuildGitSha,omitempty"`
	WorkerProtocolMinimum int     `json:"workerProtocolMinimum"`
	WorkerProtocolMaximum int     `json:"workerProtocolMaximum"`
	RuntimeEventMinimum   int     `json:"runtimeEventMinimum"`
	RuntimeEventMaximum   int     `json:"runtimeEventMaximum"`
	OperatingSystem       string  `json:"operatingSystem"`
	Architecture          string  `json:"architecture"`
	ImageDigest           *string `json:"imageDigest,omitempty"`
}

type providerHostCapabilitySummary struct {
	ProtocolVersion protocolVersionCapability                   `json:"protocolVersion"`
	Legacy          bool                                        `json:"legacy"`
	Providers       map[string]providerHostDescriptorCapability `json:"providers"`
}

type protocolVersionCapability struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

type providerHostDescriptorCapability struct {
	ProtocolVersion         protocolVersionCapability    `json:"protocolVersion"`
	HostBuildVersion        string                       `json:"hostBuildVersion"`
	CapabilityDescriptor    providerCapabilityDescriptor `json:"capabilityDescriptor"`
	MaximumCommandBytes     int                          `json:"maximumCommandBytes"`
	MaximumMessageBytes     int                          `json:"maximumMessageBytes"`
	RuntimeEventVersions    providerHostVersionRange     `json:"runtimeEventVersions"`
	CredentialDeliveryModes []string                     `json:"credentialDeliveryModes"`
	ResumeStrategies        []string                     `json:"resumeStrategies"`
}

type providerHostVersionRange struct {
	Minimum int `json:"minimum"`
	Maximum int `json:"maximum"`
}

type providerCapabilityDescriptor struct {
	Provider           string                    `json:"provider"`
	SupportTier        string                    `json:"supportTier"`
	AdapterVersion     string                    `json:"adapterVersion"`
	ProviderCLIVersion *string                   `json:"providerCliVersion,omitempty"`
	Runtime            providerRuntimeCapability `json:"runtime"`
	ReleasePolicy      providerReleasePolicy     `json:"releasePolicy"`
	Capabilities       map[string]string         `json:"capabilities"`
}

type providerRuntimeCapability struct {
	Kind            string                      `json:"kind"`
	Name            string                      `json:"name"`
	Version         *string                     `json:"version,omitempty"`
	Available       *bool                       `json:"available"`
	VersionSource   string                      `json:"versionSource"`
	CompatibleRange providerRuntimeVersionRange `json:"compatibleRange"`
	Compatible      *bool                       `json:"compatible"`
}

type providerRuntimeVersionRange struct {
	MinimumInclusive string  `json:"minimumInclusive"`
	MaximumExclusive *string `json:"maximumExclusive,omitempty"`
}

type providerReleasePolicy struct {
	RequiresExplicitEnablement *bool `json:"requiresExplicitEnablement"`
	Enabled                    *bool `json:"enabled"`
}

type normalizedWorkerManifest struct {
	Manifest  persistence.WorkerManifest
	Providers []persistence.WorkerProviderManifest
	Status    string
	Reason    *string
}

type workerManifestHashPayload struct {
	StorageSchemaVersion int
	Runtime              workerRuntimeCapability
	ProviderHost         providerHostCapabilitySummary
	FeatureFlags         map[string]any
}

func normalizeWorkerManifest(
	version string,
	capabilities map[string]any,
	targetCapabilities map[string]any,
	targetKind platform.ExecutionTargetKind,
	now time.Time,
) (*normalizedWorkerManifest, error) {
	providerPolicy, err := executiontargets.ParseProviderPolicy(targetCapabilities)
	if err != nil {
		return nil, err
	}
	rawProviderHost, found := capabilities["providerHost"]
	if !found {
		return nil, nil
	}
	var providerHost providerHostCapabilitySummary
	if err := decodeCapability(rawProviderHost, &providerHost); err != nil {
		return nil, problem.New(400, "invalid_worker_manifest", "providerHost capability is invalid.")
	}
	if providerHost.Legacy {
		return nil, nil
	}
	if providerHost.ProtocolVersion.Major <= 0 || providerHost.ProtocolVersion.Minor < 0 || len(providerHost.Providers) == 0 {
		return nil, problem.New(400, "invalid_worker_manifest", "Provider Host v2 summary is incomplete.")
	}
	if err := validateStage3ProviderSet(providerHost.Providers); err != nil {
		return nil, err
	}
	rawRuntime, found := capabilities["workerRuntime"]
	if !found {
		return nil, problem.New(400, "invalid_worker_manifest", "workerRuntime capability is required for Provider Host v2.")
	}
	var runtime workerRuntimeCapability
	if err := decodeCapability(rawRuntime, &runtime); err != nil {
		return nil, problem.New(400, "invalid_worker_manifest", "workerRuntime capability is invalid.")
	}
	if strings.TrimSpace(runtime.WorkerBuildVersion) == "" {
		runtime.WorkerBuildVersion = version
	}
	runtime.WorkerBuildVersion = strings.TrimSpace(runtime.WorkerBuildVersion)
	runtime.OperatingSystem = strings.TrimSpace(runtime.OperatingSystem)
	runtime.Architecture = strings.TrimSpace(runtime.Architecture)
	runtime.WorkerBuildGitSHA = trimOptionalString(runtime.WorkerBuildGitSHA)
	runtime.ImageDigest = trimOptionalString(runtime.ImageDigest)
	if runtime.WorkerProtocolMinimum <= 0 || runtime.WorkerProtocolMaximum < runtime.WorkerProtocolMinimum ||
		runtime.RuntimeEventMinimum <= 0 || runtime.RuntimeEventMaximum < runtime.RuntimeEventMinimum ||
		strings.TrimSpace(runtime.OperatingSystem) == "" || strings.TrimSpace(runtime.Architecture) == "" {
		return nil, problem.New(400, "invalid_worker_manifest", "workerRuntime compatibility ranges are invalid.")
	}
	providerModels := make([]persistence.WorkerProviderManifest, 0, len(stage3ProviderNames))
	compatibleProviders := 0
	for _, provider := range stage3ProviderNames {
		descriptor := providerHost.Providers[provider]
		if descriptor.ProtocolVersion != providerHost.ProtocolVersion {
			return nil, problem.New(400, "invalid_worker_manifest", "Provider descriptor Protocol version does not match the Provider Host summary.")
		}
		model, err := normalizeProviderManifest(provider, descriptor, targetKind, providerPolicy, now)
		if err != nil {
			return nil, err
		}
		if model.CompatibilityStatus == "compatible" {
			compatibleProviders++
		}
		providerModels = append(providerModels, model)
	}
	featureFlags := map[string]any{}
	if value, ok := capabilities["featureFlags"].(map[string]any); ok {
		featureFlags = value
	}
	hashPayload := workerManifestHashPayload{
		StorageSchemaVersion: workerManifestStorageSchemaVersion,
		Runtime:              runtime,
		ProviderHost:         providerHost,
		FeatureFlags:         featureFlags,
	}
	manifestHash, err := canonicalHash(hashPayload)
	if err != nil {
		return nil, problem.Wrap(500, "worker_manifest_hash_failed", "Failed to hash the Worker manifest.", err)
	}
	manifestID := uuid.New()
	manifest := persistence.WorkerManifest{
		ID: manifestID, ManifestHash: manifestHash, WorkerBuildVersion: runtime.WorkerBuildVersion,
		WorkerBuildGitSHA:     runtime.WorkerBuildGitSHA,
		WorkerProtocolMinimum: runtime.WorkerProtocolMinimum, WorkerProtocolMaximum: runtime.WorkerProtocolMaximum,
		RuntimeEventMinimum: runtime.RuntimeEventMinimum, RuntimeEventMaximum: runtime.RuntimeEventMaximum,
		OperatingSystem: runtime.OperatingSystem, Architecture: runtime.Architecture,
		ImageDigest: runtime.ImageDigest, FeatureFlags: featureFlags, CreatedAt: now,
	}
	for index := range providerModels {
		providerModels[index].WorkerManifestID = manifestID
	}
	status := "compatible"
	var reason *string
	if runtime.WorkerProtocolMinimum > WorkerProtocolVersion || runtime.WorkerProtocolMaximum < WorkerProtocolVersion {
		status = "incompatible"
		value := "Worker runtime does not support Worker Protocol version 2."
		reason = &value
	} else if runtime.RuntimeEventMinimum > RuntimeEventVersionV2 || runtime.RuntimeEventMaximum < RuntimeEventVersionV2 {
		status = "incompatible"
		value := "Worker runtime does not support Runtime Event version 2."
		reason = &value
	} else if providerHost.ProtocolVersion.Major != providerHostProtocolMajor ||
		providerHost.ProtocolVersion.Minor < providerHostProtocolMinimumMinor {
		status = "incompatible"
		value := "Worker Provider Host does not support Protocol version 2.1."
		reason = &value
	} else if compatibleProviders == 0 {
		status = "incompatible"
		value := "No enabled Provider has a compatible send-turn capability on this Worker manifest."
		reason = &value
	}
	return &normalizedWorkerManifest{Manifest: manifest, Providers: providerModels, Status: status, Reason: reason}, nil
}

func normalizeProviderManifest(
	provider string,
	descriptor providerHostDescriptorCapability,
	targetKind platform.ExecutionTargetKind,
	providerPolicy executiontargets.ProviderPolicy,
	now time.Time,
) (persistence.WorkerProviderManifest, error) {
	capability := descriptor.CapabilityDescriptor
	canonicalProvider, validProvider := executiontargets.CanonicalStage3Provider(provider)
	if !validProvider || provider != canonicalProvider || capability.Provider != canonicalProvider ||
		strings.TrimSpace(capability.AdapterVersion) == "" || strings.TrimSpace(descriptor.HostBuildVersion) == "" ||
		capability.Capabilities == nil {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Provider descriptor is incomplete.")
	}
	if !validProviderSupportTier(capability.SupportTier) {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Provider descriptor contains an unsupported Provider or support tier.")
	}
	if err := validateProviderCapabilityMatrix(capability.Capabilities); err != nil {
		return persistence.WorkerProviderManifest{}, err
	}
	runtime := capability.Runtime
	runtime.Kind = strings.TrimSpace(runtime.Kind)
	runtime.Name = strings.TrimSpace(runtime.Name)
	runtime.Version = trimOptionalString(runtime.Version)
	runtime.VersionSource = strings.TrimSpace(runtime.VersionSource)
	runtime.CompatibleRange.MinimumInclusive = strings.TrimSpace(runtime.CompatibleRange.MinimumInclusive)
	runtime.CompatibleRange.MaximumExclusive = trimOptionalString(runtime.CompatibleRange.MaximumExclusive)
	if !containsString([]string{"cli", "sdk", "local"}, runtime.Kind) || runtime.Name == "" ||
		!containsString([]string{"probe", "package", "build"}, runtime.VersionSource) ||
		runtime.Available == nil || runtime.Compatible == nil ||
		capability.ReleasePolicy.RequiresExplicitEnablement == nil || capability.ReleasePolicy.Enabled == nil {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Provider runtime or release policy descriptor is incomplete.")
	}
	minimum, minimumValid := parseSemanticVersion(runtime.CompatibleRange.MinimumInclusive)
	if !minimumValid {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Provider runtime minimum version is invalid.")
	}
	var maximum *semanticVersion
	if runtime.CompatibleRange.MaximumExclusive != nil {
		parsed, valid := parseSemanticVersion(*runtime.CompatibleRange.MaximumExclusive)
		if !valid || compareSemanticVersion(parsed, minimum) <= 0 {
			return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Provider runtime maximum version is invalid.")
		}
		maximum = &parsed
	}
	runtimeAvailable := *runtime.Available
	reportedRuntimeCompatible := *runtime.Compatible
	if reportedRuntimeCompatible && (!runtimeAvailable || runtime.Version == nil) {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Compatible Provider runtime must be available and report a version.")
	}
	computedRuntimeCompatible := false
	if runtimeAvailable && runtime.Version != nil {
		parsed, valid := parseSemanticVersion(*runtime.Version)
		if valid && compareSemanticVersion(parsed, minimum) >= 0 &&
			(maximum == nil || compareSemanticVersion(parsed, *maximum) < 0) {
			computedRuntimeCompatible = true
		}
	}
	if capability.ProviderCLIVersion != nil {
		capability.ProviderCLIVersion = trimOptionalString(capability.ProviderCLIVersion)
		if capability.ProviderCLIVersion == nil || runtime.Version == nil || *capability.ProviderCLIVersion != *runtime.Version {
			return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Legacy Provider version does not match the normalized runtime version.")
		}
	}
	legacyProviderVersion := capability.ProviderCLIVersion
	requiresExplicitEnablement := *capability.ReleasePolicy.RequiresExplicitEnablement
	releaseEnabled := *capability.ReleasePolicy.Enabled
	expectedReleaseEnabled := true
	if capability.SupportTier == "experimental" {
		if !requiresExplicitEnablement {
			return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Experimental Provider must require explicit enablement.")
		}
		expectedReleaseEnabled = providerPolicy.ExperimentalProviderEnabled(provider)
	} else if requiresExplicitEnablement {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Only Experimental Providers may require explicit enablement.")
	}
	if releaseEnabled != expectedReleaseEnabled {
		err := problem.New(409, "worker_provider_policy_mismatch", "Provider Host enablement does not match the authoritative Execution Target Provider policy.")
		err.Details = map[string]any{
			"provider": provider, "expectedEnabled": expectedReleaseEnabled, "reportedEnabled": releaseEnabled,
		}
		return persistence.WorkerProviderManifest{}, err
	}
	descriptorHash, err := canonicalHash(capability)
	if err != nil {
		return persistence.WorkerProviderManifest{}, problem.Wrap(500, "provider_descriptor_hash_failed", "Failed to hash the Provider descriptor.", err)
	}
	status := "compatible"
	var code, message *string
	switch {
	case capability.SupportTier == "local-only" && platform.IsRemoteTarget(targetKind):
		status = "local-only"
		code, message = stringReference("capability_unsupported"), stringReference("Provider is Local-only on remote Workers.")
	case capability.SupportTier == "experimental" && !releaseEnabled:
		status = "disabled"
		code, message = stringReference("capability_unsupported"), stringReference("Experimental Provider is disabled by the Execution Target Provider policy.")
	case descriptor.ProtocolVersion.Major != providerHostProtocolMajor || descriptor.ProtocolVersion.Minor < providerHostProtocolMinimumMinor:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider Host Protocol version 2.1 is required.")
	case !runtimeAvailable:
		status = "unavailable"
		code, message = stringReference("provider_not_installed"), stringReference("Provider runtime is unavailable on this Worker.")
	case !computedRuntimeCompatible || !reportedRuntimeCompatible:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider runtime version is outside the compatible release range.")
	case !isSupportedProviderCapability(capability.Capabilities["send-turn"]):
		status = "incompatible"
		code, message = stringReference("capability_unsupported"), stringReference("Provider does not declare a supported send-turn capability.")
	case descriptor.MaximumCommandBytes <= 0 || descriptor.MaximumMessageBytes <= 0 ||
		descriptor.RuntimeEventVersions.Minimum > RuntimeEventVersionV2 ||
		descriptor.RuntimeEventVersions.Maximum < RuntimeEventVersionV2:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider Host message or Runtime Event range is incompatible.")
	case !containsCapabilityString(descriptor.CredentialDeliveryModes, "anonymous-fd") || len(descriptor.ResumeStrategies) == 0:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider Host Credential or Resume strategy is incompatible.")
	}
	return persistence.WorkerProviderManifest{
		Provider: providerStorageName(canonicalProvider), SupportTier: capability.SupportTier, CompatibilityStatus: status,
		ProviderHostMajor: descriptor.ProtocolVersion.Major, ProviderHostMinor: descriptor.ProtocolVersion.Minor,
		HostBuildVersion: strings.TrimSpace(descriptor.HostBuildVersion), AdapterVersion: strings.TrimSpace(capability.AdapterVersion),
		ProviderCLIVersion: legacyProviderVersion,
		RuntimeKind:        runtime.Kind, RuntimeName: runtime.Name, RuntimeVersion: runtime.Version,
		RuntimeAvailable: runtimeAvailable, RuntimeVersionSource: runtime.VersionSource,
		RuntimeMinimumInclusive:           runtime.CompatibleRange.MinimumInclusive,
		RuntimeMaximumExclusive:           runtime.CompatibleRange.MaximumExclusive,
		RuntimeCompatible:                 computedRuntimeCompatible && reportedRuntimeCompatible,
		ReleaseRequiresExplicitEnablement: requiresExplicitEnablement, ReleaseEnabled: releaseEnabled,
		MaximumCommandBytes: descriptor.MaximumCommandBytes, MaximumMessageBytes: descriptor.MaximumMessageBytes,
		RuntimeEventMinimum: descriptor.RuntimeEventVersions.Minimum, RuntimeEventMaximum: descriptor.RuntimeEventVersions.Maximum,
		CredentialDeliveryModes: descriptor.CredentialDeliveryModes, ResumeStrategies: descriptor.ResumeStrategies,
		CapabilityDescriptorHash: descriptorHash,
		Capabilities:             stringMapToAny(capability.Capabilities), IncompatibilityCode: code,
		IncompatibilityMessage: message, CheckedAt: now,
	}, nil
}

func providerStorageName(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func persistWorkerManifest(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	version string,
	capabilities map[string]any,
	targetCapabilities map[string]any,
	targetKind platform.ExecutionTargetKind,
	now time.Time,
) error {
	normalized, err := normalizeWorkerManifest(version, capabilities, targetCapabilities, targetKind, now)
	if err != nil {
		return err
	}
	if normalized == nil {
		updates := map[string]any{
			"current_manifest_id": nil, "compatibility_status": "unknown",
			"compatibility_reason": nil, "compatibility_checked_at": nil,
		}
		if err := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ?", worker.ID).Updates(updates).Error; err != nil {
			return problem.Wrap(500, "worker_manifest_clear_failed", "Failed to clear the obsolete Worker manifest.", err)
		}
		worker.CurrentManifestID = nil
		worker.CompatibilityStatus = "unknown"
		worker.CompatibilityReason = nil
		worker.CompatibilityCheckedAt = nil
		return nil
	}
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "manifest_hash"}}, DoNothing: true,
	}).Create(&normalized.Manifest)
	if result.Error != nil {
		return problem.Wrap(500, "worker_manifest_store_failed", "Failed to store the Worker manifest.", result.Error)
	}
	if result.RowsAffected == 0 {
		var stored persistence.WorkerManifest
		if err := tx.WithContext(ctx).Where("manifest_hash = ?", normalized.Manifest.ManifestHash).
			Take(&stored).Error; err != nil {
			return problem.Wrap(500, "worker_manifest_reload_failed", "Failed to reload the Worker manifest.", err)
		}
		normalized.Manifest = stored
	}
	for index := range normalized.Providers {
		normalized.Providers[index].WorkerManifestID = normalized.Manifest.ID
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
			Create(&normalized.Providers[index]).Error; err != nil {
			return problem.Wrap(500, "provider_manifest_store_failed", "Failed to store a Provider manifest.", err)
		}
	}
	updates := map[string]any{
		"current_manifest_id": normalized.Manifest.ID, "compatibility_status": normalized.Status,
		"compatibility_reason": normalized.Reason, "compatibility_checked_at": now,
	}
	if err := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where("id = ?", worker.ID).Updates(updates).Error; err != nil {
		return problem.Wrap(500, "worker_manifest_attach_failed", "Failed to attach the Worker manifest.", err)
	}
	worker.CurrentManifestID = &normalized.Manifest.ID
	worker.CompatibilityStatus = normalized.Status
	worker.CompatibilityReason = normalized.Reason
	worker.CompatibilityCheckedAt = &now
	return nil
}

func workerManifestMatches(
	ctx context.Context,
	db *gorm.DB,
	worker persistence.WorkerInstance,
	normalized *normalizedWorkerManifest,
) (bool, error) {
	if normalized == nil {
		return worker.CurrentManifestID == nil, nil
	}
	if worker.CurrentManifestID == nil || worker.CompatibilityStatus != normalized.Status {
		return false, nil
	}
	var stored persistence.WorkerManifest
	if err := db.WithContext(ctx).Where("id = ?", *worker.CurrentManifestID).Take(&stored).Error; err != nil {
		return false, problem.Wrap(500, "worker_manifest_lookup_failed", "Failed to inspect the registered Worker manifest.", err)
	}
	if stored.ManifestHash != normalized.Manifest.ManifestHash {
		return false, nil
	}
	return true, nil
}

func workerManifestReregistrationRequired(reason string) *problem.Error {
	err := problem.New(409, "worker_manifest_reregistration_required", "Worker Provider capabilities changed; re-register the Worker before sending more heartbeats.")
	if reason = strings.TrimSpace(reason); reason != "" {
		err.Details = map[string]any{"reason": reason}
	}
	return err
}

func workerSupportsProvider(tx *gorm.DB, worker persistence.WorkerInstance, provider *string) (bool, error) {
	if provider == nil || strings.TrimSpace(*provider) == "" {
		return true, nil
	}
	if worker.CompatibilityStatus != "compatible" || worker.CurrentManifestID == nil {
		return false, nil
	}
	var count int64
	err := tx.Model(&persistence.WorkerProviderManifest{}).
		Where("worker_manifest_id = ? AND provider = ? AND compatibility_status = ?",
			*worker.CurrentManifestID, *provider, "compatible").Count(&count).Error
	return count == 1, err
}

func bindExecutionRuntimeResources(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	execution persistence.AgentExecution,
	now time.Time,
) error {
	if execution.ProviderRuntimeBindingID != nil && execution.WorkerManifestID != nil && execution.Provider != nil {
		var providerManifest persistence.WorkerProviderManifest
		if err := tx.WithContext(ctx).
			Where("worker_manifest_id = ? AND provider = ?", *execution.WorkerManifestID, *execution.Provider).
			Take(&providerManifest).Error; err != nil {
			return problem.Wrap(409, "worker_provider_incompatible", "The Worker manifest does not support the Execution Provider.", err)
		}
		resumeStrategy := "authoritative-history"
		if support, _ := providerManifest.Capabilities["resume-session"].(string); support == "native" {
			resumeStrategy = "native-cursor"
		}
		updates := map[string]any{
			"worker_manifest_id":                   providerManifest.WorkerManifestID,
			"capability_descriptor_hash":           providerManifest.CapabilityDescriptorHash,
			"provider_host_protocol_major":         providerManifest.ProviderHostMajor,
			"provider_host_protocol_minor":         providerManifest.ProviderHostMinor,
			"adapter_version":                      providerManifest.AdapterVersion,
			"provider_cli_version":                 providerManifest.ProviderCLIVersion,
			"runtime_kind":                         providerManifest.RuntimeKind,
			"runtime_name":                         providerManifest.RuntimeName,
			"runtime_version":                      providerManifest.RuntimeVersion,
			"runtime_available":                    providerManifest.RuntimeAvailable,
			"runtime_version_source":               providerManifest.RuntimeVersionSource,
			"runtime_minimum_inclusive":            providerManifest.RuntimeMinimumInclusive,
			"runtime_maximum_exclusive":            providerManifest.RuntimeMaximumExclusive,
			"runtime_compatible":                   providerManifest.RuntimeCompatible,
			"release_requires_explicit_enablement": providerManifest.ReleaseRequiresExplicitEnablement,
			"release_enabled":                      providerManifest.ReleaseEnabled,
			"resume_strategy":                      resumeStrategy,
			"last_execution_id":                    execution.ID,
			"last_generation":                      execution.Generation,
			"status":                               "active",
			"updated_at":                           now,
		}
		if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ? AND session_id = ? AND provider = ?",
				execution.TenantID, *execution.ProviderRuntimeBindingID, execution.SessionID, *execution.Provider).
			Updates(updates).Error; err != nil {
			return problem.Wrap(500, "runtime_binding_manifest_update_failed", "Failed to bind the Provider runtime to the Worker manifest.", err)
		}
	}
	if execution.RemoteWorkspaceID != nil {
		if _, err := ensureExecutionWorkspaceMaterialization(ctx, tx, worker, &execution, now); err != nil {
			return err
		}
		workspaceState := "preparing"
		if execution.RestoreCheckpointID != nil {
			workspaceState = "recovering"
		}
		updates := map[string]any{
			"last_worker_id": worker.ID, "last_execution_id": execution.ID,
			"last_generation": execution.Generation, "state": workspaceState,
			"last_used_at": now, "updated_at": now, "cleaned_at": nil,
		}
		updated := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND session_id = ? AND execution_target_id = ?",
				execution.TenantID, *execution.RemoteWorkspaceID, execution.SessionID, execution.ExecutionTargetID).
			Updates(updates)
		if err := expectOne(updated, 409, "remote_workspace_execution_bind_failed", "Failed to bind the Session Workspace to the Execution."); err != nil {
			return err
		}
	}
	return nil
}

func decodeCapability(input any, output any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, output)
}

func canonicalHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func stringMapToAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func containsCapabilityString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func isSupportedProviderCapability(value any) bool {
	support, _ := value.(string)
	return support == "native" || support == "emulated"
}

func stringReference(value string) *string { return &value }

func validateStage3ProviderSet(providers map[string]providerHostDescriptorCapability) error {
	if len(providers) != len(stage3ProviderNames) {
		return problem.New(400, "invalid_worker_manifest", "Provider Host summary must contain the complete Stage 3 Provider set.")
	}
	for _, provider := range stage3ProviderNames {
		if _, found := providers[provider]; !found {
			return problem.New(400, "invalid_worker_manifest", "Provider Host summary is missing a required Stage 3 Provider.")
		}
	}
	for provider := range providers {
		if !containsString(stage3ProviderNames, provider) {
			return problem.New(400, "invalid_worker_manifest", "Provider Host summary contains an unknown Stage 3 Provider.")
		}
	}
	return nil
}

func validateProviderCapabilityMatrix(capabilities map[string]string) error {
	if len(capabilities) != len(stage3ProviderCapabilityIDs) {
		return problem.New(400, "invalid_worker_manifest", "Provider capability descriptor must contain every Stage 3 Capability ID exactly once.")
	}
	for _, capabilityID := range stage3ProviderCapabilityIDs {
		status, found := capabilities[capabilityID]
		if !found || !containsString([]string{"native", "emulated", "unsupported"}, status) {
			return problem.New(400, "invalid_worker_manifest", "Provider capability descriptor contains a missing or invalid capability status.")
		}
	}
	for capabilityID := range capabilities {
		if !containsString(stage3ProviderCapabilityIDs, capabilityID) {
			return problem.New(400, "invalid_worker_manifest", "Provider capability descriptor contains an unknown Capability ID.")
		}
	}
	return nil
}

func validProviderSupportTier(value string) bool {
	return containsString([]string{"tier-1", "tier-2", "experimental", "local-only"}, value)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func trimOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	return &normalized
}

type semanticVersion struct {
	major      int
	minor      int
	patch      int
	prerelease []string
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if value == "" || strings.Count(value, "+") > 1 {
		return semanticVersion{}, false
	}
	versionAndBuild := strings.SplitN(value, "+", 2)
	if len(versionAndBuild) == 2 && !validSemanticVersionIdentifiers(versionAndBuild[1], false) {
		return semanticVersion{}, false
	}
	coreAndPrerelease := strings.SplitN(versionAndBuild[0], "-", 2)
	parts := strings.Split(coreAndPrerelease[0], ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	values := [3]int{}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return semanticVersion{}, false
		}
		values[index] = parsed
	}
	version := semanticVersion{major: values[0], minor: values[1], patch: values[2]}
	if len(coreAndPrerelease) == 2 {
		if !validSemanticVersionIdentifiers(coreAndPrerelease[1], true) {
			return semanticVersion{}, false
		}
		version.prerelease = strings.Split(coreAndPrerelease[1], ".")
	}
	return version, true
}

func compareSemanticVersion(left, right semanticVersion) int {
	leftValues := [3]int{left.major, left.minor, left.patch}
	rightValues := [3]int{right.major, right.minor, right.patch}
	for index := range leftValues {
		if leftValues[index] < rightValues[index] {
			return -1
		}
		if leftValues[index] > rightValues[index] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		leftPart, rightPart := left.prerelease[index], right.prerelease[index]
		leftNumber, leftNumeric := semanticVersionNumber(leftPart)
		rightNumber, rightNumeric := semanticVersionNumber(rightPart)
		switch {
		case leftNumeric && rightNumeric && leftNumber < rightNumber:
			return -1
		case leftNumeric && rightNumeric && leftNumber > rightNumber:
			return 1
		case leftNumeric && !rightNumeric:
			return -1
		case !leftNumeric && rightNumeric:
			return 1
		case leftPart < rightPart:
			return -1
		case leftPart > rightPart:
			return 1
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

func validSemanticVersionIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for _, character := range identifier {
			if (character < '0' || character > '9') &&
				(character < 'A' || character > 'Z') &&
				(character < 'a' || character > 'z') && character != '-' {
				return false
			}
		}
		if _, numeric := semanticVersionNumber(identifier); rejectNumericLeadingZero && numeric &&
			len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func semanticVersionNumber(value string) (int, bool) {
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

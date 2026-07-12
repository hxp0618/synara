package executions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

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
	Provider           string            `json:"provider"`
	SupportTier        string            `json:"supportTier"`
	AdapterVersion     string            `json:"adapterVersion"`
	ProviderCLIVersion *string           `json:"providerCliVersion,omitempty"`
	Capabilities       map[string]string `json:"capabilities"`
}

type normalizedWorkerManifest struct {
	Manifest  persistence.WorkerManifest
	Providers []persistence.WorkerProviderManifest
	Status    string
	Reason    *string
}

func normalizeWorkerManifest(
	version string,
	capabilities map[string]any,
	now time.Time,
) (*normalizedWorkerManifest, error) {
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
	if providerHost.ProtocolVersion.Major <= 0 || len(providerHost.Providers) == 0 {
		return nil, problem.New(400, "invalid_worker_manifest", "Provider Host v2 summary is incomplete.")
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
	if runtime.WorkerProtocolMinimum <= 0 || runtime.WorkerProtocolMaximum < runtime.WorkerProtocolMinimum ||
		runtime.RuntimeEventMinimum <= 0 || runtime.RuntimeEventMaximum < runtime.RuntimeEventMinimum ||
		strings.TrimSpace(runtime.OperatingSystem) == "" || strings.TrimSpace(runtime.Architecture) == "" {
		return nil, problem.New(400, "invalid_worker_manifest", "workerRuntime compatibility ranges are invalid.")
	}
	providerNames := make([]string, 0, len(providerHost.Providers))
	for provider := range providerHost.Providers {
		providerNames = append(providerNames, provider)
	}
	sort.Strings(providerNames)
	providerModels := make([]persistence.WorkerProviderManifest, 0, len(providerNames))
	compatibleProviders := 0
	for _, provider := range providerNames {
		descriptor := providerHost.Providers[provider]
		model, err := normalizeProviderManifest(provider, descriptor, now)
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
	hashPayload := struct {
		Runtime      workerRuntimeCapability
		ProviderHost providerHostCapabilitySummary
		FeatureFlags map[string]any
	}{runtime, providerHost, featureFlags}
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
	if compatibleProviders == 0 {
		status = "incompatible"
		value := "No remote Provider has a compatible send-turn capability on this Worker manifest."
		reason = &value
	}
	return &normalizedWorkerManifest{Manifest: manifest, Providers: providerModels, Status: status, Reason: reason}, nil
}

func normalizeProviderManifest(
	provider string,
	descriptor providerHostDescriptorCapability,
	now time.Time,
) (persistence.WorkerProviderManifest, error) {
	capability := descriptor.CapabilityDescriptor
	if normalizeProviderName(provider) != normalizeProviderName(capability.Provider) ||
		strings.TrimSpace(capability.AdapterVersion) == "" || strings.TrimSpace(descriptor.HostBuildVersion) == "" ||
		capability.Capabilities == nil {
		return persistence.WorkerProviderManifest{}, problem.New(400, "invalid_worker_manifest", "Provider descriptor is incomplete.")
	}
	descriptorHash, err := canonicalHash(capability)
	if err != nil {
		return persistence.WorkerProviderManifest{}, problem.Wrap(500, "provider_descriptor_hash_failed", "Failed to hash the Provider descriptor.", err)
	}
	status := "compatible"
	var code, message *string
	switch {
	case capability.SupportTier == "local-only":
		status = "local-only"
		code, message = stringReference("capability_unsupported"), stringReference("Provider is Local-only on remote Workers.")
	case descriptor.ProtocolVersion.Major != 2:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider Host Protocol major is incompatible.")
	case capability.ProviderCLIVersion == nil || strings.EqualFold(strings.TrimSpace(*capability.ProviderCLIVersion), "unavailable"):
		status = "unavailable"
		code, message = stringReference("provider_not_installed"), stringReference("Provider CLI is unavailable on this Worker.")
	case capability.Capabilities["send-turn"] != "native" && capability.Capabilities["send-turn"] != "emulated":
		status = "incompatible"
		code, message = stringReference("capability_unsupported"), stringReference("Provider does not declare a supported send-turn capability.")
	case descriptor.MaximumCommandBytes <= 0 || descriptor.MaximumMessageBytes <= 0 ||
		descriptor.RuntimeEventVersions.Minimum > 1 || descriptor.RuntimeEventVersions.Maximum < 1:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider Host message or Runtime Event range is incompatible.")
	case !containsCapabilityString(descriptor.CredentialDeliveryModes, "anonymous-fd") || len(descriptor.ResumeStrategies) == 0:
		status = "incompatible"
		code, message = stringReference("provider_version_incompatible"), stringReference("Provider Host Credential or Resume strategy is incompatible.")
	}
	return persistence.WorkerProviderManifest{
		Provider: capability.Provider, SupportTier: capability.SupportTier, CompatibilityStatus: status,
		ProviderHostMajor: descriptor.ProtocolVersion.Major, ProviderHostMinor: descriptor.ProtocolVersion.Minor,
		HostBuildVersion: descriptor.HostBuildVersion, AdapterVersion: capability.AdapterVersion,
		ProviderCLIVersion:  capability.ProviderCLIVersion,
		MaximumCommandBytes: descriptor.MaximumCommandBytes, MaximumMessageBytes: descriptor.MaximumMessageBytes,
		RuntimeEventMinimum: descriptor.RuntimeEventVersions.Minimum, RuntimeEventMaximum: descriptor.RuntimeEventVersions.Maximum,
		CredentialDeliveryModes: descriptor.CredentialDeliveryModes, ResumeStrategies: descriptor.ResumeStrategies,
		CapabilityDescriptorHash: descriptorHash,
		Capabilities:             stringMapToAny(capability.Capabilities), IncompatibilityCode: code,
		IncompatibilityMessage: message, CheckedAt: now,
	}, nil
}

func persistWorkerManifest(
	ctx context.Context,
	tx *gorm.DB,
	worker *persistence.WorkerInstance,
	version string,
	capabilities map[string]any,
	now time.Time,
) error {
	normalized, err := normalizeWorkerManifest(version, capabilities, now)
	if err != nil || normalized == nil {
		return err
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

func workerSupportsProvider(tx *gorm.DB, worker persistence.WorkerInstance, provider *string) (bool, error) {
	if worker.CompatibilityStatus == "incompatible" || worker.CompatibilityStatus == "revoked" {
		return false, nil
	}
	if worker.CurrentManifestID == nil || provider == nil || strings.TrimSpace(*provider) == "" {
		return true, nil
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
			"worker_manifest_id":           providerManifest.WorkerManifestID,
			"capability_descriptor_hash":   providerManifest.CapabilityDescriptorHash,
			"provider_host_protocol_major": providerManifest.ProviderHostMajor,
			"provider_host_protocol_minor": providerManifest.ProviderHostMinor,
			"adapter_version":              providerManifest.AdapterVersion,
			"provider_cli_version":         providerManifest.ProviderCLIVersion,
			"resume_strategy":              resumeStrategy,
			"last_execution_id":            execution.ID,
			"last_generation":              execution.Generation,
			"status":                       "active",
			"updated_at":                   now,
		}
		if err := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
			Where("tenant_id = ? AND id = ? AND session_id = ? AND provider = ?",
				execution.TenantID, *execution.ProviderRuntimeBindingID, execution.SessionID, *execution.Provider).
			Updates(updates).Error; err != nil {
			return problem.Wrap(500, "runtime_binding_manifest_update_failed", "Failed to bind the Provider runtime to the Worker manifest.", err)
		}
	}
	if execution.RemoteWorkspaceID != nil {
		updates := map[string]any{
			"last_worker_id": worker.ID, "last_execution_id": execution.ID,
			"last_generation": execution.Generation, "state": "preparing",
			"last_used_at": now, "updated_at": now,
		}
		if err := tx.WithContext(ctx).Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ? AND session_id = ? AND execution_target_id = ?",
				execution.TenantID, *execution.RemoteWorkspaceID, execution.SessionID, execution.ExecutionTargetID).
			Updates(updates).Error; err != nil {
			return problem.Wrap(500, "remote_workspace_execution_bind_failed", "Failed to bind the Session Workspace to the Execution.", err)
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

func normalizeProviderName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "claude" || value == "claudeagent" {
		return "claudeagent"
	}
	return value
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

func stringReference(value string) *string { return &value }

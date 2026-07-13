package executions

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type WorkerManifestProjection struct {
	ExecutionTargetID  uuid.UUID                    `json:"executionTargetId"`
	ManifestID         uuid.UUID                    `json:"manifestId"`
	WorkerStatusCounts WorkerManifestStatusCounts   `json:"workerStatusCounts"`
	LastHeartbeatAt    time.Time                    `json:"lastHeartbeatAt"`
	WorkerBuild        WorkerManifestBuild          `json:"workerBuild"`
	WorkerProtocol     WorkerManifestVersionRange   `json:"workerProtocol"`
	RuntimeEvent       WorkerManifestVersionRange   `json:"runtimeEvent"`
	Providers          []WorkerProviderManifestView `json:"providers"`
}

type WorkerManifestStatusCounts struct {
	Online   int64 `json:"online"`
	Draining int64 `json:"draining"`
	Offline  int64 `json:"offline"`
}

type WorkerManifestBuild struct {
	Version         string  `json:"version"`
	GitSHA          *string `json:"gitSha,omitempty"`
	ImageDigest     *string `json:"imageDigest,omitempty"`
	OperatingSystem string  `json:"operatingSystem"`
	Architecture    string  `json:"architecture"`
}

type WorkerManifestVersionRange struct {
	Minimum int `json:"minimum"`
	Maximum int `json:"maximum"`
}

type WorkerProviderManifestView struct {
	Provider               string                      `json:"provider"`
	SupportTier            string                      `json:"supportTier"`
	CompatibilityStatus    string                      `json:"compatibilityStatus"`
	Runtime                WorkerProviderRuntimeView   `json:"runtime"`
	ReleasePolicy          WorkerProviderReleasePolicy `json:"releasePolicy"`
	IncompatibilityCode    *string                     `json:"incompatibilityCode,omitempty"`
	IncompatibilityMessage *string                     `json:"incompatibilityMessage,omitempty"`
	Capabilities           map[string]string           `json:"capabilities"`
}

type WorkerProviderRuntimeView struct {
	Kind            string                            `json:"kind"`
	Name            string                            `json:"name"`
	Version         *string                           `json:"version,omitempty"`
	Available       bool                              `json:"available"`
	VersionSource   string                            `json:"versionSource"`
	CompatibleRange WorkerProviderRuntimeVersionRange `json:"compatibleRange"`
	Compatible      bool                              `json:"compatible"`
}

type WorkerProviderRuntimeVersionRange struct {
	MinimumInclusive string  `json:"minimumInclusive"`
	MaximumExclusive *string `json:"maximumExclusive,omitempty"`
}

type WorkerProviderReleasePolicy struct {
	RequiresExplicitEnablement bool `json:"requiresExplicitEnablement"`
	Enabled                    bool `json:"enabled"`
}

type workerManifestGroupRow struct {
	ExecutionTargetID uuid.UUID      `gorm:"column:execution_target_id"`
	ManifestID        uuid.UUID      `gorm:"column:manifest_id"`
	OnlineCount       int64          `gorm:"column:online_count"`
	DrainingCount     int64          `gorm:"column:draining_count"`
	OfflineCount      int64          `gorm:"column:offline_count"`
	LastHeartbeatAt   sql.NullString `gorm:"column:last_heartbeat_at"`
}

func (s *Service) ListWorkerManifests(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) ([]WorkerManifestProjection, error) {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return nil, problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.WorkerRead); err != nil {
		return nil, err
	}

	groups := make([]workerManifestGroupRow, 0)
	err := s.db.WithContext(ctx).
		Table("worker_instances AS worker").
		Select(`
			worker.execution_target_id AS execution_target_id,
			worker.current_manifest_id AS manifest_id,
			SUM(CASE WHEN worker.status = 'online' THEN 1 ELSE 0 END) AS online_count,
			SUM(CASE WHEN worker.status = 'draining' THEN 1 ELSE 0 END) AS draining_count,
			SUM(CASE WHEN worker.status = 'offline' THEN 1 ELSE 0 END) AS offline_count,
			CAST(MAX(worker.last_heartbeat_at) AS TEXT) AS last_heartbeat_at
		`).
		Joins("JOIN execution_targets AS target ON target.id = worker.execution_target_id").
		Where("worker.current_manifest_id IS NOT NULL").
		Where("worker.status <> ? AND worker.terminated_at IS NULL", "terminated").
		Where("target.tenant_id = ?", tenantID).
		Group("worker.execution_target_id, worker.current_manifest_id").
		Order("worker.execution_target_id, worker.current_manifest_id").
		Find(&groups).Error
	if err != nil {
		return nil, problem.Wrap(500, "worker_manifests_load_failed", "Failed to load Worker manifests.", err)
	}
	if len(groups) == 0 {
		return []WorkerManifestProjection{}, nil
	}

	manifestIDs := uniqueManifestIDs(groups)
	manifests := make([]persistence.WorkerManifest, 0, len(manifestIDs))
	if err := s.db.WithContext(ctx).Where("id IN ?", manifestIDs).Find(&manifests).Error; err != nil {
		return nil, problem.Wrap(500, "worker_manifests_load_failed", "Failed to load Worker manifests.", err)
	}
	providerModels := make([]persistence.WorkerProviderManifest, 0, len(manifestIDs)*len(stage3ProviderNames))
	if err := s.db.WithContext(ctx).
		Where("worker_manifest_id IN ?", manifestIDs).
		Order("worker_manifest_id, provider").
		Find(&providerModels).Error; err != nil {
		return nil, problem.Wrap(500, "worker_manifests_load_failed", "Failed to load Worker manifests.", err)
	}

	manifestByID := make(map[uuid.UUID]persistence.WorkerManifest, len(manifests))
	for _, manifest := range manifests {
		manifestByID[manifest.ID] = manifest
	}
	providersByManifest := make(map[uuid.UUID][]persistence.WorkerProviderManifest, len(manifestIDs))
	for _, provider := range providerModels {
		providersByManifest[provider.WorkerManifestID] = append(providersByManifest[provider.WorkerManifestID], provider)
	}

	items := make([]WorkerManifestProjection, 0, len(groups))
	for _, group := range groups {
		manifest, found := manifestByID[group.ManifestID]
		if !found {
			return nil, invalidStoredWorkerManifest()
		}
		lastHeartbeatAt, err := workerManifestHeartbeatTime(group.LastHeartbeatAt)
		if err != nil {
			return nil, problem.Wrap(500, "worker_manifests_load_failed", "Failed to load Worker manifests.", err)
		}
		providers, err := projectWorkerProviders(providersByManifest[group.ManifestID])
		if err != nil {
			return nil, err
		}
		items = append(items, WorkerManifestProjection{
			ExecutionTargetID: group.ExecutionTargetID,
			ManifestID:        group.ManifestID,
			WorkerStatusCounts: WorkerManifestStatusCounts{
				Online: group.OnlineCount, Draining: group.DrainingCount, Offline: group.OfflineCount,
			},
			LastHeartbeatAt: lastHeartbeatAt,
			WorkerBuild: WorkerManifestBuild{
				Version: manifest.WorkerBuildVersion, GitSHA: manifest.WorkerBuildGitSHA,
				ImageDigest: manifest.ImageDigest, OperatingSystem: manifest.OperatingSystem,
				Architecture: manifest.Architecture,
			},
			WorkerProtocol: WorkerManifestVersionRange{
				Minimum: manifest.WorkerProtocolMinimum, Maximum: manifest.WorkerProtocolMaximum,
			},
			RuntimeEvent: WorkerManifestVersionRange{
				Minimum: manifest.RuntimeEventMinimum, Maximum: manifest.RuntimeEventMaximum,
			},
			Providers: providers,
		})
	}
	return items, nil
}

func workerManifestHeartbeatTime(value sql.NullString) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, invalidStoredWorkerManifest()
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, value.String)
		if err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, invalidStoredWorkerManifest()
}

func uniqueManifestIDs(groups []workerManifestGroupRow) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(groups))
	result := make([]uuid.UUID, 0, len(groups))
	for _, group := range groups {
		if _, found := seen[group.ManifestID]; found {
			continue
		}
		seen[group.ManifestID] = struct{}{}
		result = append(result, group.ManifestID)
	}
	return result
}

func projectWorkerProviders(models []persistence.WorkerProviderManifest) ([]WorkerProviderManifestView, error) {
	if len(models) != len(stage3ProviderNames) {
		return nil, invalidStoredWorkerManifest()
	}
	byName := make(map[string]persistence.WorkerProviderManifest, len(models))
	for _, model := range models {
		canonical, valid := executiontargets.CanonicalStage3Provider(model.Provider)
		if !valid {
			return nil, invalidStoredWorkerManifest()
		}
		if _, duplicate := byName[canonical]; duplicate {
			return nil, invalidStoredWorkerManifest()
		}
		byName[canonical] = model
	}
	providers := make([]WorkerProviderManifestView, 0, len(stage3ProviderNames))
	for _, name := range stage3ProviderNames {
		model, found := byName[name]
		if !found {
			return nil, invalidStoredWorkerManifest()
		}
		capabilities, err := projectProviderCapabilities(model.Capabilities)
		if err != nil {
			return nil, err
		}
		providers = append(providers, WorkerProviderManifestView{
			Provider: name, SupportTier: model.SupportTier, CompatibilityStatus: model.CompatibilityStatus,
			Runtime: WorkerProviderRuntimeView{
				Kind: model.RuntimeKind, Name: model.RuntimeName, Version: model.RuntimeVersion,
				Available: model.RuntimeAvailable, VersionSource: model.RuntimeVersionSource,
				CompatibleRange: WorkerProviderRuntimeVersionRange{
					MinimumInclusive: model.RuntimeMinimumInclusive,
					MaximumExclusive: model.RuntimeMaximumExclusive,
				},
				Compatible: model.RuntimeCompatible,
			},
			ReleasePolicy: WorkerProviderReleasePolicy{
				RequiresExplicitEnablement: model.ReleaseRequiresExplicitEnablement,
				Enabled:                    model.ReleaseEnabled,
			},
			IncompatibilityCode: model.IncompatibilityCode, IncompatibilityMessage: model.IncompatibilityMessage,
			Capabilities: capabilities,
		})
	}
	return providers, nil
}

func projectProviderCapabilities(raw map[string]any) (map[string]string, error) {
	if len(raw) != len(stage3ProviderCapabilityIDs) {
		return nil, invalidStoredWorkerManifest()
	}
	capabilities := make(map[string]string, len(stage3ProviderCapabilityIDs))
	for _, capabilityID := range stage3ProviderCapabilityIDs {
		value, found := raw[capabilityID]
		if !found {
			return nil, invalidStoredWorkerManifest()
		}
		support, ok := value.(string)
		if !ok || (support != "native" && support != "emulated" && support != "unsupported") {
			return nil, invalidStoredWorkerManifest()
		}
		capabilities[capabilityID] = support
	}
	return capabilities, nil
}

func invalidStoredWorkerManifest() error {
	return problem.New(500, "worker_manifest_projection_invalid", "A stored Worker manifest is invalid.")
}

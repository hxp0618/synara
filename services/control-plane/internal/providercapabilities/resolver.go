package providercapabilities

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

const workerProtocolVersion = 2

func LoadTargetProjection(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	now time.Time,
	heartbeatTimeout time.Duration,
) (Projection, error) {
	policy, err := executiontargets.ParseProviderPolicy(target.Capabilities)
	if err != nil {
		return Projection{}, err
	}
	enabled := make(map[string]bool, len(policy.ExperimentalProviders))
	for _, provider := range policy.ExperimentalProviders {
		canonical, valid := providercatalog.CanonicalName(provider)
		if valid {
			enabled[canonical] = true
		}
	}
	input := TargetInput{
		ExecutionTargetID: target.ID, TargetKind: target.Kind, TargetStatus: target.Status,
		ExperimentalProviderEnabled: enabled,
		Observations:                map[string][]ManifestObservation{},
	}
	if target.Status != "active" {
		return ProjectTarget(input)
	}
	if heartbeatTimeout <= 0 {
		heartbeatTimeout = 90 * time.Second
	}
	workers := make([]persistence.WorkerInstance, 0)
	query := db.WithContext(ctx).
		Where("execution_target_id = ? AND administrative_status = ? AND status = ? AND terminated_at IS NULL", target.ID, "active", "online").
		Where("last_heartbeat_at >= ? AND current_manifest_id IS NOT NULL", now.Add(-heartbeatTimeout)).
		Where("protocol_version = ?", workerProtocolVersion)
	if target.Kind != "local" {
		query = query.Where("lease_supported = ? AND fencing_supported = ?", true, true)
	}
	if err := query.Order("id").Find(&workers).Error; err != nil {
		return Projection{}, err
	}
	if len(workers) == 0 {
		return ProjectTarget(input)
	}

	manifestIDs := make([]uuid.UUID, 0, len(workers))
	seenManifestIDs := make(map[uuid.UUID]struct{}, len(workers))
	for _, worker := range workers {
		if worker.CurrentManifestID == nil {
			continue
		}
		if _, found := seenManifestIDs[*worker.CurrentManifestID]; found {
			continue
		}
		seenManifestIDs[*worker.CurrentManifestID] = struct{}{}
		manifestIDs = append(manifestIDs, *worker.CurrentManifestID)
	}
	if len(manifestIDs) == 0 {
		return ProjectTarget(input)
	}
	models := make([]persistence.WorkerProviderManifest, 0, len(manifestIDs)*len(providercatalog.ProviderNames()))
	if err := db.WithContext(ctx).
		Where("worker_manifest_id IN ?", manifestIDs).
		Order("worker_manifest_id, provider").Find(&models).Error; err != nil {
		return Projection{}, err
	}
	byManifest := make(map[uuid.UUID]map[string]persistence.WorkerProviderManifest, len(manifestIDs))
	for _, model := range models {
		canonical, valid := providercatalog.CanonicalName(model.Provider)
		if !valid {
			return Projection{}, ErrInvalidManifest
		}
		providers := byManifest[model.WorkerManifestID]
		if providers == nil {
			providers = make(map[string]persistence.WorkerProviderManifest, len(providercatalog.ProviderNames()))
			byManifest[model.WorkerManifestID] = providers
		}
		if _, duplicate := providers[canonical]; duplicate {
			return Projection{}, ErrInvalidManifest
		}
		providers[canonical] = model
	}
	for _, worker := range workers {
		if worker.CurrentManifestID == nil {
			continue
		}
		providers := byManifest[*worker.CurrentManifestID]
		if len(providers) != len(providercatalog.ProviderNames()) {
			return Projection{}, ErrInvalidManifest
		}
		for _, providerName := range providercatalog.ProviderNames() {
			model, found := providers[providerName]
			if !found {
				return Projection{}, ErrInvalidManifest
			}
			input.Observations[providerName] = append(input.Observations[providerName], ManifestObservation{
				WorkerCompatible:    worker.CompatibilityStatus == "compatible",
				CompatibilityStatus: model.CompatibilityStatus,
				IncompatibilityCode: optionalString(model.IncompatibilityCode),
				Capabilities:        model.Capabilities,
			})
		}
	}
	return ProjectTarget(input)
}

func LoadExecutionProjection(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	execution persistence.AgentExecution,
	provider string,
) (Projection, error) {
	input := ExecutionInput{
		ExecutionTargetID: target.ID, TargetKind: execution.TargetKind, TargetStatus: target.Status,
		ExecutionID: execution.ID, Provider: provider,
	}
	if execution.WorkerManifestID == nil {
		return ProjectExecution(input)
	}
	canonical, valid := providercatalog.CanonicalName(provider)
	if !valid {
		return ProjectExecution(input)
	}
	var model persistence.WorkerProviderManifest
	err := db.WithContext(ctx).
		Where("worker_manifest_id = ? AND LOWER(provider) = ?", *execution.WorkerManifestID, strings.ToLower(canonical)).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Projection{}, ErrInvalidManifest
	}
	if err != nil {
		return Projection{}, err
	}
	input.Manifest = &ManifestObservation{
		WorkerCompatible:    true,
		CompatibilityStatus: model.CompatibilityStatus,
		IncompatibilityCode: optionalString(model.IncompatibilityCode),
		Capabilities:        model.Capabilities,
	}
	return ProjectExecution(input)
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

package sessions

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
)

func (s *Service) requireTargetProviderCapabilities(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	provider string,
	capabilityIDs ...string,
) error {
	projection, err := providercapabilities.LoadTargetProjection(
		ctx, tx, target, s.now(), s.providerCapabilityHeartbeatTimeout,
	)
	if err != nil {
		if errors.Is(err, providercapabilities.ErrInvalidManifest) {
			return problem.Wrap(500, "worker_manifest_projection_invalid", "A stored Worker manifest is invalid.", err)
		}
		return problem.Wrap(500, "provider_capabilities_load_failed", "Provider capabilities could not be loaded.", err)
	}
	decision := providercapabilities.Check(projection, provider, capabilityIDs...)
	if decision.Status != providercapabilities.StatusUnsupported {
		return nil
	}
	apiError := problem.New(
		409,
		decision.ReasonCode,
		"The selected Provider does not support a required capability on this Execution Target.",
	)
	apiError.Details = map[string]any{
		"executionTargetId": target.ID,
		"provider":          decision.Provider,
		"capabilityId":      decision.CapabilityID,
	}
	return apiError
}

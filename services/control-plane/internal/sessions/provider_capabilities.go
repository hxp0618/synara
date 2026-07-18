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
	return s.requireTargetProviderCapabilitiesWithObservation(
		ctx, tx, target, provider, false, capabilityIDs...,
	)
}

func (s *Service) requireObservedTargetProviderCapabilities(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	provider string,
	capabilityIDs ...string,
) error {
	return s.requireTargetProviderCapabilitiesWithObservation(
		ctx, tx, target, provider, true, capabilityIDs...,
	)
}

func (s *Service) RequireObservedTargetProviderCapabilities(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	provider string,
	capabilityIDs ...string,
) error {
	return s.requireObservedTargetProviderCapabilities(ctx, tx, target, provider, capabilityIDs...)
}

func (s *Service) requireTargetProviderCapabilitiesWithObservation(
	ctx context.Context,
	tx *gorm.DB,
	target persistence.ExecutionTarget,
	provider string,
	requireObserved bool,
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
	return EnforceProviderCapabilityDecision(target, decision, requireObserved)
}

func EnforceProviderCapabilityDecision(
	target persistence.ExecutionTarget,
	decision providercapabilities.Decision,
	requireObserved bool,
) error {
	if decision.Status == providercapabilities.StatusSupported ||
		(!requireObserved && decision.Status == providercapabilities.StatusUnobserved) {
		return nil
	}
	message := "The selected Provider does not support a required capability on this Execution Target."
	if decision.Status == providercapabilities.StatusUnobserved {
		message = "The selected Provider capability has not been observed on this Execution Target."
	}
	apiError := problem.New(
		409,
		decision.ReasonCode,
		message,
	)
	apiError.Details = map[string]any{
		"executionTargetId": target.ID,
		"provider":          decision.Provider,
		"capabilityId":      decision.CapabilityID,
		"status":            decision.Status,
	}
	return apiError
}

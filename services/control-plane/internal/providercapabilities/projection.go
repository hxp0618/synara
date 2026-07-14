package providercapabilities

import (
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

type Status string

const (
	StatusSupported   Status = "supported"
	StatusUnsupported Status = "unsupported"
	StatusUnobserved  Status = "unobserved"
)

type Basis string

const (
	BasisTarget    Basis = "target"
	BasisExecution Basis = "execution"
)

type SupportMode string

const (
	SupportModeNative   SupportMode = "native"
	SupportModeEmulated SupportMode = "emulated"
)

const (
	ReasonCapabilitySupported                  = "capability_supported"
	ReasonCapabilityUnsupported                = "capability_unsupported"
	ReasonProviderNotInstalled                 = "provider_not_installed"
	ReasonProviderVersionIncompatible          = "provider_version_incompatible"
	ReasonWorkerManifestRequired               = "worker_manifest_required"
	ReasonWorkerManifestReregistrationRequired = "worker_manifest_reregistration_required"
	ReasonExecutionTargetUnavailable           = "execution_target_unavailable"
)

var ErrInvalidManifest = errors.New("stored Provider manifest is invalid")

type Item struct {
	Provider     string       `json:"provider"`
	CapabilityID string       `json:"capabilityId"`
	Status       Status       `json:"status"`
	ReasonCode   string       `json:"reasonCode"`
	SupportMode  *SupportMode `json:"supportMode,omitempty"`
}

type Projection struct {
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	TargetKind        string     `json:"targetKind"`
	Basis             Basis      `json:"basis"`
	ExecutionID       *uuid.UUID `json:"executionId,omitempty"`
	Items             []Item     `json:"items"`
}

type ManifestObservation struct {
	WorkerCompatible    bool
	CompatibilityStatus string
	IncompatibilityCode string
	Capabilities        map[string]any
}

type TargetInput struct {
	ExecutionTargetID           uuid.UUID
	TargetKind                  string
	TargetStatus                string
	ExperimentalProviderEnabled map[string]bool
	Observations                map[string][]ManifestObservation
}

type ExecutionInput struct {
	ExecutionTargetID uuid.UUID
	TargetKind        string
	TargetStatus      string
	ExecutionID       uuid.UUID
	Provider          string
	Manifest          *ManifestObservation
}

type Decision struct {
	Provider     string
	CapabilityID string
	Status       Status
	ReasonCode   string
	SupportMode  *SupportMode
}

func ProjectTarget(input TargetInput) (Projection, error) {
	projection := Projection{
		ExecutionTargetID: input.ExecutionTargetID,
		TargetKind:        input.TargetKind,
		Basis:             BasisTarget,
		Items:             make([]Item, 0, len(providercatalog.ProviderNames())*len(providercatalog.CapabilityIDs())),
	}
	for _, providerName := range providercatalog.ProviderNames() {
		provider, found := providercatalog.Lookup(providerName)
		if !found {
			return Projection{}, ErrInvalidManifest
		}
		observations := input.Observations[providerName]
		for _, capabilityID := range providercatalog.CapabilityIDs() {
			item, err := projectTargetItem(input, provider, capabilityID, observations)
			if err != nil {
				return Projection{}, err
			}
			projection.Items = append(projection.Items, item)
		}
	}
	return projection, nil
}

func ProjectExecution(input ExecutionInput) (Projection, error) {
	providerName, catalogProvider := providercatalog.CanonicalName(input.Provider)
	if !catalogProvider {
		providerName = strings.TrimSpace(input.Provider)
	}
	executionID := input.ExecutionID
	projection := Projection{
		ExecutionTargetID: input.ExecutionTargetID,
		TargetKind:        input.TargetKind,
		Basis:             BasisExecution,
		ExecutionID:       &executionID,
		Items:             make([]Item, 0, len(providercatalog.CapabilityIDs())),
	}
	for _, capabilityID := range providercatalog.CapabilityIDs() {
		if !catalogProvider {
			projection.Items = append(projection.Items, unsupportedItem(providerName, capabilityID, ReasonCapabilityUnsupported))
			continue
		}
		provider, _ := providercatalog.Lookup(providerName)
		if provider.SupportTier == "local-only" || provider.Capabilities[capabilityID] == "unsupported" {
			projection.Items = append(projection.Items, unsupportedItem(providerName, capabilityID, ReasonCapabilityUnsupported))
			continue
		}
		if input.Manifest == nil {
			projection.Items = append(projection.Items, unobservedItem(providerName, capabilityID))
			continue
		}
		item, err := projectObservedItem(providerName, capabilityID, []ManifestObservation{*input.Manifest})
		if err != nil {
			return Projection{}, err
		}
		projection.Items = append(projection.Items, item)
	}
	return projection, nil
}

func Check(projection Projection, provider string, capabilityIDs ...string) Decision {
	canonical, found := providercatalog.CanonicalName(provider)
	if !found {
		canonical = strings.TrimSpace(provider)
		return Decision{Provider: canonical, CapabilityID: firstCapability(capabilityIDs), Status: StatusUnsupported, ReasonCode: ReasonCapabilityUnsupported}
	}
	byCapability := make(map[string]Item, len(projection.Items))
	for _, item := range projection.Items {
		itemProvider, valid := providercatalog.CanonicalName(item.Provider)
		if valid && itemProvider == canonical {
			byCapability[item.CapabilityID] = item
		}
	}
	var firstUnobserved *Item
	var supportedMode *SupportMode
	for _, capabilityID := range capabilityIDs {
		item, exists := byCapability[capabilityID]
		if !exists {
			return Decision{Provider: canonical, CapabilityID: capabilityID, Status: StatusUnsupported, ReasonCode: ReasonCapabilityUnsupported}
		}
		decision := Decision{
			Provider: canonical, CapabilityID: capabilityID, Status: item.Status,
			ReasonCode: item.ReasonCode, SupportMode: item.SupportMode,
		}
		if item.Status == StatusUnsupported {
			return decision
		}
		if item.Status == StatusUnobserved && firstUnobserved == nil {
			copy := item
			firstUnobserved = &copy
		}
		if item.Status == StatusSupported && item.SupportMode != nil {
			mode := *item.SupportMode
			if supportedMode == nil || mode == SupportModeEmulated {
				supportedMode = &mode
			}
		}
	}
	if firstUnobserved != nil {
		return Decision{
			Provider: canonical, CapabilityID: firstUnobserved.CapabilityID,
			Status: firstUnobserved.Status, ReasonCode: firstUnobserved.ReasonCode,
		}
	}
	return Decision{
		Provider: canonical, CapabilityID: firstCapability(capabilityIDs), Status: StatusSupported,
		ReasonCode: ReasonCapabilitySupported, SupportMode: supportedMode,
	}
}

func projectTargetItem(
	input TargetInput,
	provider providercatalog.Provider,
	capabilityID string,
	observations []ManifestObservation,
) (Item, error) {
	if input.TargetStatus != "active" {
		return unsupportedItem(provider.Name, capabilityID, ReasonExecutionTargetUnavailable), nil
	}
	if provider.SupportTier == "local-only" || provider.Capabilities[capabilityID] == "unsupported" {
		return unsupportedItem(provider.Name, capabilityID, ReasonCapabilityUnsupported), nil
	}
	if provider.SupportTier == "experimental" && !input.ExperimentalProviderEnabled[provider.Name] {
		return unsupportedItem(provider.Name, capabilityID, ReasonCapabilityUnsupported), nil
	}
	if len(observations) == 0 {
		return unobservedItem(provider.Name, capabilityID), nil
	}
	return projectObservedItem(provider.Name, capabilityID, observations)
}

func projectObservedItem(provider, capabilityID string, observations []ManifestObservation) (Item, error) {
	compatible := make([]ManifestObservation, 0, len(observations))
	reasons := make([]string, 0, len(observations))
	for _, observation := range observations {
		if observation.WorkerCompatible && observation.CompatibilityStatus == "compatible" {
			compatible = append(compatible, observation)
			continue
		}
		reasons = append(reasons, observationReason(observation))
	}
	if len(compatible) == 0 {
		return unsupportedItem(provider, capabilityID, preferredReason(reasons)), nil
	}
	mode := SupportModeNative
	for _, observation := range compatible {
		raw, found := observation.Capabilities[capabilityID]
		if !found {
			return Item{}, ErrInvalidManifest
		}
		support, ok := raw.(string)
		if !ok {
			return Item{}, ErrInvalidManifest
		}
		switch support {
		case "native":
		case "emulated":
			mode = SupportModeEmulated
		case "unsupported":
			return unsupportedItem(provider, capabilityID, ReasonCapabilityUnsupported), nil
		default:
			return Item{}, ErrInvalidManifest
		}
	}
	return supportedItem(provider, capabilityID, mode), nil
}

func observationReason(observation ManifestObservation) string {
	if code := strings.TrimSpace(observation.IncompatibilityCode); code != "" {
		switch code {
		case ReasonCapabilityUnsupported, ReasonProviderNotInstalled, ReasonProviderVersionIncompatible,
			ReasonWorkerManifestReregistrationRequired:
			return code
		}
	}
	if !observation.WorkerCompatible {
		return ReasonWorkerManifestReregistrationRequired
	}
	switch observation.CompatibilityStatus {
	case "local-only", "disabled":
		return ReasonCapabilityUnsupported
	case "unavailable":
		return ReasonProviderNotInstalled
	case "incompatible":
		return ReasonProviderVersionIncompatible
	default:
		return ReasonWorkerManifestReregistrationRequired
	}
}

func preferredReason(reasons []string) string {
	if len(reasons) == 0 {
		return ReasonWorkerManifestRequired
	}
	priority := map[string]int{
		ReasonCapabilityUnsupported:                0,
		ReasonProviderNotInstalled:                 1,
		ReasonProviderVersionIncompatible:          2,
		ReasonWorkerManifestReregistrationRequired: 3,
	}
	sort.SliceStable(reasons, func(i, j int) bool { return priority[reasons[i]] < priority[reasons[j]] })
	return reasons[0]
}

func supportedItem(provider, capabilityID string, mode SupportMode) Item {
	return Item{
		Provider: provider, CapabilityID: capabilityID, Status: StatusSupported,
		ReasonCode: ReasonCapabilitySupported, SupportMode: &mode,
	}
}

func unsupportedItem(provider, capabilityID, reason string) Item {
	return Item{Provider: provider, CapabilityID: capabilityID, Status: StatusUnsupported, ReasonCode: reason}
}

func unobservedItem(provider, capabilityID string) Item {
	return Item{
		Provider: provider, CapabilityID: capabilityID, Status: StatusUnobserved,
		ReasonCode: ReasonWorkerManifestRequired,
	}
}

func firstCapability(capabilityIDs []string) string {
	if len(capabilityIDs) == 0 {
		return ""
	}
	return capabilityIDs[0]
}

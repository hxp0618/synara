package executiontargets

import (
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

var stage3ProviderOrder = providercatalog.ProviderNames()

type ProviderRoutingPreference string

const (
	ProviderRoutingPreferenceNeutral ProviderRoutingPreference = ""
	ProviderRoutingPreferencePrefer  ProviderRoutingPreference = "prefer"
	ProviderRoutingPreferenceAvoid   ProviderRoutingPreference = "avoid"
)

type ProviderPolicy struct {
	ExperimentalProviders []string                             `json:"experimentalProviders"`
	RoutingPreferences    map[string]ProviderRoutingPreference `json:"routingPreferences,omitempty"`
}

func ParseProviderPolicy(capabilities map[string]any) (ProviderPolicy, error) {
	rawPolicy, found := capabilities["providerPolicy"]
	if !found {
		return ProviderPolicy{ExperimentalProviders: []string{}, RoutingPreferences: map[string]ProviderRoutingPreference{}}, nil
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok {
		return ProviderPolicy{}, invalidProviderPolicy("providerPolicy must be a JSON object.")
	}
	for field := range policy {
		if field != "experimentalProviders" && field != "routingPreferences" {
			return ProviderPolicy{}, invalidProviderPolicy("providerPolicy contains an unknown field.")
		}
	}
	rawProviders, found := policy["experimentalProviders"]
	normalizedProviders := []string{}
	if found {
		providers, ok := providerPolicyStrings(rawProviders)
		if !ok {
			return ProviderPolicy{}, invalidProviderPolicy("providerPolicy.experimentalProviders must be a JSON array of Provider names.")
		}
		selected := make(map[string]struct{}, len(providers))
		for _, provider := range providers {
			canonical, valid := CanonicalStage3Provider(provider)
			if !valid {
				return ProviderPolicy{}, invalidProviderPolicy("providerPolicy.experimentalProviders contains an unknown Provider.")
			}
			if _, duplicate := selected[canonical]; duplicate {
				return ProviderPolicy{}, invalidProviderPolicy("providerPolicy.experimentalProviders contains a duplicate Provider.")
			}
			selected[canonical] = struct{}{}
		}
		normalizedProviders = make([]string, 0, len(selected))
		for _, provider := range stage3ProviderOrder {
			if _, enabled := selected[provider]; enabled {
				normalizedProviders = append(normalizedProviders, provider)
			}
		}
	}

	rawRoutingPreferences, found := policy["routingPreferences"]
	if !found {
		return ProviderPolicy{
			ExperimentalProviders: normalizedProviders,
			RoutingPreferences:    map[string]ProviderRoutingPreference{},
		}, nil
	}

	routingPreferences, ok := providerPolicyRoutingPreferences(rawRoutingPreferences)
	if !ok {
		return ProviderPolicy{}, invalidProviderPolicy("providerPolicy.routingPreferences must be a JSON object of canonical Provider names to prefer or avoid.")
	}
	normalizedPreferences := make(map[string]ProviderRoutingPreference, len(routingPreferences))
	for provider, preference := range routingPreferences {
		canonical, valid := CanonicalStage3Provider(provider)
		if !valid || canonical != provider {
			return ProviderPolicy{}, invalidProviderPolicy("providerPolicy.routingPreferences keys must be canonical Stage 3 Provider names.")
		}
		switch preference {
		case ProviderRoutingPreferencePrefer, ProviderRoutingPreferenceAvoid:
			normalizedPreferences[canonical] = preference
		default:
			return ProviderPolicy{}, invalidProviderPolicy("providerPolicy.routingPreferences values must be prefer or avoid.")
		}
	}
	return ProviderPolicy{
		ExperimentalProviders: normalizedProviders,
		RoutingPreferences:    normalizedPreferences,
	}, nil
}

func (policy ProviderPolicy) ExperimentalProviderEnabled(provider string) bool {
	canonical, valid := CanonicalStage3Provider(provider)
	if !valid {
		return false
	}
	for _, enabled := range policy.ExperimentalProviders {
		if enabled == canonical {
			return true
		}
	}
	return false
}

func (policy ProviderPolicy) RoutingPreference(provider string) ProviderRoutingPreference {
	canonical, valid := CanonicalStage3Provider(provider)
	if !valid || len(policy.RoutingPreferences) == 0 {
		return ProviderRoutingPreferenceNeutral
	}
	preference, found := policy.RoutingPreferences[canonical]
	if !found {
		return ProviderRoutingPreferenceNeutral
	}
	return preference
}

func (policy ProviderPolicy) Equal(other ProviderPolicy) bool {
	if !policy.WorkerCompatibilityEqual(other) {
		return false
	}
	if len(policy.RoutingPreferences) != len(other.RoutingPreferences) {
		return false
	}
	for provider, preference := range policy.RoutingPreferences {
		if other.RoutingPreferences[provider] != preference {
			return false
		}
	}
	return true
}

// WorkerCompatibilityEqual compares only policy fields that can change which
// Providers an already-registered Worker is allowed to claim. Soft routing
// preferences reorder Targets but do not change a Worker's manifest contract.
func (policy ProviderPolicy) WorkerCompatibilityEqual(other ProviderPolicy) bool {
	if len(policy.ExperimentalProviders) != len(other.ExperimentalProviders) {
		return false
	}
	for index := range policy.ExperimentalProviders {
		if policy.ExperimentalProviders[index] != other.ExperimentalProviders[index] {
			return false
		}
	}
	return true
}

func ExperimentalProviderEnabled(capabilities map[string]any, provider string) (bool, error) {
	policy, err := ParseProviderPolicy(capabilities)
	if err != nil {
		return false, err
	}
	return policy.ExperimentalProviderEnabled(provider), nil
}

func normalizeProviderPolicyCapabilities(capabilities map[string]any) (map[string]any, error) {
	policy, err := ParseProviderPolicy(capabilities)
	if err != nil {
		return nil, err
	}
	normalized := make(map[string]any, len(capabilities))
	for key, value := range capabilities {
		normalized[key] = value
	}
	if _, found := capabilities["providerPolicy"]; found {
		normalizedPolicy := map[string]any{
			"experimentalProviders": append([]string(nil), policy.ExperimentalProviders...),
		}
		if len(policy.RoutingPreferences) != 0 {
			routingPreferences := make(map[string]any, len(policy.RoutingPreferences))
			for _, provider := range stage3ProviderOrder {
				preference, found := policy.RoutingPreferences[provider]
				if !found {
					continue
				}
				routingPreferences[provider] = string(preference)
			}
			normalizedPolicy["routingPreferences"] = routingPreferences
		}
		normalized["providerPolicy"] = normalizedPolicy
	}
	return normalized, nil
}

func providerPolicyStrings(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			provider, ok := value.(string)
			if !ok {
				return nil, false
			}
			result = append(result, provider)
		}
		return result, true
	default:
		return nil, false
	}
}

func providerPolicyRoutingPreferences(value any) (map[string]ProviderRoutingPreference, bool) {
	switch values := value.(type) {
	case map[string]string:
		result := make(map[string]ProviderRoutingPreference, len(values))
		for provider, preference := range values {
			result[provider] = ProviderRoutingPreference(preference)
		}
		return result, true
	case map[string]any:
		result := make(map[string]ProviderRoutingPreference, len(values))
		for provider, rawPreference := range values {
			preference, ok := rawPreference.(string)
			if !ok {
				return nil, false
			}
			result[provider] = ProviderRoutingPreference(preference)
		}
		return result, true
	default:
		return nil, false
	}
}

// CanonicalStage3Provider returns the Provider catalog name for a supported
// Stage 3 Provider. Matching is case-insensitive so persisted storage codes can
// be safely projected back to the canonical Provider Host/API name.
func CanonicalStage3Provider(value string) (string, bool) {
	return providercatalog.CanonicalName(value)
}

func invalidProviderPolicy(message string) error {
	return problem.New(400, "invalid_execution_target_provider_policy", message)
}

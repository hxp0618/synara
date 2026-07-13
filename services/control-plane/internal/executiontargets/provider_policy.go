package executiontargets

import (
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var stage3ProviderOrder = []string{
	"codex", "claudeAgent", "cursor", "gemini", "grok", "kilo", "opencode", "pi",
}

type ProviderPolicy struct {
	ExperimentalProviders []string `json:"experimentalProviders"`
}

func ParseProviderPolicy(capabilities map[string]any) (ProviderPolicy, error) {
	rawPolicy, found := capabilities["providerPolicy"]
	if !found {
		return ProviderPolicy{ExperimentalProviders: []string{}}, nil
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok {
		return ProviderPolicy{}, invalidProviderPolicy("providerPolicy must be a JSON object.")
	}
	for field := range policy {
		if field != "experimentalProviders" {
			return ProviderPolicy{}, invalidProviderPolicy("providerPolicy contains an unknown field.")
		}
	}
	rawProviders, found := policy["experimentalProviders"]
	if !found {
		return ProviderPolicy{ExperimentalProviders: []string{}}, nil
	}
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
	normalized := make([]string, 0, len(selected))
	for _, provider := range stage3ProviderOrder {
		if _, enabled := selected[provider]; enabled {
			normalized = append(normalized, provider)
		}
	}
	return ProviderPolicy{ExperimentalProviders: normalized}, nil
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
		normalized["providerPolicy"] = map[string]any{
			"experimentalProviders": append([]string(nil), policy.ExperimentalProviders...),
		}
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

// CanonicalStage3Provider returns the Provider catalog name for a supported
// Stage 3 Provider. Matching is case-insensitive so persisted storage codes can
// be safely projected back to the canonical Provider Host/API name.
func CanonicalStage3Provider(value string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, provider := range stage3ProviderOrder {
		if strings.ToLower(provider) == normalized {
			return provider, true
		}
	}
	return "", false
}

func invalidProviderPolicy(message string) error {
	return problem.New(400, "invalid_execution_target_provider_policy", message)
}

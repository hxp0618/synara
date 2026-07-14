package providercatalog

import "strings"

type CompatibleRange struct {
	MinimumInclusive string
	MaximumExclusive string
}

type RuntimePolicy struct {
	Kind            string
	Name            string
	VersionSource   string
	CompatibleRange CompatibleRange
}

type Provider struct {
	Name           string
	SupportTier    string
	AdapterVersion string
	RuntimePolicy  RuntimePolicy
	Capabilities   map[string]string
}

func ProviderNames() []string {
	return append([]string(nil), generatedProviderNames...)
}

func CapabilityIDs() []string {
	return append([]string(nil), generatedCapabilityIDs...)
}

func Providers() []Provider {
	result := make([]Provider, len(generatedProviders))
	for index := range generatedProviders {
		result[index] = cloneProvider(generatedProviders[index])
	}
	return result
}

func Lookup(name string) (Provider, bool) {
	provider, found := generatedProviderByName[name]
	if !found {
		return Provider{}, false
	}
	return cloneProvider(provider), true
}

func CanonicalName(value string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	canonical, found := generatedCanonicalProviderNames[normalized]
	return canonical, found
}

func CapabilitySupport(provider, capabilityID string) (string, bool) {
	entry, found := generatedProviderByName[provider]
	if !found {
		return "", false
	}
	support, found := entry.Capabilities[capabilityID]
	return support, found
}

func cloneProvider(provider Provider) Provider {
	provider.Capabilities = cloneCapabilities(provider.Capabilities)
	return provider
}

func cloneCapabilities(capabilities map[string]string) map[string]string {
	result := make(map[string]string, len(capabilities))
	for capabilityID, support := range capabilities {
		result[capabilityID] = support
	}
	return result
}

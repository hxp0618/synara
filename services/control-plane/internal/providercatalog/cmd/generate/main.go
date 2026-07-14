package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strconv"
	"strings"
)

type sourceCatalog struct {
	Version       int              `json:"version"`
	CapabilityIDs []string         `json:"capabilityIds"`
	Providers     []sourceProvider `json:"providers"`
}

type sourceProvider struct {
	Provider       string            `json:"provider"`
	SupportTier    string            `json:"supportTier"`
	AdapterVersion string            `json:"adapterVersion"`
	RuntimePolicy  sourceRuntime     `json:"runtimePolicy"`
	Capabilities   map[string]string `json:"capabilities"`
}

type sourceRuntime struct {
	Kind            string      `json:"kind"`
	Name            string      `json:"name"`
	VersionSource   string      `json:"versionSource"`
	CompatibleRange sourceRange `json:"compatibleRange"`
}

type sourceRange struct {
	MinimumInclusive string  `json:"minimumInclusive"`
	MaximumExclusive *string `json:"maximumExclusive"`
}

func main() {
	var sourcePath, outputPath string
	var check bool
	flag.StringVar(&sourcePath, "source", "", "path to providerCapabilityCatalog.json")
	flag.StringVar(&outputPath, "output", "catalog_gen.go", "generated Go output path")
	flag.BoolVar(&check, "check", false, "verify the output is current without writing it")
	flag.Parse()
	if sourcePath == "" {
		fatal(errors.New("-source is required"))
	}

	source, err := os.ReadFile(sourcePath)
	if err != nil {
		fatal(fmt.Errorf("read catalog: %w", err))
	}
	var catalog sourceCatalog
	if err := json.Unmarshal(source, &catalog); err != nil {
		fatal(fmt.Errorf("decode catalog: %w", err))
	}
	if err := validateCatalog(catalog); err != nil {
		fatal(err)
	}
	generated, err := generateCatalog(catalog, source)
	if err != nil {
		fatal(err)
	}
	if check {
		current, readErr := os.ReadFile(outputPath)
		if readErr != nil {
			fatal(fmt.Errorf("read generated catalog: %w", readErr))
		}
		if !bytes.Equal(current, generated) {
			fatal(errors.New("generated Provider catalog is stale; run go generate ./internal/providercatalog"))
		}
		return
	}
	if err := os.WriteFile(outputPath, generated, 0o644); err != nil {
		fatal(fmt.Errorf("write generated catalog: %w", err))
	}
}

func validateCatalog(catalog sourceCatalog) error {
	if catalog.Version != 1 {
		return fmt.Errorf("catalog version = %d, want 1", catalog.Version)
	}
	if len(catalog.CapabilityIDs) == 0 || len(catalog.Providers) == 0 {
		return errors.New("catalog must contain capabilities and Providers")
	}
	capabilities := make(map[string]struct{}, len(catalog.CapabilityIDs))
	for _, capabilityID := range catalog.CapabilityIDs {
		if capabilityID == "" || capabilityID != strings.TrimSpace(capabilityID) {
			return errors.New("catalog contains an empty or non-canonical Capability ID")
		}
		if _, duplicate := capabilities[capabilityID]; duplicate {
			return fmt.Errorf("catalog contains duplicate Capability ID %q", capabilityID)
		}
		capabilities[capabilityID] = struct{}{}
	}
	providers := make(map[string]struct{}, len(catalog.Providers))
	canonicalProviders := make(map[string]struct{}, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		if provider.Provider == "" || provider.Provider != strings.TrimSpace(provider.Provider) ||
			provider.SupportTier == "" || provider.AdapterVersion == "" ||
			provider.RuntimePolicy.Kind == "" || provider.RuntimePolicy.Name == "" ||
			provider.RuntimePolicy.VersionSource == "" || provider.RuntimePolicy.CompatibleRange.MinimumInclusive == "" {
			return fmt.Errorf("Provider %q contains an incomplete descriptor", provider.Provider)
		}
		if !oneOf(provider.SupportTier, "tier-1", "tier-2", "experimental", "local-only") {
			return fmt.Errorf("Provider %q contains invalid support tier %q", provider.Provider, provider.SupportTier)
		}
		if !oneOf(provider.RuntimePolicy.Kind, "cli", "sdk", "local") ||
			!oneOf(provider.RuntimePolicy.VersionSource, "probe", "package", "build") {
			return fmt.Errorf("Provider %q contains an invalid Runtime policy", provider.Provider)
		}
		if _, duplicate := providers[provider.Provider]; duplicate {
			return fmt.Errorf("catalog contains duplicate Provider %q", provider.Provider)
		}
		providers[provider.Provider] = struct{}{}
		canonical := strings.ToLower(provider.Provider)
		if _, duplicate := canonicalProviders[canonical]; duplicate {
			return fmt.Errorf("catalog contains case-insensitive duplicate Provider %q", provider.Provider)
		}
		canonicalProviders[canonical] = struct{}{}
		if len(provider.Capabilities) != len(catalog.CapabilityIDs) {
			return fmt.Errorf("Provider %q capability count = %d, want %d", provider.Provider, len(provider.Capabilities), len(catalog.CapabilityIDs))
		}
		for _, capabilityID := range catalog.CapabilityIDs {
			support, found := provider.Capabilities[capabilityID]
			if !found || (support != "native" && support != "emulated" && support != "unsupported") {
				return fmt.Errorf("Provider %q capability %q has invalid support %q", provider.Provider, capabilityID, support)
			}
		}
		for capabilityID := range provider.Capabilities {
			if _, found := capabilities[capabilityID]; !found {
				return fmt.Errorf("Provider %q contains unknown capability %q", provider.Provider, capabilityID)
			}
		}
	}
	return nil
}

func generateCatalog(catalog sourceCatalog, source []byte) ([]byte, error) {
	var output bytes.Buffer
	digest := sha256.Sum256(source)
	fmt.Fprintln(&output, "// Code generated by go generate; DO NOT EDIT.")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "package providercatalog")
	fmt.Fprintln(&output)
	fmt.Fprintf(&output, "const SchemaVersion = %d\n", catalog.Version)
	fmt.Fprintf(&output, "const SourceSHA256 = %s\n\n", strconv.Quote(hex.EncodeToString(digest[:])))
	writeStringSlice(&output, "generatedCapabilityIDs", catalog.CapabilityIDs)
	providerNames := make([]string, 0, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		providerNames = append(providerNames, provider.Provider)
	}
	writeStringSlice(&output, "generatedProviderNames", providerNames)

	fmt.Fprintln(&output, "var generatedProviders = []Provider{")
	for _, provider := range catalog.Providers {
		fmt.Fprintln(&output, "\t{")
		fmt.Fprintf(&output, "\t\tName: %s,\n", strconv.Quote(provider.Provider))
		fmt.Fprintf(&output, "\t\tSupportTier: %s,\n", strconv.Quote(provider.SupportTier))
		fmt.Fprintf(&output, "\t\tAdapterVersion: %s,\n", strconv.Quote(provider.AdapterVersion))
		fmt.Fprintln(&output, "\t\tRuntimePolicy: RuntimePolicy{")
		fmt.Fprintf(&output, "\t\t\tKind: %s,\n", strconv.Quote(provider.RuntimePolicy.Kind))
		fmt.Fprintf(&output, "\t\t\tName: %s,\n", strconv.Quote(provider.RuntimePolicy.Name))
		fmt.Fprintf(&output, "\t\t\tVersionSource: %s,\n", strconv.Quote(provider.RuntimePolicy.VersionSource))
		fmt.Fprintln(&output, "\t\t\tCompatibleRange: CompatibleRange{")
		fmt.Fprintf(&output, "\t\t\t\tMinimumInclusive: %s,\n", strconv.Quote(provider.RuntimePolicy.CompatibleRange.MinimumInclusive))
		if provider.RuntimePolicy.CompatibleRange.MaximumExclusive != nil {
			fmt.Fprintf(&output, "\t\t\t\tMaximumExclusive: %s,\n", strconv.Quote(*provider.RuntimePolicy.CompatibleRange.MaximumExclusive))
		}
		fmt.Fprintln(&output, "\t\t\t},")
		fmt.Fprintln(&output, "\t\t},")
		fmt.Fprintln(&output, "\t\tCapabilities: map[string]string{")
		for _, capabilityID := range catalog.CapabilityIDs {
			fmt.Fprintf(&output, "\t\t\t%s: %s,\n", strconv.Quote(capabilityID), strconv.Quote(provider.Capabilities[capabilityID]))
		}
		fmt.Fprintln(&output, "\t\t},")
		fmt.Fprintln(&output, "\t},")
	}
	fmt.Fprintln(&output, "}")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "var generatedProviderByName = func() map[string]Provider {")
	fmt.Fprintln(&output, "\tresult := make(map[string]Provider, len(generatedProviders))")
	fmt.Fprintln(&output, "\tfor _, provider := range generatedProviders {")
	fmt.Fprintln(&output, "\t\tresult[provider.Name] = provider")
	fmt.Fprintln(&output, "\t}")
	fmt.Fprintln(&output, "\treturn result")
	fmt.Fprintln(&output, "}()")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "var generatedCanonicalProviderNames = map[string]string{")
	canonicalNames := append([]string(nil), providerNames...)
	sort.Slice(canonicalNames, func(left, right int) bool {
		return canonicalNames[left] < canonicalNames[right]
	})
	for _, provider := range canonicalNames {
		fmt.Fprintf(&output, "\t%s: %s,\n", strconv.Quote(strings.ToLower(provider)), strconv.Quote(provider))
	}
	fmt.Fprintln(&output, "}")

	formatted, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated catalog: %w\n%s", err, output.String())
	}
	return formatted, nil
}

func writeStringSlice(output *bytes.Buffer, name string, values []string) {
	fmt.Fprintf(output, "var %s = []string{\n", name)
	for _, value := range values {
		fmt.Fprintf(output, "\t%s,\n", strconv.Quote(value))
	}
	fmt.Fprintln(output, "}")
	fmt.Fprintln(output)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

package providercatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

type catalogFixture struct {
	Version       int                      `json:"version"`
	CapabilityIDs []string                 `json:"capabilityIds"`
	Providers     []catalogProviderFixture `json:"providers"`
}

type catalogProviderFixture struct {
	Provider       string `json:"provider"`
	SupportTier    string `json:"supportTier"`
	AdapterVersion string `json:"adapterVersion"`
	RuntimePolicy  struct {
		Kind            string `json:"kind"`
		Name            string `json:"name"`
		VersionSource   string `json:"versionSource"`
		CompatibleRange struct {
			MinimumInclusive string  `json:"minimumInclusive"`
			MaximumExclusive *string `json:"maximumExclusive"`
		} `json:"compatibleRange"`
	} `json:"runtimePolicy"`
	Capabilities map[string]string `json:"capabilities"`
}

func TestGeneratedCatalogMatchesSharedSource(t *testing.T) {
	encoded := sharedCatalogSource(t)
	digest := sha256.Sum256(encoded)
	if actual := hex.EncodeToString(digest[:]); actual != SourceSHA256 {
		t.Fatalf("generated catalog source hash = %q, want %q; run go generate ./internal/providercatalog", SourceSHA256, actual)
	}
	var fixture catalogFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != SchemaVersion {
		t.Fatalf("generated catalog version = %d, want %d", SchemaVersion, fixture.Version)
	}
	if !reflect.DeepEqual(CapabilityIDs(), fixture.CapabilityIDs) {
		t.Fatalf("generated Capability IDs drifted: %#v", CapabilityIDs())
	}
	wantNames := make([]string, 0, len(fixture.Providers))
	for _, expected := range fixture.Providers {
		wantNames = append(wantNames, expected.Provider)
		actual, found := Lookup(expected.Provider)
		if !found {
			t.Fatalf("generated catalog omitted Provider %q", expected.Provider)
		}
		if actual.SupportTier != expected.SupportTier || actual.AdapterVersion != expected.AdapterVersion ||
			actual.RuntimePolicy.Kind != expected.RuntimePolicy.Kind || actual.RuntimePolicy.Name != expected.RuntimePolicy.Name ||
			actual.RuntimePolicy.VersionSource != expected.RuntimePolicy.VersionSource ||
			actual.RuntimePolicy.CompatibleRange.MinimumInclusive != expected.RuntimePolicy.CompatibleRange.MinimumInclusive ||
			actual.RuntimePolicy.CompatibleRange.MaximumExclusive != stringValue(expected.RuntimePolicy.CompatibleRange.MaximumExclusive) ||
			!reflect.DeepEqual(actual.Capabilities, expected.Capabilities) {
			t.Fatalf("generated Provider %q drifted: %#v", expected.Provider, actual)
		}
	}
	if !reflect.DeepEqual(ProviderNames(), wantNames) {
		t.Fatalf("generated Provider names drifted: %#v", ProviderNames())
	}
}

func TestCatalogAPIReturnsCopiesAndCanonicalNames(t *testing.T) {
	names := ProviderNames()
	names[0] = "mutated"
	if ProviderNames()[0] == "mutated" {
		t.Fatal("ProviderNames exposed mutable generated state")
	}
	provider, found := Lookup("codex")
	if !found {
		t.Fatal("Codex is missing from the generated catalog")
	}
	provider.Capabilities[CapabilityIDs()[0]] = "mutated"
	if support, _ := CapabilitySupport("codex", CapabilityIDs()[0]); support == "mutated" {
		t.Fatal("Lookup exposed mutable generated capability state")
	}
	if canonical, valid := CanonicalName(" CLAUDEAGENT "); !valid || canonical != "claudeAgent" {
		t.Fatalf("CanonicalName returned (%q, %t)", canonical, valid)
	}
	if canonical, valid := CanonicalName("gemini"); !valid || canonical != "antigravity" {
		t.Fatalf("legacy Gemini alias returned (%q, %t)", canonical, valid)
	}
	if _, valid := CanonicalName("droid"); valid {
		t.Fatal("Droid must remain outside the Provider Host catalog")
	}
}

func sharedCatalogSource(t *testing.T) []byte {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve catalog test source")
	}
	path := filepath.Clean(filepath.Join(
		filepath.Dir(sourceFile), "../../../../packages/contracts/src/providerCapabilityCatalog.json",
	))
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

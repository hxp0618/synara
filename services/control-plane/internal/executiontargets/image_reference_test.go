package executiontargets

import (
	"strings"
	"testing"
)

func TestImmutableImageDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for reference, expected := range map[string]string{
		"registry.example.com/synara-worker@" + digest:                         digest,
		"registry.example.com/synara-worker:0.5.3@" + digest:                   digest,
		"registry.example.com/synara-worker:latest":                            "",
		"registry.example.com/synara-worker@sha256:short":                      "",
		"registry.example.com/synara-worker@sha256:" + strings.Repeat("A", 64): "",
	} {
		if actual := immutableImageDigest(reference); actual != expected {
			t.Fatalf("immutableImageDigest(%q) = %q, want %q", reference, actual, expected)
		}
	}
}

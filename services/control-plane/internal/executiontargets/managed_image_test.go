package executiontargets

import (
	"strings"
	"testing"
)

func TestPinImageReferencePreservesRepositoryAndReplacesMutableTag(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for input, want := range map[string]string{
		"ghcr.io/synara/worker:latest":                                  "ghcr.io/synara/worker@" + digest,
		"ghcr.io/synara/worker:1.2.3@sha256:" + strings.Repeat("b", 64): "ghcr.io/synara/worker@" + digest,
		"synara/worker:latest":                                          "synara/worker@" + digest,
	} {
		got, err := pinImageReference(input, digest)
		if err != nil || got != want {
			t.Fatalf("pinImageReference(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestRegistryAuthorityFromImageReference(t *testing.T) {
	for input, want := range map[string]string{
		"ghcr.io/synara/worker:latest":                                              "ghcr.io",
		"registry.example.com:5443/synara/worker@sha256:" + strings.Repeat("a", 64): "registry.example.com:5443",
		"localhost:5000/synara/worker:latest":                                       "localhost:5000",
		"synara/worker:latest":                                                      "docker.io",
		"synara-stage3-provider-acceptance:local":                                   "docker.io",
		"synara-stage3-provider-acceptance@sha256:" + strings.Repeat("b", 64):       "docker.io",
		"ubuntu": "docker.io",
	} {
		got, err := registryAuthorityFromImageReference(input)
		if err != nil || got != want {
			t.Fatalf("registryAuthorityFromImageReference(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestValidateImagePullCredentialRequiresExactRegistry(t *testing.T) {
	credential := &ImagePullCredential{Host: "ghcr.io", Username: "synara", Password: "secret"}
	if err := validateImagePullCredential("ghcr.io/synara/worker:latest", credential); err != nil {
		t.Fatal(err)
	}
	if err := validateImagePullCredential("docker.io/synara/worker:latest", credential); err == nil {
		t.Fatal("mismatched image-pull Credential was accepted")
	}
}

func TestValidateImagePullCredentialPreservesRegistryPortBoundary(t *testing.T) {
	credential := &ImagePullCredential{Host: "registry.example.com", Username: "synara", Password: "secret"}
	if err := validateImagePullCredential("registry.example.com:5443/synara/worker:latest", credential); err == nil {
		t.Fatal("image-pull Credential crossed a registry port boundary")
	}
	credential.Host = "registry.example.com:5443"
	if err := validateImagePullCredential("registry.example.com:5443/synara/worker:latest", credential); err != nil {
		t.Fatal(err)
	}
}

func TestValidateImagePullCredentialTreatsDockerHubAliasesAsOneRegistry(t *testing.T) {
	credential := &ImagePullCredential{Host: "index.docker.io", Username: "synara", Password: "secret"}
	if err := validateImagePullCredential("synara/worker:latest", credential); err != nil {
		t.Fatal(err)
	}
}

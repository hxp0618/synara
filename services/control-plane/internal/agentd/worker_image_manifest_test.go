package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type workerImageManifestFixture struct {
	Path     string
	Manifest workerImageManifest
	Files    map[string]string
}

func TestLoadWorkerImageManifestValidatesReferencesAndProducesRedactedFeatureFlag(t *testing.T) {
	fixture := newWorkerImageManifestFixture(t)
	t.Setenv(workerImageManifestEnvironment, fixture.Path)

	manifest, err := loadConfiguredWorkerImageManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest == nil || manifest.Source.Version != "1.2.3" ||
		manifest.Source.GitSHA != strings.Repeat("d", 40) ||
		manifest.Platform.OS != runtime.GOOS || manifest.Platform.Architecture != runtime.GOARCH {
		t.Fatalf("unexpected Worker image manifest: %#v", manifest)
	}
	if manifest.BaseImages[0].Name != "agentd-build" ||
		manifest.ProviderRuntimes[0].Provider != "claudeAgent" || manifest.ProviderRuntimes[0].Kind != "cli" {
		t.Fatalf("Worker image manifest was not normalized deterministically: %#v", manifest)
	}
	encoded, err := json.Marshal(manifest.featureFlagValue())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"path"`) || strings.Contains(string(encoded), filepath.Dir(fixture.Path)) {
		t.Fatalf("Worker image build Feature Flag leaked a filesystem path: %s", encoded)
	}
	for _, required := range []string{
		`"schemaVersion":1`, `"source"`, `"platform"`, `"baseImages"`,
		`"lockfiles"`, `"providerRuntimes"`, `"sboms"`,
	} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("Worker image build Feature Flag omitted %s: %s", required, encoded)
		}
	}
}

func TestLoadWorkerImageManifestRejectsInvalidOrChangedInputs(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		fixture := newWorkerImageManifestFixture(t)
		encoded, err := json.Marshal(map[string]any{
			"schemaVersion": fixture.Manifest.SchemaVersion,
			"source":        fixture.Manifest.Source, "platform": fixture.Manifest.Platform,
			"baseImages": fixture.Manifest.BaseImages, "lockfiles": fixture.Manifest.Lockfiles,
			"providerRuntimes": fixture.Manifest.ProviderRuntimes, "sboms": fixture.Manifest.SBOMs,
			"unexpected": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.Path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(workerImageManifestEnvironment, fixture.Path)
		if _, err := loadConfiguredWorkerImageManifest(); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("Worker image manifest with an unknown field was accepted: %v", err)
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		fixture := newWorkerImageManifestFixture(t)
		file, err := os.OpenFile(fixture.Path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("\n{}\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		t.Setenv(workerImageManifestEnvironment, fixture.Path)
		if _, err := loadConfiguredWorkerImageManifest(); err == nil || !strings.Contains(err.Error(), "trailing") {
			t.Fatalf("Worker image manifest with trailing data was accepted: %v", err)
		}
	})

	t.Run("reference hash mismatch", func(t *testing.T) {
		fixture := newWorkerImageManifestFixture(t)
		if err := os.WriteFile(fixture.Files["provider-tools-npm"], []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(workerImageManifestEnvironment, fixture.Path)
		if _, err := loadConfiguredWorkerImageManifest(); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("Worker image manifest with a changed Lockfile was accepted: %v", err)
		}
	})

	t.Run("symlink reference", func(t *testing.T) {
		fixture := newWorkerImageManifestFixture(t)
		linkPath := filepath.Join(filepath.Dir(fixture.Path), "artifacts", "provider-tools-link.lock")
		if err := os.Symlink(fixture.Files["provider-tools-npm"], linkPath); err != nil {
			t.Fatal(err)
		}
		fixture.Manifest.Lockfiles[0].Path = filepath.ToSlash(filepath.Join("artifacts", filepath.Base(linkPath)))
		writeWorkerImageManifestFixture(t, fixture.Path, fixture.Manifest)
		t.Setenv(workerImageManifestEnvironment, fixture.Path)
		if _, err := loadConfiguredWorkerImageManifest(); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("Worker image manifest followed a Lockfile symlink: %v", err)
		}
	})

	t.Run("missing required runtime", func(t *testing.T) {
		fixture := newWorkerImageManifestFixture(t)
		fixture.Manifest.ProviderRuntimes = fixture.Manifest.ProviderRuntimes[:2]
		writeWorkerImageManifestFixture(t, fixture.Path, fixture.Manifest)
		t.Setenv(workerImageManifestEnvironment, fixture.Path)
		if _, err := loadConfiguredWorkerImageManifest(); err == nil || !strings.Contains(err.Error(), "three required Provider runtimes") {
			t.Fatalf("Worker image manifest omitted a required runtime: %v", err)
		}
	})

	t.Run("unpinned Base Image", func(t *testing.T) {
		fixture := newWorkerImageManifestFixture(t)
		fixture.Manifest.BaseImages[0].Reference = "golang:1.26-bookworm"
		writeWorkerImageManifestFixture(t, fixture.Path, fixture.Manifest)
		t.Setenv(workerImageManifestEnvironment, fixture.Path)
		if _, err := loadConfiguredWorkerImageManifest(); err == nil || !strings.Contains(err.Error(), "not digest-pinned") {
			t.Fatalf("Worker image manifest accepted an unpinned Base Image: %v", err)
		}
	})
}

func TestWithProviderHostCapabilitiesPublishesAuthoritativeWorkerImageBuild(t *testing.T) {
	fixture := newWorkerImageManifestFixture(t)
	t.Setenv(workerImageManifestEnvironment, fixture.Path)
	manifest, err := loadConfiguredWorkerImageManifest()
	if err != nil {
		t.Fatal(err)
	}
	baseFeatureFlags := map[string]any{
		"existing":                  true,
		workerImageBuildFeatureFlag: map[string]any{"forged": true},
	}
	base := map[string]any{"featureFlags": baseFeatureFlags}
	result := withProviderHostCapabilities(base, map[string]any{"protocolVersion": 2}, Config{
		Version: "1.2.3", BuildGitSHA: strings.Repeat("d", 40), RunnerProtocol: RunnerProtocolV2,
		WorkerImageManifest: manifest,
	})
	featureFlags, ok := result["featureFlags"].(map[string]any)
	if !ok || featureFlags["existing"] != true {
		t.Fatalf("existing Worker Feature Flags were not preserved: %#v", result)
	}
	imageBuild, ok := featureFlags[workerImageBuildFeatureFlag].(map[string]any)
	if !ok || imageBuild["schemaVersion"] != 1 {
		t.Fatalf("authoritative Worker image build was not published: %#v", featureFlags)
	}
	if forged, found := imageBuild["forged"]; found || forged != nil {
		t.Fatalf("forged Worker image build survived authoritative replacement: %#v", imageBuild)
	}
	if _, mutated := baseFeatureFlags[workerImageBuildFeatureFlag].(map[string]any)["schemaVersion"]; mutated {
		t.Fatalf("base Worker Feature Flags were mutated: %#v", baseFeatureFlags)
	}
}

func newWorkerImageManifestFixture(t *testing.T) workerImageManifestFixture {
	t.Helper()
	root := t.TempDir()
	artifacts := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	lockfiles := make([]workerImageLockfile, 0, 3)
	for _, item := range []struct {
		name    string
		content string
	}{
		{name: "provider-tools-npm", content: `{"lockfileVersion":3}`},
		{name: "provider-host-bun", content: "bun-lock-v1"},
		{name: "worker-apk", content: "apk-lock-v1"},
	} {
		path := filepath.Join(artifacts, item.name+".lock")
		writeWorkerImageReference(t, path, item.content)
		files[item.name] = path
		lockfiles = append(lockfiles, workerImageLockfile{
			Name: item.name, Path: filepath.ToSlash(filepath.Join("artifacts", filepath.Base(path))),
			SHA256: workerImageFixtureSHA256(item.content),
		})
	}
	sbomContent := `{"spdxVersion":"SPDX-2.3"}`
	sbomPath := filepath.Join(artifacts, "provider-tools.spdx.json")
	writeWorkerImageReference(t, sbomPath, sbomContent)
	files["provider-tools-sbom"] = sbomPath
	manifest := workerImageManifest{
		SchemaVersion: 1,
		Source:        workerImageSource{Version: "1.2.3", GitSHA: strings.Repeat("d", 40)},
		Platform:      workerImagePlatform{OS: runtime.GOOS, Architecture: runtime.GOARCH},
		BaseImages: []workerImageBaseImage{
			{Name: "worker-runtime", Reference: "node:24-alpine@sha256:" + strings.Repeat("c", 64)},
			{Name: "agentd-build", Reference: "golang:1.26-bookworm@sha256:" + strings.Repeat("a", 64)},
			{Name: "provider-host-build", Reference: "oven/bun:1.3.14@sha256:" + strings.Repeat("b", 64)},
		},
		Lockfiles: lockfiles,
		ProviderRuntimes: []workerImageProviderRuntime{
			{Provider: "codex", Kind: "cli", Package: "@openai/codex", Version: "0.144.1"},
			{Provider: "claudeAgent", Kind: "sdk", Package: "@anthropic-ai/claude-agent-sdk", Version: "0.3.207"},
			{Provider: "claudeAgent", Kind: "cli", Package: "@anthropic-ai/claude-code", Version: "2.1.197"},
		},
		SBOMs: []workerImageSoftwareBill{{
			Name: "provider-tools", Format: "spdx-json",
			Path:   filepath.ToSlash(filepath.Join("artifacts", filepath.Base(sbomPath))),
			SHA256: workerImageFixtureSHA256(sbomContent),
		}},
	}
	path := filepath.Join(root, "worker-image-manifest.json")
	writeWorkerImageManifestFixture(t, path, manifest)
	return workerImageManifestFixture{Path: path, Manifest: manifest, Files: files}
}

func writeWorkerImageManifestFixture(t *testing.T, path string, manifest workerImageManifest) {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWorkerImageReference(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func workerImageFixtureSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

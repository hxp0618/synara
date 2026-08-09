package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	workerImageManifestEnvironment  = "SYNARA_AGENTD_WORKER_IMAGE_MANIFEST_PATH"
	workerImageManifestMaximumSize  = 128 << 10
	workerImageReferenceMaximumSize = 64 << 20
	workerImageBuildFeatureFlag     = "workerImageBuild"
	cloudAgentReleaseRepository     = "hxp0618/cloud-agents"
	cloudAgentReleaseTag            = "cloud-agent-m1-rc.1"
	cloudAgentReleaseBaseURL        = "https://github.com/hxp0618/cloud-agents/releases/download/cloud-agent-m1-rc.1/"
	cloudAgentSourceCommit          = "49e8cdc6a3a4f88c7324d055ce519e9f25a8ca8a"
	cloudAgentStandaloneURL         = cloudAgentReleaseBaseURL + "cloud-agent-runtime-standalone.mjs"
	cloudAgentStandaloneSHA256      = "sha256:c24a1c23f89f62e714e3465b59a4be12e1b5cb75387a2d0d6de9811803064521"
)

var (
	workerImageSourceVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+\-]*$`)
	pinnedPackageVersionPattern     = regexp.MustCompile(
		`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
	)
)

type workerImageManifest struct {
	SchemaVersion       int                            `json:"schemaVersion"`
	Source              workerImageSource              `json:"source"`
	Platform            workerImagePlatform            `json:"platform"`
	BaseImages          []workerImageBaseImage         `json:"baseImages"`
	Lockfiles           []workerImageLockfile          `json:"lockfiles"`
	ProviderRuntimes    []workerImageProviderRuntime   `json:"providerRuntimes"`
	CloudAgentCandidate workerImageCloudAgentCandidate `json:"cloudAgentCandidate"`
	SBOMs               []workerImageSoftwareBill      `json:"sboms"`
}

type workerImageSource struct {
	Version string `json:"version"`
	GitSHA  string `json:"gitSha"`
}

type workerImagePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type workerImageBaseImage struct {
	Name      string `json:"name"`
	Reference string `json:"reference"`
}

type workerImageLockfile struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type workerImageProviderRuntime struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Package  string `json:"package"`
	Version  string `json:"version"`
}

type workerImageCloudAgentCandidate struct {
	SourceCommit            string                         `json:"sourceCommit"`
	CandidateDigest         string                         `json:"candidateDigest"`
	StandaloneRuntimeSHA256 string                         `json:"standaloneRuntimeSha256"`
	Packages                []workerImageCloudAgentPackage `json:"packages"`
}

type workerImageCloudAgentPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type workerImageCloudAgentCandidateLock struct {
	SchemaVersion     int                                   `json:"schemaVersion"`
	Release           workerImageCloudAgentRelease          `json:"release"`
	StandaloneRuntime workerImageCloudAgentStandalone       `json:"standaloneRuntime"`
	Packages          map[string]workerImageCloudAgentAsset `json:"packages"`
}

type workerImageCloudAgentRelease struct {
	Repository      string `json:"repository"`
	Tag             string `json:"tag"`
	SourceCommit    string `json:"sourceCommit"`
	CandidateDigest string `json:"candidateDigest"`
}

type workerImageCloudAgentStandalone struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type workerImageCloudAgentAsset struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

type workerImageCloudAgentExpectedAsset struct {
	Version string
	URL     string
	SHA256  string
}

type workerImageSoftwareBill struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func loadConfiguredWorkerImageManifest() (*workerImageManifest, error) {
	path := strings.TrimSpace(os.Getenv(workerImageManifestEnvironment))
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
		return nil, fmt.Errorf("%s must be a canonical absolute path", workerImageManifestEnvironment)
	}
	encoded, err := readSmallRegularFile(path, workerImageManifestMaximumSize)
	if err != nil {
		return nil, errors.New("Worker image manifest is unavailable")
	}
	manifest, err := decodeWorkerImageManifest([]byte(encoded))
	if err != nil {
		return nil, err
	}
	if err := validateWorkerImageManifest(&manifest, path); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func decodeWorkerImageManifest(encoded []byte) (workerImageManifest, error) {
	var manifest workerImageManifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return workerImageManifest{}, errors.New("Worker image manifest is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workerImageManifest{}, errors.New("Worker image manifest contains trailing data")
	}
	return manifest, nil
}

func validateWorkerImageManifest(manifest *workerImageManifest, manifestPath string) error {
	if manifest.SchemaVersion != 1 {
		return errors.New("Worker image manifest schemaVersion must be 1")
	}
	manifest.Source.Version = strings.TrimSpace(manifest.Source.Version)
	manifest.Source.GitSHA = strings.TrimSpace(manifest.Source.GitSHA)
	if len(manifest.Source.Version) > 128 || !workerImageSourceVersionPattern.MatchString(manifest.Source.Version) {
		return errors.New("Worker image manifest source version is invalid")
	}
	if !validFullBuildGitSHA(manifest.Source.GitSHA) {
		return errors.New("Worker image manifest source Git SHA is invalid")
	}
	manifest.Platform.OS = strings.TrimSpace(manifest.Platform.OS)
	manifest.Platform.Architecture = strings.TrimSpace(manifest.Platform.Architecture)
	if manifest.Platform.OS != runtime.GOOS || manifest.Platform.Architecture != runtime.GOARCH {
		return fmt.Errorf(
			"Worker image manifest platform %s/%s does not match agentd %s/%s",
			manifest.Platform.OS,
			manifest.Platform.Architecture,
			runtime.GOOS,
			runtime.GOARCH,
		)
	}
	if err := validateWorkerImageBaseImages(manifest.BaseImages); err != nil {
		return err
	}
	if err := validateWorkerImageProviderRuntimes(manifest.ProviderRuntimes); err != nil {
		return err
	}
	if err := validateWorkerImageCloudAgentCandidate(&manifest.CloudAgentCandidate); err != nil {
		return err
	}
	if err := validateWorkerImageLockfiles(manifest.Lockfiles, manifestPath); err != nil {
		return err
	}
	if err := validateWorkerImageCloudAgentLock(
		&manifest.CloudAgentCandidate,
		manifest.Lockfiles,
		manifestPath,
	); err != nil {
		return err
	}
	if err := validateWorkerImageSBOMs(manifest.SBOMs, manifestPath); err != nil {
		return err
	}
	sort.Slice(manifest.BaseImages, func(left, right int) bool {
		return manifest.BaseImages[left].Name < manifest.BaseImages[right].Name
	})
	sort.Slice(manifest.Lockfiles, func(left, right int) bool {
		return manifest.Lockfiles[left].Name < manifest.Lockfiles[right].Name
	})
	sort.Slice(manifest.ProviderRuntimes, func(left, right int) bool {
		leftKey := manifest.ProviderRuntimes[left].Provider + "\x00" + manifest.ProviderRuntimes[left].Kind
		rightKey := manifest.ProviderRuntimes[right].Provider + "\x00" + manifest.ProviderRuntimes[right].Kind
		return leftKey < rightKey
	})
	sort.Slice(manifest.SBOMs, func(left, right int) bool {
		return manifest.SBOMs[left].Name < manifest.SBOMs[right].Name
	})
	return nil
}

func validateWorkerImageCloudAgentCandidate(candidate *workerImageCloudAgentCandidate) error {
	required := map[string]struct{}{
		"@synara/cloud-agent-protocol":        {},
		"@synara/cloud-agent-provider-api":    {},
		"@synara/cloud-agent-runtime":         {},
		"@synara/cloud-agent-provider-codex":  {},
		"@synara/cloud-agent-provider-claude": {},
		"@synara/cloud-agent-testkit":         {},
		"@synara/cloud-agent-distribution":    {},
	}
	candidate.SourceCommit = strings.TrimSpace(candidate.SourceCommit)
	candidate.CandidateDigest = strings.TrimSpace(candidate.CandidateDigest)
	candidate.StandaloneRuntimeSHA256 = strings.TrimSpace(candidate.StandaloneRuntimeSHA256)
	if !validFullBuildGitSHA(candidate.SourceCommit) ||
		!strings.HasPrefix(candidate.CandidateDigest, "sha256:") ||
		!validSHA256Hex(strings.TrimPrefix(candidate.CandidateDigest, "sha256:")) ||
		!strings.HasPrefix(candidate.StandaloneRuntimeSHA256, "sha256:") ||
		!validSHA256Hex(strings.TrimPrefix(candidate.StandaloneRuntimeSHA256, "sha256:")) {
		return errors.New("Worker image manifest Cloud Agent candidate identity is invalid")
	}
	if len(candidate.Packages) != len(required) {
		return errors.New("Worker image manifest Cloud Agent candidate must contain seven packages")
	}
	seen := make(map[string]struct{}, len(candidate.Packages))
	for index := range candidate.Packages {
		item := &candidate.Packages[index]
		item.Name = strings.TrimSpace(item.Name)
		item.Version = strings.TrimSpace(item.Version)
		item.SHA256 = strings.TrimSpace(item.SHA256)
		if _, expected := required[item.Name]; !expected ||
			!pinnedPackageVersionPattern.MatchString(item.Version) ||
			!strings.HasPrefix(item.SHA256, "sha256:") ||
			!validSHA256Hex(strings.TrimPrefix(item.SHA256, "sha256:")) {
			return fmt.Errorf("Worker image manifest Cloud Agent package %q is invalid", item.Name)
		}
		if _, duplicate := seen[item.Name]; duplicate {
			return fmt.Errorf("Worker image manifest Cloud Agent package %q is duplicated", item.Name)
		}
		seen[item.Name] = struct{}{}
	}
	sort.Slice(candidate.Packages, func(left, right int) bool {
		return candidate.Packages[left].Name < candidate.Packages[right].Name
	})
	return nil
}

func expectedWorkerImageCloudAgentAssets() map[string]workerImageCloudAgentExpectedAsset {
	return map[string]workerImageCloudAgentExpectedAsset{
		"@synara/cloud-agent-distribution": {
			Version: "0.1.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-distribution-0.1.0-rc.1.tgz",
			SHA256: "sha256:0326203b90a5be4f566b3d4282aea5fda8532753855a8a6b4b449776a966e29c",
		},
		"@synara/cloud-agent-protocol": {
			Version: "0.1.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-protocol-0.1.0-rc.1.tgz",
			SHA256: "sha256:e46c5a639482ac8af5c9794cda2c03e93779ce7d10cd1aac52761d6ec3cb2ac2",
		},
		"@synara/cloud-agent-provider-api": {
			Version: "0.1.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-provider-api-0.1.0-rc.1.tgz",
			SHA256: "sha256:88737cda1b0cef0922f50509acb154e2112a2d78acf219a1a5568edf3d731a59",
		},
		"@synara/cloud-agent-provider-claude": {
			Version: "0.1.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-provider-claude-0.1.0-rc.1.tgz",
			SHA256: "sha256:e6a3058fe21fc9799d242bd8c3ae7d8e7bcf7674f2003319d493d4afdda636fd",
		},
		"@synara/cloud-agent-provider-codex": {
			Version: "0.1.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-provider-codex-0.1.0-rc.1.tgz",
			SHA256: "sha256:8715b6c9d116d8b229c810903d1d7da16aaa6fed327020a06e0bd48813f49ec8",
		},
		"@synara/cloud-agent-runtime": {
			Version: "0.2.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-runtime-0.2.0-rc.1.tgz",
			SHA256: "sha256:89aaa8bc42b2527fee584dfa9bccbc3e994dc1c67b99166989ca20be9fcf6488",
		},
		"@synara/cloud-agent-testkit": {
			Version: "0.1.0-rc.1", URL: cloudAgentReleaseBaseURL + "synara-cloud-agent-testkit-0.1.0-rc.1.tgz",
			SHA256: "sha256:5e4031c8bc449a78e0d5dff6721c7ab708361a5c1b7efa5dfd833752d5add21c",
		},
	}
}

func validateWorkerImageCloudAgentLock(
	candidate *workerImageCloudAgentCandidate,
	lockfiles []workerImageLockfile,
	manifestPath string,
) error {
	var lockPath string
	for _, lockfile := range lockfiles {
		if lockfile.Name != "cloud-agent-candidate" {
			continue
		}
		resolved, err := resolveWorkerImageReferencePath(manifestPath, lockfile.Path)
		if err != nil {
			return errors.New("Worker image Cloud Agent candidate lock path is invalid")
		}
		lockPath = resolved
		break
	}
	if lockPath == "" {
		return errors.New("Worker image Cloud Agent candidate lock is missing")
	}
	encoded, err := readSmallRegularFile(lockPath, workerImageManifestMaximumSize)
	if err != nil {
		return errors.New("Worker image Cloud Agent candidate lock is unavailable")
	}
	var lock workerImageCloudAgentCandidateLock
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return errors.New("Worker image Cloud Agent candidate lock is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("Worker image Cloud Agent candidate lock is invalid")
	}
	expectedAssets := expectedWorkerImageCloudAgentAssets()
	if lock.SchemaVersion != 1 ||
		lock.Release.Repository != cloudAgentReleaseRepository ||
		lock.Release.Tag != cloudAgentReleaseTag ||
		lock.Release.SourceCommit != cloudAgentSourceCommit ||
		lock.StandaloneRuntime.URL != cloudAgentStandaloneURL ||
		lock.StandaloneRuntime.SHA256 != cloudAgentStandaloneSHA256 ||
		len(lock.Packages) != len(expectedAssets) {
		return errors.New("Worker image Cloud Agent candidate lock does not match the immutable RC")
	}
	names := make([]string, 0, len(expectedAssets))
	for name, expected := range expectedAssets {
		asset, found := lock.Packages[name]
		if !found || asset.Version != expected.Version || asset.URL != expected.URL || asset.SHA256 != expected.SHA256 {
			return fmt.Errorf("Worker image Cloud Agent candidate lock package %q does not match the immutable RC", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	normalizedPackages := make([]workerImageCloudAgentPackage, 0, len(names))
	for _, name := range names {
		asset := lock.Packages[name]
		fmt.Fprintf(&canonical, "%s@%s %s\n", name, asset.Version, asset.SHA256)
		normalizedPackages = append(normalizedPackages, workerImageCloudAgentPackage{
			Name: name, Version: asset.Version, SHA256: asset.SHA256,
		})
	}
	digestBytes := sha256.Sum256([]byte(canonical.String()))
	candidateDigest := "sha256:" + hex.EncodeToString(digestBytes[:])
	if lock.Release.CandidateDigest != candidateDigest {
		return errors.New("Worker image Cloud Agent candidate lock canonical digest is invalid")
	}
	if candidate.SourceCommit != lock.Release.SourceCommit ||
		candidate.CandidateDigest != candidateDigest ||
		candidate.StandaloneRuntimeSHA256 != lock.StandaloneRuntime.SHA256 ||
		len(candidate.Packages) != len(normalizedPackages) {
		return errors.New("Worker image manifest Cloud Agent candidate does not match its embedded lock")
	}
	for index := range normalizedPackages {
		if candidate.Packages[index] != normalizedPackages[index] {
			return errors.New("Worker image manifest Cloud Agent packages do not match their embedded lock")
		}
	}
	return nil
}

func validateWorkerImageBaseImages(images []workerImageBaseImage) error {
	required := map[string]struct{}{
		"agentd-build": {}, "provider-host-build": {}, "worker-runtime": {},
	}
	if len(images) != len(required) {
		return errors.New("Worker image manifest must contain exactly three required Base Images")
	}
	seen := make(map[string]struct{}, len(images))
	for index := range images {
		images[index].Name = strings.TrimSpace(images[index].Name)
		images[index].Reference = strings.TrimSpace(images[index].Reference)
		if _, expected := required[images[index].Name]; !expected {
			return fmt.Errorf("Worker image manifest contains unexpected Base Image %q", images[index].Name)
		}
		if _, duplicate := seen[images[index].Name]; duplicate {
			return fmt.Errorf("Worker image manifest contains duplicate Base Image %q", images[index].Name)
		}
		if !validPinnedImageReference(images[index].Reference) {
			return fmt.Errorf("Worker image manifest Base Image %q is not digest-pinned", images[index].Name)
		}
		seen[images[index].Name] = struct{}{}
	}
	return nil
}

func validateWorkerImageProviderRuntimes(runtimes []workerImageProviderRuntime) error {
	required := map[string]string{
		"codex\x00cli":       "@openai/codex",
		"claudeAgent\x00sdk": "@anthropic-ai/claude-agent-sdk",
		"claudeAgent\x00cli": "@anthropic-ai/claude-code",
	}
	if len(runtimes) != len(required) {
		return errors.New("Worker image manifest must contain exactly three required Provider runtimes")
	}
	seen := make(map[string]struct{}, len(runtimes))
	for index := range runtimes {
		runtime := &runtimes[index]
		runtime.Provider = strings.TrimSpace(runtime.Provider)
		runtime.Kind = strings.TrimSpace(runtime.Kind)
		runtime.Package = strings.TrimSpace(runtime.Package)
		runtime.Version = strings.TrimSpace(runtime.Version)
		key := runtime.Provider + "\x00" + runtime.Kind
		expectedPackage, expected := required[key]
		if !expected || runtime.Package != expectedPackage {
			return fmt.Errorf("Worker image manifest contains unexpected Provider runtime %q/%q", runtime.Provider, runtime.Kind)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("Worker image manifest contains duplicate Provider runtime %q/%q", runtime.Provider, runtime.Kind)
		}
		if !pinnedPackageVersionPattern.MatchString(runtime.Version) || len(runtime.Version) > 200 {
			return fmt.Errorf("Worker image manifest Provider runtime %q/%q has an invalid version", runtime.Provider, runtime.Kind)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateWorkerImageLockfiles(lockfiles []workerImageLockfile, manifestPath string) error {
	required := map[string]bool{
		"provider-tools-npm":    false,
		"cloud-agent-candidate": false,
		"worker-apk":            false,
	}
	seen := make(map[string]struct{}, len(lockfiles))
	for index := range lockfiles {
		lockfile := &lockfiles[index]
		lockfile.Name = strings.TrimSpace(lockfile.Name)
		lockfile.Path = strings.TrimSpace(lockfile.Path)
		lockfile.SHA256 = strings.TrimSpace(lockfile.SHA256)
		if !validManifestEntryName(lockfile.Name) {
			return errors.New("Worker image manifest contains an invalid Lockfile name")
		}
		if _, duplicate := seen[lockfile.Name]; duplicate {
			return fmt.Errorf("Worker image manifest contains duplicate Lockfile %q", lockfile.Name)
		}
		if !validSHA256Hex(lockfile.SHA256) {
			return fmt.Errorf("Worker image manifest Lockfile %q has an invalid SHA-256", lockfile.Name)
		}
		resolved, err := resolveWorkerImageReferencePath(manifestPath, lockfile.Path)
		if err != nil {
			return fmt.Errorf("Worker image manifest Lockfile %q has an invalid path", lockfile.Name)
		}
		actual, err := sha256RegularFile(resolved, workerImageReferenceMaximumSize)
		if err != nil || actual != lockfile.SHA256 {
			return fmt.Errorf("Worker image manifest Lockfile %q does not match its SHA-256", lockfile.Name)
		}
		if _, expected := required[lockfile.Name]; expected {
			required[lockfile.Name] = true
		}
		seen[lockfile.Name] = struct{}{}
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("Worker image manifest is missing required Lockfile %q", name)
		}
	}
	return nil
}

func validateWorkerImageSBOMs(sboms []workerImageSoftwareBill, manifestPath string) error {
	seen := make(map[string]struct{}, len(sboms))
	foundProviderTools := false
	for index := range sboms {
		sbom := &sboms[index]
		sbom.Name = strings.TrimSpace(sbom.Name)
		sbom.Format = strings.TrimSpace(sbom.Format)
		sbom.Path = strings.TrimSpace(sbom.Path)
		sbom.SHA256 = strings.TrimSpace(sbom.SHA256)
		if !validManifestEntryName(sbom.Name) {
			return errors.New("Worker image manifest contains an invalid SBOM name")
		}
		if _, duplicate := seen[sbom.Name]; duplicate {
			return fmt.Errorf("Worker image manifest contains duplicate SBOM %q", sbom.Name)
		}
		if sbom.Format != "spdx-json" || !validSHA256Hex(sbom.SHA256) {
			return fmt.Errorf("Worker image manifest SBOM %q is invalid", sbom.Name)
		}
		resolved, err := resolveWorkerImageReferencePath(manifestPath, sbom.Path)
		if err != nil {
			return fmt.Errorf("Worker image manifest SBOM %q has an invalid path", sbom.Name)
		}
		actual, err := sha256RegularFile(resolved, workerImageReferenceMaximumSize)
		if err != nil || actual != sbom.SHA256 {
			return fmt.Errorf("Worker image manifest SBOM %q does not match its SHA-256", sbom.Name)
		}
		if sbom.Name == "provider-tools" {
			foundProviderTools = true
		}
		seen[sbom.Name] = struct{}{}
	}
	if !foundProviderTools {
		return errors.New("Worker image manifest is missing the provider-tools SPDX JSON SBOM")
	}
	return nil
}

func resolveWorkerImageReferencePath(manifestPath, value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n\t\x00") {
		return "", errors.New("reference path is empty")
	}
	clean := filepath.Clean(value)
	if clean != value {
		return "", errors.New("reference path is not canonical")
	}
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("reference path escapes the manifest directory")
	}
	return filepath.Join(filepath.Dir(manifestPath), clean), nil
}

func sha256RegularFile(path string, maximum int64) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return "", errors.New("referenced file is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("referenced file is unavailable")
	}
	defer file.Close()
	opened, openedErr := file.Stat()
	current, currentErr := os.Lstat(path)
	if openedErr != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!opened.Mode().IsRegular() || !os.SameFile(before, current) || !os.SameFile(current, opened) ||
		opened.Size() != before.Size() {
		return "", errors.New("referenced file changed while it was opened")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil || written != opened.Size() || written > maximum {
		return "", errors.New("referenced file could not be hashed")
	}
	after, afterErr := os.Lstat(path)
	if afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(opened, after) || after.Size() != opened.Size() {
		return "", errors.New("referenced file changed while it was hashed")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (manifest workerImageManifest) featureFlagValue() map[string]any {
	baseImages := make([]map[string]any, 0, len(manifest.BaseImages))
	for _, image := range manifest.BaseImages {
		baseImages = append(baseImages, map[string]any{"name": image.Name, "reference": image.Reference})
	}
	lockfiles := make([]map[string]any, 0, len(manifest.Lockfiles))
	for _, lockfile := range manifest.Lockfiles {
		lockfiles = append(lockfiles, map[string]any{"name": lockfile.Name, "sha256": lockfile.SHA256})
	}
	providerRuntimes := make([]map[string]any, 0, len(manifest.ProviderRuntimes))
	for _, providerRuntime := range manifest.ProviderRuntimes {
		providerRuntimes = append(providerRuntimes, map[string]any{
			"provider": providerRuntime.Provider,
			"kind":     providerRuntime.Kind,
			"package":  providerRuntime.Package,
			"version":  providerRuntime.Version,
		})
	}
	sboms := make([]map[string]any, 0, len(manifest.SBOMs))
	for _, sbom := range manifest.SBOMs {
		sboms = append(sboms, map[string]any{
			"name": sbom.Name, "format": sbom.Format, "sha256": sbom.SHA256,
		})
	}
	return map[string]any{
		"schemaVersion": manifest.SchemaVersion,
		"source": map[string]any{
			"version": manifest.Source.Version,
			"gitSha":  manifest.Source.GitSHA,
		},
		"platform": map[string]any{
			"os": manifest.Platform.OS, "architecture": manifest.Platform.Architecture,
		},
		"baseImages":       baseImages,
		"lockfiles":        lockfiles,
		"providerRuntimes": providerRuntimes,
		"cloudAgentCandidate": map[string]any{
			"sourceCommit":            manifest.CloudAgentCandidate.SourceCommit,
			"candidateDigest":         manifest.CloudAgentCandidate.CandidateDigest,
			"standaloneRuntimeSha256": manifest.CloudAgentCandidate.StandaloneRuntimeSHA256,
			"packages":                manifest.CloudAgentCandidate.Packages,
		},
		"sboms": sboms,
	}
}

func validateWorkerImageBuildFeatureFlagReservation(capabilities map[string]any) error {
	raw, found := capabilities["featureFlags"]
	if !found {
		return nil
	}
	featureFlags, ok := raw.(map[string]any)
	if !ok || featureFlags == nil {
		return errors.New("SYNARA_AGENTD_CAPABILITIES_JSON featureFlags must be a JSON object")
	}
	if _, reserved := featureFlags[workerImageBuildFeatureFlag]; reserved {
		return errors.New("SYNARA_AGENTD_CAPABILITIES_JSON featureFlags.workerImageBuild is reserved for agentd")
	}
	return nil
}

func resolveWorkerBuildIdentity(
	environmentVersion, environmentGitSHA string,
	manifest *workerImageManifest,
) (string, string, error) {
	version := strings.TrimSpace(environmentVersion)
	gitSHA := strings.TrimSpace(environmentGitSHA)
	if manifest == nil {
		if version == "" {
			version = "dev"
		}
		if !validWorkerBuildVersion(version) || (gitSHA != "" && !validBuildGitSHA(gitSHA)) {
			return "", "", errors.New("agentd build version or Git SHA is invalid")
		}
		return version, gitSHA, nil
	}
	if version == "" {
		version = manifest.Source.Version
	} else if version != manifest.Source.Version {
		return "", "", errors.New("SYNARA_AGENTD_VERSION does not match the Worker image manifest")
	}
	if gitSHA == "" {
		gitSHA = manifest.Source.GitSHA
	} else if gitSHA != manifest.Source.GitSHA {
		return "", "", errors.New("SYNARA_AGENTD_BUILD_GIT_SHA does not match the Worker image manifest")
	}
	return version, gitSHA, nil
}

func validWorkerBuildVersion(value string) bool {
	return value != "" && len(value) <= 160 && !strings.ContainsAny(value, "\r\n\t\x00")
}

func validFullBuildGitSHA(value string) bool {
	return (len(value) == 40 || len(value) == 64) && validBuildGitSHA(value)
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validImageDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}

func validPinnedImageReference(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\t \x00") ||
		strings.Count(value, "@sha256:") != 1 {
		return false
	}
	parts := strings.SplitN(value, "@sha256:", 2)
	if len(parts) != 2 || !validSHA256Hex(parts[1]) {
		return false
	}
	lastSlash := strings.LastIndex(parts[0], "/")
	lastColon := strings.LastIndex(parts[0], ":")
	return lastColon > lastSlash && lastColon < len(parts[0])-1
}

func validManifestEntryName(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

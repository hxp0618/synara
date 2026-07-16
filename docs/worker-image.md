# Synara Worker image

The production Worker image is built through the checked-in Buildx entrypoint. Do not publish or deploy
`synara-worker:latest`.

```bash
deploy/worker/build.sh \
  --image registry.example.com/synara-worker:0.5.3 \
  --load
```

The script derives the full Git SHA, product version, and Git commit timestamp and writes Buildx metadata under
`build/`. It refuses a dirty worktree because image contents would no longer match the declared Git SHA.
`--allow-dirty` is only for a local verification build. A local `--load` build keeps the embedded normalized SPDX
document but disables outer attestations because the Docker image store cannot retain them; `--push` enables
BuildKit SPDX and `mode=max` provenance attestations through a `docker-container` builder.

Publish a multi-platform image and preserve its attestations with:

```bash
deploy/worker/build.sh \
  --image registry.example.com/synara-worker:0.5.3 \
  --platform linux/amd64,linux/arm64 \
  --push
```

The image contains Node.js 24, `synara-agentd`, `provider-host`, Codex CLI, Claude Agent SDK, Claude Code CLI,
and a writable Workspace root. It runs as non-root UID/GID 10001 with no embedded registration, Lease,
Provider, Git, or cloud credentials.

## Reproducible inputs

The Worker build fails closed unless all of these inputs are immutable:

- `agentd-build`, `provider-host-build`, and `worker-runtime` base images use full `sha256` digests.
- `deploy/worker/provider-tools/package-lock.json` pins npm integrity hashes for Codex CLI, Claude Code CLI, and
  npm `12.0.1`. The final Worker removes the older npm bundled in the Node base image and points npm, npx, and
  node-gyp at the locked tree; npm's transient `/tmp/node-compile-cache` is removed in the producing layer.
- `bun.lock` pins the Provider Host and Claude Agent SDK graph.
- `deploy/worker/apk-packages.lock` pins the complete Alpine package closure installed over the runtime base.
- `deploy/worker/buildkit-sbom-generator.lock` pins the BuildKit Syft scanner image used for outer SPDX
  attestations; release builds never resolve the mutable `stable-1` tag.
- `SOURCE_DATE_EPOCH` fixes the embedded SPDX creation time to the source commit time.
- Registry exports rewrite every generated layer timestamp to `SOURCE_DATE_EPOCH`; the build removes
  timestamp-bearing APK logs in the producing layer and consumes the raw npm SBOM through a read-only BuildKit
  mount so neither transient file enters the final layer history. `/opt/synara/.build-revision` carries the clean
  Git SHA into the Worker rootfs cache key, and copied agentd/Provider Host/Provider tool mtimes are normalized to
  prevent an older unrelated cache entry from changing a release digest.

The tracked Provider runtime versions are intentionally separate:

```text
Codex CLI:        @openai/codex@0.144.1
Claude SDK:       @anthropic-ai/claude-agent-sdk@0.3.207
Claude Code CLI:  @anthropic-ai/claude-code@2.1.197
```

When upgrading one Provider runtime, update its package declaration and lockfile together, rebuild the image,
inspect the embedded manifest/SBOM, and run the shared Provider acceptance suite before changing a deployment
digest. Never replace a package version in the Dockerfile without updating the corresponding lock.

Pinned APK versions prevent a repository update from silently changing the image. If the external Alpine mirror
no longer serves a locked artifact, the build fails instead of selecting a newer package; long-term release
retention therefore also requires an operator-controlled package mirror or archived build cache.

## Embedded manifest and SBOM

Every official Worker image contains:

```text
/opt/synara/worker-image-manifest.json
/opt/synara/provider-tools.spdx.json
/opt/synara/provider-tools/package-lock.json
/opt/synara/provider-host/bun.lock
/opt/synara/worker-apk-packages.lock
/opt/synara/.build-revision
```

The version manifest records schema version, source version and full Git SHA, target OS/architecture, immutable
base image references, lockfile SHA-256 values, the three Provider runtime versions, and normalized SPDX hashes.
It deliberately excludes build-host paths, build time, credentials, and the final image digest.

`npm sbom` normally emits a current timestamp and a generated namespace. The Worker build normalizes those fields
using `SOURCE_DATE_EPOCH` and the provider lock hash before hashing the SPDX JSON, so repeated builds do not drift
solely because of SBOM metadata.

The image sets `SYNARA_AGENTD_WORKER_IMAGE_MANIFEST_PATH` to the embedded manifest. Agentd treats a missing,
malformed, unknown-schema, or hash-mismatched file as a startup error. Explicit `SYNARA_AGENTD_VERSION` and
`SYNARA_AGENTD_BUILD_GIT_SHA` values must match the embedded source identity. Local and SSH Workers that run a
standalone agentd binary do not set this variable and retain their existing environment-based identity.

The final image digest cannot be embedded in the image that it hashes. Production Docker and Kubernetes Targets
must use a digest-qualified reference such as:

```text
registry.example.com/synara-worker@sha256:...
```

The reconcilers extract that digest and pass it to agentd as `SYNARA_AGENTD_IMAGE_DIGEST`. Tag-only development
images remain usable, but cannot provide immutable image-digest evidence in the Worker Manifest.

Inspect a locally loaded image with:

```bash
docker run --rm --entrypoint sh synara-worker:0.5.3 -euxc '
  test "$(id -u)" = 10001
  sha256sum \
    /opt/synara/worker-image-manifest.json \
    /opt/synara/provider-tools.spdx.json \
    /opt/synara/provider-tools/package-lock.json \
    /opt/synara/provider-host/bun.lock \
    /opt/synara/worker-apk-packages.lock
  codex --version
  claude --version
'
```

For release builds, retain the Buildx metadata file and registry attestations alongside the digest. Buildx
provenance describes the outer build environment and can differ in attestation metadata; equivalent Worker image
inputs are established by the embedded manifest, normalized SBOM, lock hashes, binaries, and installed package
set.

## Registry release gate

Run the consolidated Registry gate only from a clean, committed checkout. The selected Buildx builder must use
the `docker-container` driver and expose both `linux/amd64` and `linux/arm64`. Docker/Buildx Registry
authentication, when required, must already be configured outside the command; credentials are rejected in the
repository and GOPROXY arguments and are never written to the report.

```bash
python3 scripts/stage3-provider-acceptance/registry_release_gate.py \
  --image-repository registry.example.com/synara/worker \
  --builder synara-worker-release \
  --supply-chain-timeout 1800 \
  --output-dir /tmp/synara-worker-registry-release
```

If the default Go module proxy is unavailable, append the public credential-free override
`--go-proxy https://goproxy.cn,direct`.
For a disposable local HTTP Registry only, add `--insecure-registry`; production Registry runs must use TLS.

The gate pushes two uniquely tagged builds from the same Git SHA: one normal cached build and one independent
`--no-cache` build. The Registry exporter rewrites layer timestamps to `SOURCE_DATE_EPOCH`; transient APK logs and
the pre-normalized npm SBOM are excluded from final layers. The gate requires both builds to reproduce the same
platform manifest digests and validates:

- the Registry-returned OCI index digest and exactly one `linux/amd64` plus one `linux/arm64` image manifest;
- one attached attestation manifest per platform containing both SPDX and SLSA provenance predicates;
- non-root UID/GID, entrypoint, working directory, source labels, fixed creation time, and no credential-like
  environment names;
- embedded Worker Manifest, normalized SPDX, checked-in npm/Bun/APK locks, Provider Host wrapper/bundle, and
  agentd binary evidence for both platforms;
- digest-pinned Cosign and Trivy containers from `deploy/worker/supply-chain-tools.lock` with runtime versions
  matching their checked-in tags;
- an isolated ephemeral Cosign key that signs both OCI index digests and verifies exact Git SHA, version, run ID,
  slot, Registry identity, and manifest digest claims before the private key is removed;
- both platform manifests scanned by Trivy for vulnerabilities and Secret-like material under
  `deploy/worker/vulnerability-policy.json`, including vulnerability-database freshness and OS end-of-life checks;
- exact local inspection cleanup, report redaction, and an empty output Secret scan without Docker-wide prune.

The JSON and Markdown reports are written to the requested empty output directory. The gate intentionally retains
the two remote image tags and their signatures as release evidence; apply the Registry retention policy only after
their digests, attestations, signatures, scan summaries, and tool/database identities are archived. Ephemeral-key
signing proves the cryptographic Registry path and exact source claims, but it does not satisfy a production
KMS/keyless identity or transparency-log policy. A pass also does not prove production Registry Credential and
retention, real Provider rollout across all four Targets, multi-node canary/rollback, or soak.

## Runtime and storage

Docker and Kubernetes Execution Target configuration should use the official Worker image, not the older
`synara-agentd` example.

Managed Workers explicitly select Provider Host Protocol v2. Agentd performs Describe/compatibility
gating before registration and again before the actual Provider Turn; v1 requires the documented
operator-only compatibility switch and is never an automatic execution fallback.

Production deployments should configure CPU, memory, ephemeral storage, a read-only root filesystem, disabled
ServiceAccount token automount, and a dedicated Workspace volume or `emptyDir` according to recovery requirements.
Configure separate writable Workspace and Git cache roots. Docker Workers for one Target may share both roots on
the target-scoped volume because agentd uses cross-process locks and private Workspace repositories. Kubernetes
keeps the cache Pod-local by default; an optional dedicated cache PVC must provide RWX-equivalent access and
reliable POSIX locking before it is used across Pods.

## Provider process environment

Agentd and Provider Host build child-process environments from an explicit runtime allowlist. Ambient Worker
credentials, Control Plane/Lease tokens, cloud credentials, database/object-store settings, GitHub tokens,
`NODE_OPTIONS`, SSH Agent sockets, and standard proxy variables are not inherited by Codex or Claude. Provider
credentials continue to use the Provider Host credential file descriptor and provider-specific field allowlists.

Directly operated and Local Workers that require an outbound proxy must configure the explicit Provider-only
inputs below instead of ambient `HTTP_PROXY` variables:

```text
SYNARA_PROVIDER_HTTP_PROXY
SYNARA_PROVIDER_HTTPS_PROXY
SYNARA_PROVIDER_ALL_PROXY
SYNARA_PROVIDER_NO_PROXY
```

Provider Host validates these values, maps them to the standard proxy names only in the Provider child
environment, and redacts authenticated proxy URLs and credentials from Provider diagnostics. Do not use this
channel for Control Plane, Git Workspace, database, or object-store proxy configuration; those processes retain
their own separately scoped network settings. Managed SSH, Docker, or Kubernetes Targets must expose these values
through their target-specific encrypted configuration/Secret plumbing before use; host-level ambient proxy values
are intentionally not treated as that plumbing.

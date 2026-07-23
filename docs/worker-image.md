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
Codex CLI:        @openai/codex@0.145.0
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

The default command below uses the checked-in disposable profile (`--signing-policy-profile disposable`) and
`deploy/worker/signing-policy.json`:

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

### Signing policy

The checked-in default `deploy/worker/signing-policy.json` is `ephemeral-key`, which proves exact-digest signing
mechanics without claiming a production identity or transparency log. The generic signing-policy schema still
distinguishes `ephemeral-key`, `keyless`, and `kms-key`, but the current production selector is the checked-in pair
`deploy/worker/production-signing-policy.json` plus `deploy/worker/production-signing-profile.json`. That
production profile is pinned to Vault Transit KMS (`hashivault://synara-worker-release`), Rekor upload and
verification, and Kyverno admission.

Keyless example using exact certificate identity and issuer:

```json
{
  "schemaVersion": 1,
  "mode": "keyless",
  "requireTransparencyLog": true,
  "keyReference": null,
  "credentialEnvironment": [],
  "identityTokenEnvironment": "SYNARA_COSIGN_IDENTITY_TOKEN",
  "certificateIdentity": "https://github.com/example/synara/.github/workflows/release.yml@refs/tags/v1",
  "certificateIdentityRegexp": null,
  "certificateOidcIssuer": "https://token.actions.githubusercontent.com",
  "certificateOidcIssuerRegexp": null
}
```

KMS example:

```json
{
  "schemaVersion": 1,
  "mode": "kms-key",
  "requireTransparencyLog": true,
  "keyReference": "awskms:///arn:aws:kms:us-east-1:123456789012:key/example",
  "credentialEnvironment": ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"],
  "identityTokenEnvironment": null,
  "certificateIdentity": null,
  "certificateIdentityRegexp": null,
  "certificateOidcIssuer": null,
  "certificateOidcIssuerRegexp": null
}
```

Use either an exact certificate field or its regexp alternative for identity and issuer, never both. Regexps must
be anchored and Cosign RE2-compatible. Environment names are policy input; their values must be injected only into
the gate process and never written to JSON, shell arguments, CI logs, or Docker configuration. The keyless token is
stored in a gate-owned `0600` file for Cosign and deleted in `finally`; KMS values are passed only through the named
container environment. Reports retain only non-Secret identity, policy SHA, tlog conclusion, and cleanup evidence.

### Current production profile

Before a production run, finish any long-running rollout/load/soak gates, then start the operator-owned short-lived
credential shell. For the repository-owned Stage 3 production-like overlay observed on July 21, 2026, the live
non-secret runtime values are `kind-synara-stage3-prod`, `synara-kms`, `192.168.139.3:5443/synara/worker`, and
Registry container `synara-stage3-prod-registry`. Pass only environment-variable names for Registry auth and CA
paths; never inline their values:

```bash
export SYNARA_STAGE3_KMS_RUNTIME=/secure/synara-stage3-kms-runtime
export SYNARA_VAULT_INIT_JSON=/secure/synara-vault/init.json
export VAULT_ADDR=<approved-live-vault-address>
export VAULT_CACERT="$SYNARA_STAGE3_KMS_RUNTIME/ca.crt"

"$SYNARA_STAGE3_KMS_RUNTIME/bin/start-short-lived-credential-session.py"

kubectl --context kind-synara-stage3-prod -n synara-system get configmap synara-worker-cosign-public-key -o yaml \
  > "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-cosign-public-key.live.yaml"
kubectl --context kind-synara-stage3-prod -n synara-system get configmap synara-worker-signing-settings -o yaml \
  > "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-signing-settings.live.yaml"

python3 scripts/stage3-provider-acceptance/registry_release_gate.py \
  --image-repository 192.168.139.3:5443/synara/worker \
  --builder synara-worker-release \
  --signing-policy-profile production \
  --registry-auth-username-env REGISTRY_USERNAME \
  --registry-auth-password-env REGISTRY_PASSWORD \
  --registry-ca-cert-env REGISTRY_CA_CERT \
  --production-public-key-configmap "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-cosign-public-key.live.yaml" \
  --production-repository-configmap "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-signing-settings.live.yaml" \
  --production-registry-config "$SYNARA_STAGE3_KMS_RUNTIME/registry-production.yml" \
  --production-registry-retention-policy "$SYNARA_STAGE3_KMS_RUNTIME/registry-retention-policy.json" \
  --production-registry-container synara-stage3-prod-registry \
  --production-registry-runtime-config-path /etc/distribution/config.yml \
  --output-dir /tmp/synara-worker-registry-release
```

In `production` mode the gate binds the clean Git SHA to the checked-in production signing source set:

- `deploy/worker/production-signing-policy.json`
- `deploy/worker/production-signing-profile.json`
- `deploy/kubernetes/security/cluster/verify-synara-worker-images.yaml`
- `deploy/kubernetes/security/namespaced/synara-worker-cosign-public-key-configmap.yaml`
- `deploy/kubernetes/security/namespaced/synara-worker-signing-settings-configmap.yaml`
- `deploy/kubernetes/security/production/kustomization.yaml`

It also validates the runtime ConfigMap YAML inputs against the configured image repository and the public key
exported from the current Vault Transit KMS key. The gate reads the named running Registry container's configuration
at `--production-registry-runtime-config-path` and binds its container/image identity, TLS certificate, auth mode,
repository, and deletion/retention settings to the exported production configuration and checked-in retention
contract. A production run fails if TLS is missing, Registry auth is not passed by environment-variable name, live
runtime evidence is stale or drifts, the runtime ConfigMaps drift from the KMS key or repository pattern, or the
isolated state, materialized Vault/Registry CA files, and Registry auth config are not removed exactly. A static
configuration file or disposable loopback Registry alone is not live production Registry evidence.

The checked-in Registry runtime pin is
`registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373`. The gate
requires the container's requested `Config.Image`, top-level runtime Image ID, and the ID's inspected RepoDigest to
bind to that exact pin; a mutable tag, alias repository, wrong digest, or Image-ID mismatch fails closed.

### Vault KMS admission gate

After a passing production `registry_release_gate.py` run, verify the live Vault Transit, Registry, Rekor-backed
signature, and Kyverno admission boundary with `vault_kms_admission_gate.py`. The default `--admission-mode` is
`verify-existing`, which checks the current live ConfigMaps and `ClusterPolicy` instead of applying a temporary
bundle:

```bash
python3 scripts/stage3-provider-acceptance/vault_kms_admission_gate.py \
  --kube-context kind-synara-stage3-prod \
  --vault-namespace synara-kms \
  --security-namespace synara-system \
  --admission-test-namespace synara-admission \
  --vault-bin "$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active" \
  --expected-approle-policy synara-worker-release-signer \
  --registry-release-gate-report /tmp/synara-worker-registry-release/worker-registry-release-gate.json \
  --unsigned-image-ref 192.168.139.3:5443/synara/worker@sha256:<unsigned-digest> \
  --wrong-key-image-ref 192.168.139.3:5443/synara/worker@sha256:<wrong-key-digest> \
  --tag-drift-image-ref 192.168.139.3:5443/synara/worker:synara-stage3-tag-drift-<unique-run-id> \
  --output-dir /tmp/synara-worker-vault-kms-admission
```

The tag-drift probe must use a gate-owned run-scoped tag that resolves to a non-baseline digest and is removed by
exact ownership after the run. It must not read, replace, or reuse `latest`.

The production Vault deployment is pinned to Helm chart `hashicorp/vault` `0.34.0`, release `synara-vault`,
namespace `synara-kms`, and image
`hashicorp/vault:2.0.3@sha256:a296a888b118615dc01d5f1a6846e6d4a7277946caaed5b447008fff5fe06b54`.
The signer identity is AppRole `auth/approle/role/synara-worker-release-signer`; it may call only the audited
`transit/sign/synara-worker-release` path for KMS reference `hashivault://synara-worker-release`. Cosign receives a
short-lived policy-scoped token via the `VAULT_ADDR`, `VAULT_TOKEN`, and `VAULT_CACERT` environment names. The
helper shell additionally exports only `VAULT_OPERATOR_TOKEN`, `VAULT_SNAPSHOT_OPERATOR_ROLE_ID`,
`VAULT_SNAPSHOT_OPERATOR_SECRET_ID`, `VAULT_SNAPSHOT_RESTORE_KEY_1`, `VAULT_SNAPSHOT_RESTORE_KEY_2`,
`VAULT_SNAPSHOT_RESTORE_KEY_3`, `REGISTRY_USERNAME`, `REGISTRY_PASSWORD`, and `REGISTRY_CA_CERT`. The production
tlog policy requires upload and online verification against public Rekor `https://rekor.sigstore.dev`,
including an inclusion proof and signed entry timestamp. Kyverno admission is fail-closed and enforced, mutates
matching tags to digests, and verifies the exact digest signature using the live `synara-system` ConfigMaps.
Vault `lookup-self` must additionally prove an AppRole `batch` orphan token with only the
`synara-worker-release-signer` policy. Reports retain only the role/type/orphan/policy-count fields and the policy
list SHA256, never the token or Credential values.

The gate reads only `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_CACERT`, `REGISTRY_USERNAME`, `REGISTRY_PASSWORD`, and
`REGISTRY_CA_CERT` by environment-variable name; it does not persist their values. It re-checks the clean current
source boundary against the passing production `registry_release_gate.py` report, verifies the exported Vault public
key and live Registry repository pattern, then requires one positive signed-image admission plus negative unsigned,
wrong-key, and tag-drift probes. It fails closed if source hashes drift, if the passing Registry report retained
secret-like findings, or if isolated state or exact-owner temporary admission resources are not removed.

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
- the checked-in Cosign signing policy, which signs both OCI index digests and verifies exact Git SHA, version,
  run ID, slot, Registry identity, and manifest digest claims; disposable mode removes its private key, while
  the current production Vault Transit KMS profile additionally requires TLS, approved Registry auth/CA environment
  names, current runtime admission ConfigMap inputs, Rekor upload and verification, and clean removal of all
  materialized secret state;
- both platform manifests scanned by Trivy for vulnerabilities and Secret-like material under
  `deploy/worker/vulnerability-policy.json`, including vulnerability-database freshness and OS end-of-life checks;
- exact local inspection cleanup, report redaction, and an empty output Secret scan without Docker-wide prune.

The JSON and Markdown reports are written to the requested empty output directory. The gate intentionally retains
the two remote image tags and their signatures as release evidence; apply the Registry retention policy only after
their digests, attestations, signatures, scan summaries, and tool/database identities are archived. A default
ephemeral-key pass proves the cryptographic Registry path and exact source claims, but it does not satisfy the
current production Vault Transit KMS, Rekor, or Kyverno admission policy. A production-mode pass must also archive
its Registry release report, production admission verification report, KMS identity, transparency-log conclusion,
and the checked-in Registry retention-policy boundary. Neither mode alone proves real Provider rollout across all
four Targets, multi-node canary/rollback, or soak.

## Docker Release Revision rollout gate

After the supply-chain boundary is green, use the clean-SHA Docker rollout gate to prove that Registry-returned
digests survive the product Release Revision, managed Worker pool, and Execution scheduling paths:

```bash
python3 scripts/stage3-provider-acceptance/docker_worker_release_rollout_gate.py \
  --go-proxy https://goproxy.cn,direct \
  --output-dir /tmp/synara-docker-worker-release-rollout \
  --timeout 3600
```

This command owns a loopback-only disposable Registry. It builds two single-platform Worker acceptance images from
one clean Git SHA with different controlled build versions, creates a two-Worker main Docker Target and a separate
candidate observer, and drives immutable Revision creation, initial promote, canary, active-Execution fencing,
promote, and rollback through the user API. A pass requires the Registry digest, Worker Manifest, Docker container
labels/environment, and `turn.created` / `execution.leased` release pins to agree at every stage. It also requires
strict-CAS rejection, immutable Transition/Audit/Outbox history, one terminal per Execution, exact cleanup, and an
empty output Secret scan.

The local Registry intentionally has no TLS or Registry Credential. Its result closes only deterministic managed
Docker rollout mechanics; production Registry auth/retention, keyless or KMS identity, Kubernetes multi-node
rollout, real Provider credentials, load, and soak remain separate gates.

## Kubernetes real Provider Release Revision rollout gate

After both production supply-chain gates are green, run the immutable Kubernetes rollout separately for Codex and
Claude. The following template uses only controlled environment-variable names and supports third-party Base URLs,
keys, and custom models:

```bash
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/kubernetes_real_provider_release_rollout_gate.py \
  --provider codex \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 5400 \
  --output-dir /tmp/synara-kubernetes-real-provider-codex-rollout

python3 scripts/stage3-provider-acceptance/kubernetes_real_provider_release_rollout_gate.py \
  --provider claudeAgent \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 5400 \
  --output-dir /tmp/synara-kubernetes-real-provider-claude-rollout
```

The default six nominal waves split three/three between candidate promotion and baseline rollback. Each phase also
has to reach `minimumDurationSeconds: 1800`, continuing whole waves as needed, so the load portion alone takes at
least about 60 minutes. A pass proves real Provider Turns, two immutable Registry digests, canary/promote/rollback,
Pod-loss recovery, one Pod/Worker identity per Execution, distinct-node overlap on two schedulable non-control-plane
Nodes, same-Session native Cursor continuity across revisions, release-pinned Audit/Outbox history, exact cleanup,
and output scanning.

This gate's Registry is disposable loopback HTTP without TLS or authentication. It does not replace production
Registry live evidence from `registry_release_gate.py` or Vault/Kyverno/Rekor evidence from
`vault_kms_admission_gate.py`.

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

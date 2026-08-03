# Release Checklist

This document covers build-only native validation and publishing desktop releases from one tag.

## What the workflow does

- Triggers:
  - Manual dispatch defaults to build-only validation and uploads workflow artifacts without publishing anything.
  - A pushed tag matching `v*.*.*` publishes after successful builds.
  - Manual publication requires the explicit `publish_release=true` input.
- Runs quality gates first: lint, typecheck, test.
- Builds four artifacts in parallel:
  - macOS `arm64` DMG
  - macOS `x64` DMG
  - Linux `x64` AppImage
  - Windows `x64` NSIS installer
- Publishes one versioned GitHub Release with all produced files.
  - Versions with a suffix after `X.Y.Z` (for example `1.2.3-alpha.1`) are published as GitHub prereleases.
  - Stable 0.5.x releases are GitHub Latest; the 0.4.x compatibility release remains historical.
- Publishes default `latest*.yml` metadata plus byte-identical `synara*.yml` aliases on every stable release so existing packaged binaries keep working.
- Keeps the historical 0.4.x compatibility release unchanged; current stable payloads stay on their own GitHub Latest release.
- Publishes prerelease installers only on their versioned GitHub prerelease; prereleases never replace the stable `synara` update manifests.
- Publishes the CLI package (`apps/server`, npm package `@synara/cli`) with OIDC trusted publishing.
- Published macOS and Windows artifacts must be signed. Build-only runs may
  produce unsigned artifacts when signing secrets are unavailable.
- Every native matrix job uses GitHub `actions/attest@v4` to create signed SLSA build provenance for the collected payloads
  and provenance manifest. The retained `artifact-<platform>-<arch>.attestation.json` bundle is uploaded with the workflow
  artifact and published release. The same job verifies the primary installer with `gh attestation verify` and retains its
  bounded JSON result as `artifact-<platform>-<arch>.attestation-verification.json`; verification pins the repository,
  `.github/workflows/release.yml` signer, candidate source commit and GitHub-hosted runner boundary.

## Desktop auto-update notes

- Runtime updater: `electron-updater` in `apps/desktop/src/main.ts`.
- Update UX:
  - Background checks run on startup delay + interval.
  - New updates are prepared/downloaded in the background after detection; install/restart stays manual.
  - The desktop UI shows a rocket update button while preparing and switches to an install action once the update is ready.
- Provider: GitHub Releases (`provider: github`) configured at build time.
- Repository visibility: public. The authenticated private-repository provider does not honor custom channel filenames.
- Runtime channel: `synara`. Stable 0.5.x releases publish both `latest` and `synara` metadata; the 0.4.x compatibility release remains available for historical migration.
- Repository slug source:
  - `SYNARA_DESKTOP_UPDATE_REPOSITORY` (format `owner/repo`), if set.
  - otherwise `GITHUB_REPOSITORY` from GitHub Actions.
- Required Synara release assets for updater:
  - platform installers (`.exe`, `.dmg`, `.AppImage`, plus macOS `.zip` for Squirrel.Mac update payloads)
  - `synara-mac.yml`, `synara.yml`, and `synara-linux.yml` metadata
  - every stable release includes both `synara-mac.yml`, `synara.yml`, `synara-linux.yml` and `latest-mac.yml`, `latest.yml`, `latest-linux.yml`
  - `*.blockmap` files, except the macOS update `.zip.blockmap` removed after zip repack
- Enforced upgrade path:
  - Stable clean Synara releases are created with `make_latest=true` and carry both six-manifest filenames in the versioned release.
  - The historical 0.4.x compatibility release remains available for predecessor migration and is never overwritten by a 0.5.x release.
  - Clean releases do not mirror payloads onto the historical compatibility release, so the 0.4.x line remains immutable.
  - Clean-release publication fails closed if either the default Latest manifests or the dedicated `synara` aliases are missing.
- Production desktop builds omit web/server/desktop source maps by default to keep update payloads small. Set `SYNARA_WEB_SOURCEMAP=1`, `SYNARA_SERVER_SOURCEMAP=1`, or `SYNARA_DESKTOP_SOURCEMAP=1` only for a diagnostic release that needs them.
- macOS metadata note:
  - The build initially emits `latest-mac.yml` for both Intel and Apple Silicon.
  - The workflow merges the per-arch macOS metadata, then keeps the merged manifest as `latest-mac.yml` and copies it to `synara-mac.yml` for stable releases.
  - The desktop build script repacks the macOS update `.zip` with `ditto`, verifies Electron framework symlinks, extracts the zip, validates the extracted app signature, patches the matching `latest-mac*.yml` hash/size, and removes the stale `.zip.blockmap`.
  - Signed publication provenance re-extracts that ZIP and read-only mounts the final DMG. Both must contain exactly one real top-level `Synara.app`; every nested Mach-O must contain the requested architecture, pass deep signing, and share the expected Apple Team ID before either artifact can enter the same-run release set.
  - macOS updater downloads intentionally use the full zip payload so Squirrel.Mac installs the exact signed archive validated by release build.
- Local smoke test:
  - Run `bun run release:smoke:mac-update -- --skip-build --build-version 0.1.5` on macOS after local desktop/server/web dist files exist.
  - The smoke builds a mock update artifact, validates manifest hash/size, serves a HEAD-only local endpoint, confirms the manifest and zip are addressable without downloading the zip body, then cleans up its temp output.
  - Boolean env flags for release scripts accept `true/false`, `1/0`, `yes/no`, and `on/off`; CLI flags are still preferred for repeatable local commands.

## Desktop Enrollment packaging and deployment

The release gate is defined by
[`Desktop installed acceptance v1`](contracts/desktop-installed-acceptance-v1.md) and the
[`native release acceptance runbook`](runbooks/desktop-native-release-acceptance.md). Validate the final four-host bundle
with `scripts/stage6-desktop/validate_desktop_native_acceptance.py`; build/startup smoke alone is not installed acceptance.

- Packaged macOS, Windows, and Linux artifacts declare `synara://` as the Desktop Enrollment
  protocol. Canary must use its separately declared `synara-canary://` identity before a Canary
  Enrollment release is approved.
- Set `SYNARA_DESKTOP_CONTROL_PLANE_ORIGINS` to a comma-separated list of exact normalized public
  Control Plane base URLs accepted by that Desktop deployment. HTTPS is required. A fixed deployment
  path such as `https://gateway.example/control-plane` is supported and is part of the exact match.
- Generic public builds intentionally have an empty allowlist until the deployment config supplies
  one. A parsed link whose `control_plane` value is absent from the allowlist performs no network I/O.
- Development loopback HTTP requires both an explicit loopback URL in
  `SYNARA_DESKTOP_CONTROL_PLANE_ORIGINS` and
  `SYNARA_DESKTOP_ALLOW_LOOPBACK_CONTROL_PLANE=1`. Packaged builds ignore that loopback opt-in.
- Development protocol registration is off by default. Set
  `SYNARA_DESKTOP_REGISTER_PROTOCOL=1` only while testing the local Electron entry point.
- Before release approval, create a fresh short-lived self-Enrollment for every attempt and verify
  the installed, signed artifact through the OS launcher:
  - macOS: `open 'synara://connect?...'`;
  - Windows: `Start-Process 'synara://connect?...'`;
  - Linux: `xdg-open 'synara://connect?...'`.
- For every target, record protocol dispatch, OS credential-store backend, complete initial
  Session/Tenant/Organization/Entitlement hydration, restart persistence, rotation, remote
  revocation, local disconnect, and secret-scan evidence. Source tests and an unpackaged Electron
  run do not satisfy this gate.

## 0) npm OIDC trusted publishing setup (CLI)

The workflow publishes the CLI with `bun publish` from `apps/server` after bumping
the package version to the release tag version.

Checklist:

1. Confirm the npm account controls the `@synara` scope and can publish `@synara/cli`.
2. In npm package settings, configure Trusted Publisher:
   - Provider: GitHub Actions
   - Repository: this repo
   - Workflow file: `.github/workflows/release.yml`
   - Environment (if used): match your npm trusted publishing config
3. Ensure npm account and org policies allow trusted publishing for the package.
4. Create release tag `vX.Y.Z` and push; workflow will:
   - set `apps/server/package.json` version to `X.Y.Z`
   - build web + server
   - run `bun publish --access public`

## Synara notes

- The desktop updater expects the pinned compatibility release in this repository to include the generated updater metadata files, not just the installers.
- The published release title should read `Synara vX.Y.Z`.
- By default, the first-party desktop release path does not require CLI publish or post-release version-bump automation.
- Optional jobs stay disabled unless repository variables enable them:
  - `SYNARA_PUBLISH_CLI=1`
  - `SYNARA_FINALIZE_RELEASE=1`
- `SYNARA_PUBLISH_CLI=1` applies only to ordinary releases. Protected Stage 6 Enterprise GA candidates always skip npm
  CLI publication because the CLI package is not part of the candidate Desktop artifact set or Final Review archive.

## 1) Build-only native CI validation

Use this before publication to validate the real native macOS, Linux, and Windows build matrix. Build-only mode does not create a tag, GitHub Release, npm package, updater manifest, or version-bump commit.

1. Push the release-candidate branch so GitHub Actions can check it out.
2. Start the workflow in build-only mode:
   - `gh workflow run release.yml --ref BRANCH -f version=X.Y.Z -f publish_release=false`
3. Wait for `.github/workflows/release.yml` to finish.
4. Confirm preflight and all four native matrix builds pass.
5. Download the workflow artifacts and sanity-check installation on each OS.
6. Verify every primary installer against the repository identity, for example
   `gh attestation verify PATH/TO/INSTALLER -R OWNER/REPOSITORY`.

For Stage 6 Enterprise GA, use the protected same-run publication mode below. A build-only run is useful rehearsal but
cannot later be promoted by rebuilding: signed/notarized bytes may change, so the rebuild is a new Desktop candidate.

To publish an ordinary release from a manual dispatch instead of a tag-push event, select the exact existing release tag
as the workflow ref and pass `publish_release=true`. Branch publication is reserved for the protected Enterprise GA mode
below, where the planned tag must still be absent.

### Stage 6 protected exact-artifact publication

Enterprise GA keeps the four signed native Artifact sets in one workflow run while external acceptance executes:

1. Create the `stage6-enterprise-ga` GitHub environment before dispatch. Configure two to six independent required
   reviewers, enable **Prevent self-review**, and disable **Allow administrators to bypass configured protection rules**.
   GitHub technically releases the waiting job after any one configured reviewer approves it; the copied Stage 6 checklist
   remains the authority for the separate Engineering, Operations, Security, Product, and applicable Privacy/Legal roles.
   After source-branch protection is active, prepare the API-supported Environment baseline:

   ```bash
   bun run stage6:environment:baseline -- \
     --repository owner/repo --branch release-branch --expected-head COMMIT \
     --reviewer User:ID --reviewer Team:ID
   ```

   Review its plan digest, then repeat the exact command with `--apply --confirm-sha256 SHA256`. The API path creates or
   verifies the Environment, reviewers, self-review prevention and protected-branch-only policy. GitHub exposes
   administrator bypass as read-only through its documented API, so the
   resulting receipt remains `environment-baseline-applied-manual-admin-bypass-required` until an authorized administrator
   disables that one setting in GitHub Settings and the same command verifies
   `environment-baseline-ready-not-release-approved`.

2. Prepare a clean protected release branch whose package versions already match the intended `vX.Y.Z` tag. The branch
   must enforce administrators, stale-review dismissal, at least one approval, strict required status checks, and deny
   force-push and deletion. The tag must not exist locally or on `origin`.
3. Dispatch from that branch with `publish_release=true` and `enterprise_ga_candidate=true`. Do not push the tag; the
   protected run reserves it and will create it only after approval.
4. Wait for preflight and all four native build jobs. The `stage6_enterprise_approval` job remains behind the protected
   `stage6-enterprise-ga` environment. The unprotected `finalize_release_assets` job first merges/copies updater metadata,
   writes and verifies `release-final-asset-set.json`, and uploads `finalized-release-assets`; download that artifact and the
   raw `desktop-*` artifacts from this still-running workflow.
5. Execute native installed acceptance and the remaining production/production-like controls. Generate the
   `stage-6-candidate-evidence-bundle-v3` receipt with `bun run stage6:candidate:prepare -- ...`, using the planned tag as
   candidate ID, this source commit, and this GitHub run ID. The preparer computes all input digests, validates all ten
   receipts before publishing, and refuses to overwrite prior evidence. Rebuilding or starting another run invalidates
   that association. Complete the review within 30 days; GitHub automatically fails a waiting job after that deadline.
6. Generate the protected configuration:

   ```bash
   bun run stage6:environment:prepare -- \
     --receipt ... --candidate-id ... --source-commit ... --build-run-id ... \
     --output /secure/.../protected-environment.json
   ```

   The command revalidates the exact receipt, derives the hashes/base64, writes a private non-overwriting configuration and
   sidecar, and prints no secret value. GitHub's documented Environment API cannot disable administrator bypass, so an
   authorized administrator must first disable that setting in the existing `stage6-enterprise-ga` Environment. Then apply
   the reviewed configuration:

   ```bash
   bun run stage6:environment:apply -- \
     --configuration /secure/.../protected-environment.json --repository owner/repo \
     --reviewer User:<id> --reviewer Team:<id> \
     --output /secure/.../environment-application.json
   ```

   The apply command first reads the exact Actions run and its source-branch protection, rejecting a different repository,
   workflow, commit, rerun, completed run or inadequately protected branch before any write. It also refuses any write while
   bypass remains enabled, requires two to six unique reviewers, enables prevent-self-review and protected-branch-only
   deployment, writes the candidate values with the secret supplied to `gh` only on stdin, and reads back the Environment,
   variables and secret presence. It emits a private non-overwriting application receipt, not an approval. The exact
   candidate-specific Environment variables are:
   `SYNARA_STAGE6_CANDIDATE_ID`, `SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT`,
   `SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID`, `SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256`, and
   `SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256`. Also set the protected environment secret
   `SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64` to the single-line base64 encoding of the exact v3 validation receipt.
   Keep both generated files outside source control and dispose of them under the private release-evidence retention policy.
   The receipt remains bounded so its single-line base64
   fits GitHub's documented [48 KiB secret limit](https://docs.github.com/en/actions/reference/security/secrets).

7. Approve the environment only after the copied GA checklist authorizes publication. Copy the exact structured review
   comment shown in the preflight job summary (`SYNARA_STAGE6_APPROVE <tag> <run-id>`); a generic approval comment or workflow
   rerun is rejected. The workflow verifies every declared value, decodes and semantically validates the actual receipt,
   independently rejects any empty or malformed projected control receipt and revalidates candidate Artifact/origin/Region/
   Migration identity plus Release Evidence, Desktop, Recovery and Residency boundaries,
   downloads and hashes the four same-run provenance/payload sets, proves that every non-YAML final public byte is identical
   to the approved raw candidate, and records `stage6-enterprise-ga-approval.json`. The publication job repeats the receipt,
   raw, final and raw-to-final checks before publishing the accepted payloads and tag. It performs no updater merge, copy,
   delete or other asset mutation after that verification.

The workflow independently reads GitHub's exact run, source-branch protection, current environment protection rules and
actual deployment review history. It fails unless the run is an active first-attempt `release.yml` workflow dispatch for
the exact repository/commit, its branch satisfies the protection policy, two to six reviewers are configured, self-review
prevention is enabled, administrator bypass is disabled, exactly one approval identifies this environment, and the actual
reviewer differs from both workflow actors. The v2 environment approval and v4 release approval retain the source branch,
normalized environment configuration, actual reviewer, actor identities and comment/configuration/review digests. GitHub
still releases the waiting job after one configured reviewer; the separate role-specific checklist remains mandatory. The
approval JSON does not claim GA by itself. The candidate receipt is retained in the private workflow artifact but is not
selected as a public release asset. Ordinary tag-triggered releases retain their existing path and do not enter Enterprise
GA mode.

Every published release includes `release-final-asset-set.json` and its sidecar. Platform provenance continues to prove the
native lane's signed installers, update archives and blockmaps; the final manifest separately covers the exact public
installers, blockmaps, updater YAML, provenance and attestation files after all deterministic feed preparation is complete.
Its updater-feed summary also verifies default/channel aliases are byte-identical and every manifest version, basename,
size, SHA-512, architecture and Windows blockmap reference matches the final local payload bytes.

The workflow-level `GITHUB_TOKEN` defaults to `contents: read`. Native build jobs receive only attestation/OIDC writes, and
only the GitHub Release publication job receives `contents: write`; the version-bump finalizer pushes through its separately
scoped Release App token. Every remote Action is pinned to a full commit SHA with its major version retained as a comment,
and every checkout except the explicit Release App push discards persisted credentials.

## 2) Apple signing + notarization setup (macOS)

Required secrets used by the workflow:

- `CSC_LINK`
- `CSC_KEY_PASSWORD`
- `APPLE_API_KEY`
- `APPLE_API_KEY_ID`
- `APPLE_API_ISSUER`
- `APPLE_TEAM_ID`

Checklist:

1. Apple Developer account access:
   - Team has rights to create Developer ID certificates.
2. Create `Developer ID Application` certificate.
3. Export certificate + private key as `.p12` from Keychain.
4. Base64-encode the `.p12` and store as `CSC_LINK`.
5. Store the `.p12` export password as `CSC_KEY_PASSWORD`.
6. In App Store Connect, create an API key (Team key).
7. Add API key values:
   - `APPLE_API_KEY`: contents of the downloaded `.p8`
   - `APPLE_API_KEY_ID`: Key ID
   - `APPLE_API_ISSUER`: Issuer ID
   - `APPLE_TEAM_ID`: Developer Team ID embedded in the signed application
8. Re-run a tag release and confirm macOS artifacts are signed/notarized.

Notes:

- `APPLE_API_KEY` is stored as raw key text in secrets.
- The workflow writes it to a temporary `AuthKey_<id>.p8` file at runtime.

## 3) Azure Trusted Signing setup (Windows)

Published Windows installers must be signed with Azure Trusted Signing. The
workflow fails closed when any required signing value is absent; unsigned
Windows artifacts are supported only for build-only validation runs. Signing
requires all of the following secrets:

- `AZURE_TENANT_ID`
- `AZURE_CLIENT_ID`
- `AZURE_CLIENT_SECRET`
- `AZURE_TRUSTED_SIGNING_ENDPOINT`
- `AZURE_TRUSTED_SIGNING_ACCOUNT_NAME`
- `AZURE_TRUSTED_SIGNING_CERTIFICATE_PROFILE_NAME`
- `AZURE_TRUSTED_SIGNING_PUBLISHER_NAME`
- `AZURE_TRUSTED_SIGNING_SUBJECT_DN`

Signing checklist:

1. Create Azure Trusted Signing account and certificate profile.
2. Record ATS values:
   - Endpoint
   - Account name
   - Certificate profile name
   - Publisher name
   - Full certificate subject distinguished name
3. Create/choose an Entra app registration (service principal).
4. Grant service principal permissions required by Trusted Signing.
5. Create a client secret for the service principal.
6. Add Azure secrets listed above in GitHub Actions secrets.
7. Re-run a build-only workflow and confirm the Windows installer is signed.

Before tagging a release, run a build-only workflow and verify the generated
installer's Authenticode identity matches both the configured publisher name and
full subject distinguished name.

## 4) Ongoing release checklist

1. Ensure `main` is green in CI.
2. Run the build-only native CI validation for the release-candidate branch and version.
3. Bump app version as needed.
4. Confirm `gh api repos/OWNER/REPO/releases/latest --jq .tag_name` returns the compatibility tag configured in `scripts/release-update-policy.json`.
5. Create release tag: `vX.Y.Z`.
6. Push tag.
7. Verify workflow steps:
   - preflight passes
   - all matrix builds pass
   - release job uploads expected files
8. Confirm the new versioned release is not GitHub Latest and the pinned compatibility release contains the new payloads plus all three `synara` manifests.
9. Smoke test downloaded artifacts.

## 5) Troubleshooting

- macOS build unsigned when expected signed:
  - Check all Apple secrets are populated and non-empty.
- Published Windows build rejected before packaging:
  - Check all eight Azure ATS, identity, and auth secrets are populated and non-empty.
- Build fails with signing error:
  - Re-check certificate/profile names and tenant/client credentials.

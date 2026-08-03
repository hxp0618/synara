# Stage 6 Desktop Enrollment macOS arm64 local acceptance

Date: 2026-07-31 (Asia/Shanghai)

Status: **PASS for a locally installed, Apple Development-signed macOS arm64 Desktop Enrollment
flow; NOT PASS for Developer ID distribution, notarization, stapling, macOS x64, Windows, Linux, or
candidate-environment GA.**

This report is immutable local acceptance evidence. Later runs must create a new report rather than
editing this file.

## Frozen source and host identity

- Branch: `codex/saas-tenancy-user`
- Embedded repository HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Worktree: dirty; the artifact was built from the current Stage 6 worktree, not from HEAD alone.
- Host: macOS 26.6 (`25G72`), arm64.
- Local build version: `0.6.3-stage6-desktop-local.6`.
- Bundle ID: `com.emanueledipietro.synara`.
- Registered URL scheme: `synara`.
- Diagnostic signing authority: `Apple Development: hxp0618@gmail.com (XS9SVAD396)`.
- Team ID: `9W6D3UJ48X`.
- Control Plane fixture: loopback HTTPS on `https://localhost:58443`, using a temporary mkcert
  certificate and deterministic fake credentials.

Source SHA-256 values used by the final run:

| Source | SHA-256 |
| --- | --- |
| `apps/desktop/src/main.ts` | `42e861087595e5704d04a52f1df78aed71cb7e54c432a9a6949d32ce31270500` |
| `apps/server/src/controlPlaneProxy.ts` | `95270cea07866cfae5a8abe68929c05b4a53a679f1b6e6e77efdf94d3120b80f` |
| `apps/server/src/controlPlaneProxy.test.ts` | `1e0c8ce53755b8f3f7c3c17d222115502938aab74d009673606553eb5cbd0db8` |
| `scripts/build-desktop-artifact.ts` | `00937bb27e3d31d8d403bd992c9812270f11ac932247ac90be5e808dbe6ade32` |
| `package.json` | `e062cedf7d2b0b4f9851b0dc37ed24332643d4d69be0fe271ee16015e94d741c` |
| `apps/server/package.json` | `d976792c6da38477670196cac31984a34d088d1a7ffd65feb374fa4aa6231a32` |
| `bun.lock` | `e727278f56997cffcce68b3cb584a6b56154c89a8fe7b056e9da48db59fc98c4` |

Local artifact identity:

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `Synara-0.6.3-stage6-desktop-local.6-arm64.dmg` | 227635366 | `a67d62eadf52a177d60075af2aa79e7eca4f7fe82d52395e820b353fafb071cf` |
| `Synara-0.6.3-stage6-desktop-local.6-arm64.zip` | 229419198 | `3a25570a59b27231964e0f67d2fb1db9dfbfc09906d73d8d9f7fdca36ddcf382` |
| `stage6-desktop-local-mac.yml` | — | `bb61fb791249784906a3a653356a311c7de6b86e7d783d214f0f572342e7ab08` |

## Failures found and closed by installed-app acceptance

The installed launch exposed defects that source-only and build-only checks did not exercise:

1. The staged server lacked the production runtime dependency `ioredis`. The server manifest now
   declares it, and the artifact builder imports `@effect/platform-node` from the frozen staged tree
   before packaging.
2. A nested registry `effect` beta could be installed beside the pinned platform packages and miss
   `dist/ServiceMap.js`. The root override now freezes the same exact Effect revision used by the
   workspace catalog.
3. The packaged renderer reached the loopback `/v1/*` Control Plane proxy from `synara://app`, but the
   proxy did not return trusted-origin CORS headers. The proxy now handles trusted preflight, adds CORS
   headers to success and error responses, preserves `Vary`, and rejects unrelated browser origins
   before forwarding a Desktop credential.
4. Backend restart after connect/disconnect left the renderer's previous Control Plane query cache in
   place. Desktop now waits for the replacement backend to become ready and reloads the existing
   renderer, so authority changes are visible without a manual reload.

## Final acceptance path

1. `bun run build:desktop` completed, including the shared Web renderer and Desktop main/preload
   bundles.
2. The artifact builder completed a frozen staged production install, the staged server import guard,
   macOS DMG/ZIP generation, update ZIP validation, and updater-manifest generation.
3. `verify-packaged-desktop-startup.ts` launched the packaged macOS arm64 payload from isolated state
   and reported `Packaged mac/arm64 startup smoke passed from isolated state.`
4. The extracted app was signed locally with the available Apple Development identity and the
   repository macOS entitlements. `codesign --verify --deep --strict` passed for the installed copy.
5. LaunchServices registered the installed application. Opening a real `synara://connect` URL reached
   the packaged app rather than passing the URL as a direct test argument.
6. A valid-shape link for `https://evil.example` was rejected with the redacted code
   `control_plane_not_allowed`; no connection file was created and no URL or Enrollment handle was
   written to the Desktop log.
7. A valid allowlisted link redeemed through the HTTPS fixture. The fixture verified the Ed25519
   device proof, then observed authenticated Session, Organization, Entitlement, and Platform Profile
   hydration. The renderer reloaded automatically and exposed the shared enterprise Tenant and
   Organization context.
8. The committed connection file was mode `0600` and 1080 bytes. It contained a 43-character public
   key, a 112-character protected private-key blob, and a 368-character protected credential blob.
   Exact scans for the fake Enrollment handle, initial credential, and rotated credential found no
   plaintext occurrence.
9. After a graceful packaged-app restart, the fixture verified the rotation proof. Hydration used
   `session-stage6-qa-2`, the protected credential remained ciphertext, and the file contained no fake
   plaintext credential or Enrollment handle.
10. The product Disconnect action was exercised through its native confirmation UI during the first
    final-package run. A second final-package run invoked the same preload IPC directly after rotation.
    Both reached `POST /v1/desktop/disconnect`, removed the protected connection file, restarted the
    backend, and reloaded the renderer automatically into local mode.
11. After disconnect, local `/health` returned 200, the local `state.sqlite` remained present at mode
    `0600` and 741376 bytes, and the ordinary local project/chat surface rendered. No local project or
    chat deletion was requested or observed.

## Security observations

- Renderer requests only received Control Plane proxy CORS access for the trusted `synara://app`
  origin. Focused tests reject `https://attacker.example` before proxy forwarding and retain
  non-browser local-tool access without adding CORS headers.
- The Desktop credential remained outside renderer-visible state and was injected only for the exact
  loopback backend `/v1` proxy boundary.
- Desktop diagnostic logs recorded lifecycle status and stable error codes only; they did not record
  the Deep Link, Enrollment handle, device private key, or session credential.
- The final run used an isolated `HOME`/`SYNARA_HOME`; only the temporary home exposed the real login
  Keychain directory so Electron `safeStorage` exercised macOS Keychain-backed protection.

## Cleanup

- The loopback fixture was stopped.
- The installed QA application was unregistered from LaunchServices and moved to macOS Trash; it is
  recoverable until Trash is emptied.
- The final connection was explicitly disconnected before shutdown, and its protected connection file
  was confirmed absent.
- Temporary acceptance home, fixture, build output, and staging directories were removed with exact,
  scoped targets after repository verification, reclaiming approximately 18 GiB.
- An early diagnostic launch used Electron's default user-data location and created normal launch
  metadata/static snapshots under `~/Library/Application Support/synara`. No pre-existing user data was
  deleted; those files were deliberately left untouched.
- No Git staging area, commit, remote branch, production database, production Control Plane, or real
  customer credential was changed.

## Scope limits and remaining release gates

- This is an Apple Development-signed local diagnostic app. Gatekeeper `spctl` rejected it and
  `stapler` confirmed that no notarization ticket was attached. It does not satisfy the release
  checklist's Developer ID signing/notarization requirement.
- The generated DMG/ZIP were unsigned local artifacts; only the extracted QA app was development
  signed for installed-host testing.
- Only macOS arm64 was exercised. macOS x64, Windows x64, and supported Linux installed link-open and
  credential-store acceptance remain open.
- The Control Plane was a deterministic loopback fixture with fake credentials, not a candidate SaaS
  deployment. Candidate-environment RBAC, external TLS/DNS, revocation propagation, SLO, recovery,
  capacity, penetration, compliance, and GA approvals remain separate gates.
- The macOS and remote-machine evidence in this report must not check either of the first two Desktop
  rows in `docs/release-checklists/stage-6-enterprise-ga.md`; those rows require a signed candidate
  matrix across every named platform and the remaining denial/rollback/replay cases.

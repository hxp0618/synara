# Stage 6 Desktop macOS arm64 local install and credential-store startup fix

Date: 2026-07-31 (Asia/Shanghai)

Status: **PASS for an Apple Development-signed native arm64 local installation and disconnected
startup without Keychain access; NOT PASS for Developer ID distribution, notarization, stapling, or
candidate-environment GA.**

This report is immutable local acceptance evidence. It supplements, and does not rewrite,
`stage-6-desktop-macos-arm64-local-acceptance-20260731.md`.

## User-visible issue and resolution

An x64/Rosetta QA launch on Apple Silicon produced the macOS warning that support for Intel-based apps
will end in a future macOS version. An earlier fully temporary `HOME` launch also produced the system
message that no Keychain could be found to store the Synara key.

The fixes are deliberately separate:

- Apple Silicon now uses a native arm64 artifact. The installed main executable is Mach-O arm64, so it
  does not invoke Rosetta or the Intel deprecation warning.
- Disconnected local startup no longer opens Electron `safeStorage`. The credential store is checked
  only after a saved Desktop SaaS connection document exists and must be decrypted. Enterprise
  enrollment still requires a supported OS credential store and fails closed; no plaintext fallback
  was introduced.
- Cross-built packages now install native optional dependencies for the artifact OS/CPU rather than
  the build host, and the macOS bundle validator checks normalized target architectures plus paired
  architecture-qualified dependency prebuilds.

## Frozen source, artifact, and installation identity

- Branch: `codex/saas-tenancy-user`.
- Embedded repository HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`.
- Worktree: dirty; the artifact was built from the current Stage 6 worktree.
- Host: macOS 26.6 (`25G72`), arm64.
- Local build: `0.6.3-stage6-desktop-local.10`.
- Installed path: `~/Applications/Synara.app`.
- Bundle ID: `com.emanueledipietro.synara`.
- Signing authority: `Apple Development: hxp0618@gmail.com (XS9SVAD396)`.
- Team ID: `9W6D3UJ48X`.

Source SHA-256 values:

| Source | SHA-256 |
| --- | --- |
| `apps/desktop/src/desktopCredentialStore.ts` | `0f79b11c854c123c64c8b0fcd2d8a043b9c863e520e82626d4795449748c586e` |
| `apps/desktop/src/desktopCredentialStore.test.ts` | `00345664b100186f70dd6bb1a31447c05a284dfc838dbb2a4a0766ac0015c9d7` |
| `scripts/build-desktop-artifact.ts` | `40c1897fb6042d2367cae7d832ff9b6c6e699d9f75e3629f8e63f4105f9ba131` |
| `scripts/lib/desktop-platform-build-config.ts` | `afeea7f516d9dfc134350be0bed0ee650c7676713c7badf27c01d80545da29ff` |
| `scripts/lib/mac-bundle-validation.ts` | `06d0f1b4cd1a7cade37f980f2909aa050fc7f27150189ec668bc06c3cff82735` |
| `scripts/lib/mac-bundle-validation.test.ts` | `7eaf9bbcd1e4ee306ac97d60303dfb1117ac0b3d68dc62d231058808f5686fc1` |
| `package.json` | `3771bec12952d2972cc598ecf1be8e3a5f7ed08e001ccaff481567a09c5a5cfc` |
| `bun.lock` | `e727278f56997cffcce68b3cb584a6b56154c89a8fe7b056e9da48db59fc98c4` |

Artifact identities:

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `Synara-0.6.3-stage6-desktop-local.10-arm64.dmg` | 225819553 | `06de4a2d1e775d68307d7d671dec23a87704949b2ece388cfea146e5917e3118` |
| `Synara-0.6.3-stage6-desktop-local.10-arm64.zip` | 228453396 | `36bbd540628c7882516e806fc67ba8377df28f4f9226609d2259a6d0ee2346c1` |

## Verification performed

1. Focused credential-store tests include a no-file adapter that throws if any `safeStorage` method
   is called. All 12 focused credential-store/Desktop SaaS tests passed.
2. The artifact builder completed the target-specific frozen production install, staged runtime
   import guard, arm64 AppSnap helper build, Apple Development signing, update-ZIP repack, Team-ID
   verification, and bundle architecture validation.
3. `verify-packaged-desktop-startup.ts` launched the signed arm64 DMG payload with a fully isolated
   temporary `HOME` and reported `Packaged mac/arm64 startup smoke passed from isolated state.` No
   real Keychain directory was exposed to that run.
4. The DMG was mounted read-only and `Synara.app` was installed to `~/Applications`. Deep strict
   `codesign` verification passed. The main executable was arm64, and 26 inspected Mach-O components
   satisfied the arm64 bundle policy.
5. LaunchServices registered the installed app and the actual installed path started successfully.
   The accessibility tree showed the ordinary local Synara project/chat surface at
   `synara://app/index.html`; no Keychain or Intel-compatibility dialog appeared.
6. Before and after launch, `~/.synara/userdata/desktop-saas-connection.json` was absent. The current
   Desktop log contained no `keychain`, `safe storage`, `credential store`, or
   `os_credential_store` entry. Existing local runtime data, including `state.sqlite`, remained in
   place.

## Scope limits

- This is an Apple Development-signed local installation. Gatekeeper rejected it and `stapler`
  confirmed that no notarization ticket was attached. It does not satisfy Developer ID release
  requirements.
- The no-connection path intentionally avoids Keychain. A real Desktop SaaS enrollment must still
  exercise Keychain-backed ciphertext, rotation, replay denial, and revocation against the signed
  candidate; the earlier arm64 report contains local fixture evidence for that behavior.
- macOS x64 native-Intel, Windows x64, supported Linux, candidate SaaS, and external operational,
  compliance, security, and GA approvals remain separate gates.

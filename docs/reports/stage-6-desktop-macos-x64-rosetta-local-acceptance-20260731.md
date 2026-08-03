# Stage 6 Desktop Enrollment macOS x64/Rosetta local acceptance

Date: 2026-07-31 (Asia/Shanghai)

Status: **PASS for local macOS x64 packaging and an Apple Development-signed installed flow under
Rosetta 2; NOT PASS for native Intel hardware, Developer ID distribution, notarization, stapling,
Windows, Linux, or candidate-environment GA.**

This report is immutable local acceptance evidence. Later runs must create a new report rather than
editing this file.

## Frozen source and host identity

- Branch: `codex/saas-tenancy-user`.
- Embedded repository HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`.
- Worktree: dirty; every artifact was built from the current Stage 6 worktree, not from HEAD alone.
- Host: macOS 26.6 (`25G72`), Apple Silicon arm64 with Rosetta 2 available
  (`arch -x86_64 uname -m` returned `x86_64`).
- Bundle ID: `com.emanueledipietro.synara`.
- Diagnostic signing authority: `Apple Development: hxp0618@gmail.com (XS9SVAD396)`.
- Team ID: `9W6D3UJ48X`.
- Full-flow fixture: loopback HTTPS on `https://localhost:58445`, with deterministic fake
  credentials and Ed25519 device-proof verification.

Final source SHA-256 values after the acceptance fixes:

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

Final artifact identities:

| Artifact | Signing boundary | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| `Synara-0.6.3-stage6-desktop-local.8-x64.dmg` | build-only | 236069111 | `0175c6f54981e01488058d6c2ef1095ff959d62a8ed2cd0619dc8443e51a9b4a` |
| `Synara-0.6.3-stage6-desktop-local.8-x64.zip` | build-only | 238044883 | `f356f8328a2e8d5fb6fd2a2a3c64324e3d38da762ab7a54e7e284f77d3d14a39` |
| `Synara-0.6.3-stage6-desktop-local.9-x64.dmg` | Apple Development | 238506062 | `396fd8cc6e6e3680e2e779f8b81ed711724309af5b8f600c2d1729dedf28bccf` |
| `Synara-0.6.3-stage6-desktop-local.9-x64.zip` | Apple Development | 241334715 | `2f49543f359949290aa72bf45c84adef6364678be704659fa324f94d71967c38` |

## Failures found and closed

1. The first generic x64 startup check used a completely temporary `HOME`. Electron
   `safeStorage.isEncryptionAvailable()` attempted to open a default login Keychain that did not
   exist there and blocked in macOS Security UI before the backend became ready. Disconnected local
   startup did not need a credential store at all. `DesktopCredentialStore.readConnection()` now
   reads the protected document first and returns `null` when no saved connection exists; it opens the
   OS credential store only when ciphertext must actually be decrypted. A focused test proves that
   the no-file path never calls `safeStorage`.
2. The macOS architecture validator compared the artifact name `x64` directly with the Mach-O slice
   name `x86_64`. It now normalizes that boundary and has positive and negative regression coverage.
3. A cross-build on arm64 installed optional native dependencies for the host rather than the x64
   artifact. The frozen staged install now passes explicit Bun target OS/CPU values. Paired
   architecture-qualified prebuilds inside packages such as `node-pty` remain allowed only when the
   required counterpart exists; core application and framework binaries must still match the target.

## Installed x64/Rosetta flow

1. Build `0.6.3-stage6-desktop-local.7` produced an x86_64 package tree. The extracted app was signed
   with the Apple Development identity, installed as `Synara Stage6 x64 QA.app`, registered with
   LaunchServices, and run under Rosetta 2.
2. A real `synara://connect` launch reached the installed application. Allowlist denial remained
   redacted and created no connection file.
3. The HTTPS fixture validated the Ed25519 device proof, then observed authenticated Session,
   Organization, Entitlement, and Platform Profile hydration. The shared Tenant/Organization UI
   appeared after the automatic renderer reload.
4. The protected connection file was mode `0600` and 1164 bytes. It contained a 43-character public
   key, a 112-character protected private-key blob, and a 412-character protected credential blob.
   Exact scans found no fake Enrollment handle, initial credential, or rotated credential in
   plaintext.
5. A graceful restart proved rotation into `session-stage6-qa-2`; the rotated credential remained
   ciphertext. Disconnect reached the fixture, removed the protected connection, preserved the local
   `state.sqlite`, restarted the backend, and returned the renderer to local mode automatically.
6. Build `0.6.3-stage6-desktop-local.8`, containing the disconnected-startup fix, passed
   `verify-packaged-desktop-startup.ts` from a fully isolated temporary `HOME`:
   `Packaged mac/x64 startup smoke passed from isolated state.`
7. Build `0.6.3-stage6-desktop-local.9` then completed target-specific dependency installation,
   Apple Development signing, update-ZIP repacking, deep signature/Team-ID validation, and architecture
   validation. Its main executable was `x86_64`; all 26 inspected Mach-O components satisfied the x64
   bundle policy. It was not launched again on the Apple Silicon workstation because macOS correctly
   warns that Intel applications will lose support in a future release; the signed installed runtime
   path was already covered by build 7 and the isolated startup regression by build 8.

## Scope limits and release consequences

- Rosetta execution is compatibility evidence, not native Intel-host evidence. A supported native
  Intel machine must still run the signed candidate link-open and credential-store matrix.
- The macOS Intel-support warning is expected for an x64 application on Apple Silicon. Apple Silicon
  users must receive the arm64 artifact; x64 is not the daily-install choice on that hardware.
- The final x64 artifact used Apple Development signing. `codesign --verify --deep --strict` passed,
  but Gatekeeper rejected it and `stapler` found no notarization ticket. It is not a distributable
  Developer ID candidate.
- Windows x64, supported Linux targets, real candidate SaaS infrastructure, and every external GA gate
  remain open. The first two Desktop rows in the Stage 6 release checklist therefore remain unchecked.

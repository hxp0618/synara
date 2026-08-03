# Stage 6 Desktop connection-mode local runtime acceptance — 2026-08-01

## Scope and assessment

This report records a local arm64 Electron runtime check of the explicit Desktop connection-mode choice. It proves only
the observed first-use and persisted-local startup behavior of the checked working tree. It is not a signed or notarized
release-candidate receipt, does not validate Cloud Panel connectivity, and does not approve Stage 6 GA.

Assessment: `local-runtime-connection-mode-validated-not-ga-approved`.

## Subject

- Host architecture: `arm64`
- Base commit: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Working tree: dirty; the source hashes below identify the exact locally built connection-mode implementation
- Application identity: `Synara Canary`
- Isolated state root: `/tmp/synara-stage6-first-run.darr3K` (removed after validation)
- Completed at: `2026-08-01T15:35:18Z`

## Source identity

| Source | SHA-256 |
| --- | --- |
| `apps/desktop/src/desktopConnectionMode.ts` | `8c4b62378951030ee3d948fb1f72f9b26c9bc5587199e186f36b74a41ce4d899` |
| `apps/desktop/src/main.ts` | `b50e9ea1c721a21bf7b71682049554c55c0935dbb83ef0f44a0053a65e4d1b7b` |
| `apps/web/src/components/settings/DesktopSettingsPanels.tsx` | `b6c2f4ba7a461224e5f404f8ecf09be848b93d1f48c44f7794a44c9f93006d19` |
| `apps/desktop/src/desktopConnectionMode.test.ts` | `05c9d990f7bef624c01bfb4c33649f4bc496098a5bb054e5486f6d093143c0db` |

## Procedure and observations

1. `bun run build:desktop` completed successfully for `@synara/desktop` and `@synara/cli`.
2. The application was launched with an empty isolated state root:
   `SYNARA_HOME=/tmp/synara-stage6-first-run.darr3K SYNARA_DESKTOP_FLAVOR=canary bun run start`.
3. Before any backend window appeared, the native dialog showed exactly these actions:
   `Use Local`, `Connect Cloud Panel`, and `Quit`. Its explanatory text stated that the mode can be switched later in
   Settings.
4. Selecting `Use Local` opened the normal application window and wrote
   `userdata/desktop-connection-mode.json` as mode `local` with permission `0600`.
5. No `desktop-saas-connection.json` or Desktop Cloud credential record was created in the isolated state root.
6. After terminating and relaunching with the same state root, the application opened its normal window without showing
   the first-use choice again. This confirms that a later launch restored the persisted Local choice.
7. `bun run test -- src/desktopConnectionMode.test.ts` passed all 6 focused tests.

## Deliberate limits and remaining candidate evidence

- The run used the repository's local Electron runtime, not a Developer ID signed and notarized DMG.
- The test selected Local mode; a real Cloud Panel Enrollment, rotation, disconnect and switch-back exercise remains a
  release-candidate requirement.
- Windows x64 and Linux x64 native UI/credential-store behavior remain outside this report.
- The working tree was dirty, so a future candidate must rerun the same behavior against immutable packaged artifacts and
  bind the result to the Stage 6 candidate evidence bundle.

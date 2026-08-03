# Stage 6 Desktop connection-mode installed local acceptance — 2026-08-02

## Scope and assessment

This report records a local installed-app runtime check of Synara Desktop's explicit Local versus Cloud Panel startup
choice. It binds the observation to the installed application payload and the matching local DMG/ZIP bytes. It does not
prove Developer ID distribution, notarization, Cloud Panel enrollment, native Intel behavior or Stage 6 GA approval.

Assessment: `installed-local-connection-mode-validated-not-ga-approved`.

## Subject

- Host: macOS on Apple Silicon (`arm64`)
- Installed application: `/Applications/Synara.app`
- Bundle identifier: `com.emanueledipietro.synara`
- Version: `0.6.3-stage6-desktop-local.12`
- Signing identity: `Apple Development: hxp0618@gmail.com (XS9SVAD396)`
- Team ID: `9W6D3UJ48X`
- Isolated Synara state root: `/tmp/synara-stage6-installed-mode.a3Jek9` (removed after validation)
- Runtime observation window: `2026-08-01T22:23:36Z` through `2026-08-01T22:26:37Z`

The `SYNARA_HOME` state root was isolated. Electron's stable browser-profile directory remained the installed app's normal
profile directory, so this report does not claim browser-profile isolation.

## Artifact identity

| Artifact | SHA-256 |
| --- | --- |
| Installed `Contents/Resources/app.asar` | `0c79f5cfed84bb035260ec098b960de2e76e15219b2fba5a1d1f38f857b92992` |
| DMG `release/Synara-0.6.3-stage6-desktop-local.12-arm64.dmg` | `c82507162ce7b165186e735e79a4d4f331e543e583318d0074459f694c8973dc` |
| ZIP `release/Synara-0.6.3-stage6-desktop-local.12-arm64.zip` | `aa58885a0a47b416aa8fc6cf4c51d2e74829c784722cd73f3dc28bf3448a0630` |

The DMG was mounted read-only. Its `Synara.app` reported the same version, passed deep/strict code-signature validation,
and its `app.asar` SHA-256 exactly matched the installed application.

## Architecture and trust observations

- Recursive bundle validation inspected 26 Mach-O files.
- Every inspected binary satisfied the `arm64` target and used Team ID `9W6D3UJ48X`.
- `codesign --verify --deep --strict` passed for the installed app and the read-only mounted DMG payload.
- Gatekeeper rejected the installed app because the origin is Apple Development.
- `stapler validate` reported that no notarization ticket is stapled.
- No `Developer ID Application` signing identity or configured `synara-notary` keychain profile was available on this
  host during the check.

This closes the local Intel-only-component regression for the observed arm64 payload, but it is deliberately not a
distribution trust result.

## First-use observation

1. The installed app was launched directly with an empty `SYNARA_HOME` state root.
2. Before the backend started, the native dialog displayed exactly these actions:
   `Use Local`, `Connect Cloud Panel`, and `Quit`.
3. The dialog explained that Local keeps projects and sessions on this Mac, Cloud Panel needs an administrator-provided
   short-lived link, and the mode can be changed later in Settings.
4. While the dialog remained unresolved, `desktop-main.log` stopped at `bootstrap start`; no backend-start record existed.
5. Selecting `Use Local` wrote `userdata/desktop-connection-mode.json` with mode `local` and permission `0600`.
6. Only after that write did the log record endpoint reservation, IPC registration, backend start, window creation and
   backend readiness.
7. No `userdata/desktop-saas-connection.json` was created, and the runtime logs contained no Keychain or `safeStorage`
   message.

## Persisted restart observation

1. The app completed its normal shutdown and was relaunched with the same isolated Synara state root.
2. The normal Synara window opened directly; the first-use dialog did not return.
3. The second run logged `Desktop connection mode selected mode=local` before starting the backend.
4. The Cloud connection file remained absent and no Keychain or `safeStorage` message appeared.

## Deliberate limits and remaining candidate evidence

- The package is Apple Development signed, Gatekeeper-rejected and not notarized; it is not a distributable candidate.
- No real Cloud Panel Enrollment, credential rotation, Cloud-to-Local switch, Local-to-Cloud switch or disconnect was
  performed in this run.
- The absence of a Cloud state file and Cloud/Keychain log entries proves that the observed startup path did not initialize
  persisted Cloud credentials. This run did not capture packets and therefore does not claim a general zero-network proof.
- A Developer ID signed, notarized and stapled arm64 candidate must repeat this flow, including upgrade continuity for an
  existing Keychain item.
- Native Intel macOS, Windows x64 and Linux x64 still require their own signed-candidate runtime evidence.

# Desktop installed acceptance v1

## Promise boundary

Stage 6 Desktop Enrollment is supported only when the exact published installer runs natively on its declared platform,
the operating system launches `synara://`, credentials remain inside the approved OS protection boundary, and the full
single-use Enrollment/session lifecycle succeeds against the exact candidate Control Plane. Build success, unpackaged
Electron, Rosetta, a development signature or unit tests do not satisfy this contract.

The required targets are:

| Target        | Package  | Native host         | Credential boundary                 | Release trust                                       |
| ------------- | -------- | ------------------- | ----------------------------------- | --------------------------------------------------- |
| `macos-arm64` | DMG      | Apple Silicon arm64 | macOS Keychain                      | Developer ID, Gatekeeper, notarization and stapling |
| `macos-x64`   | DMG      | Intel x64           | macOS Keychain                      | Developer ID, Gatekeeper, notarization and stapling |
| `windows-x64` | NSIS     | Windows x64         | DPAPI/Windows credential protection | Valid timestamped Authenticode                      |
| `linux-x64`   | AppImage | Linux x64           | Secret Service/libsecret            | immutable CI provenance and artifact attestation    |

Apple Silicon running an x64 app under Rosetta is explicitly not `macos-x64` evidence. Wine, QEMU and other translation
layers are likewise excluded. The installed app path must come from the recorded installer, not a copied unpacked build.

## Candidate and artifact identity

One exercise fixes candidate ID, exact source commit/tag, `bun.lock` SHA-256, version, environment class and deployment-
unique environment ID, Control Plane HTTPS
base URL and Migration tail. Each target supplies the primary installer digest and the unmodified
`artifact-<platform>-<arch>.provenance.json` produced by `scripts/write-release-artifact-provenance.ts`.
The same native matrix job must also retain the signed `actions/attest@v4` bundle and its successful
`gh attestation verify <installer> -R <owner/repository> --format json` result for the primary installer.

The provenance must be a publication manifest whose platform, architecture, target, version, commit, tag and lockfile
match the candidate. macOS provenance must contain verified `apple-developer-id` identity plus `codesign`, Gatekeeper and
stapler checks for app and DMG. The extracted update ZIP and read-only-mounted final DMG must each contain exactly one real
top-level `Synara.app`; every nested Mach-O must satisfy the candidate architecture and expected Apple Team ID. Windows must
contain verified Authenticode signer and timestamp identities. Linux native
package signing is `not-applicable`; the separate CI provenance/attestation evidence therefore becomes mandatory rather
than pretending `scheme=none` is a signature.

## Installed lifecycle

Every target records unique hashed evidence for provenance, artifact attestation, installation, protocol, credential
store, Enrollment and secret scan. The installed exercise proves all of the following:

1. host architecture equals artifact architecture and execution mode is `native`;
2. no architecture/compatibility warning appears;
3. the OS registers `synara://`, the native launcher dispatches to the installed application, and a disallowed Control
   Plane origin causes no network request;
4. the exact required credential backend is available, ciphertext is protected at rest, plaintext scanning passes and no
   missing-store/Keychain prompt appears;
5. a clean first launch requires an explicit local/Cloud choice before backend or Cloud traffic; local and Cloud mode each
   restore after restart, both switch directions pass, local mode emits no Cloud traffic, and a local switch retains the
   protected Cloud credential until explicit disconnect;
6. Ed25519 device proof, complete Session/Tenant/Organization/Entitlement hydration, restart persistence, credential
   rotation, replay denial, remote revocation, local disconnect and local-state preservation all pass; and
7. the candidate and collected evidence contain no Enrollment handle, credential, device private key, prompt or customer
   payload.

A failed or blocked target remains in the receipt. Assertions cannot be omitted, and one evidence file cannot be reused for
multiple controls. The final bundle is prepared from a path-only
`synara.stage6-desktop-native-acceptance-draft.v1`: callers do not supply evidence digests. The preparer derives all 35
references from stable bounded regular non-symlink reads, rejects duplicate JSON fields and prohibited secret material,
then validates the exact materialized manifest before atomically publishing private manifest/receipt pairs and sidecars.
The validator hashes and parses each structured attachment from the same captured bytes and enforces an aggregate evidence
bound, so a renamed file, symlink, mid-read mutation, post-hash JSON swap or hand-authored digest cannot become passing
evidence.

`evidence.enrollment` is not an opaque screenshot or prose attachment. It must be UTF-8 JSON with schema
`synara.stage6-desktop-enrollment-runtime-evidence.v1` and bind the candidate ID, source commit, environment ID, normalized
Control Plane URL, target, runner, host architecture and execution mode. Its start/completion and every observation must
remain inside the enclosing acceptance window. The runtime evidence records these six transitions in order, each with a
strictly increasing UTC observation time and pass/fail result:

1. `unselected → local` by first-use choice;
2. `local → local` by restart restore;
3. `local → cloud` by Settings switch;
4. `cloud → cloud` by restart restore;
5. `cloud → local` by Settings switch; and
6. `local → cloud` by Settings resume.

It also records non-negative backend/Cloud request counts before the first choice, Cloud request count while Local, and
SHA-256 fingerprints of the protected Cloud credential before the Local switch and after Cloud resume. The validator
derives the seven connection-mode assertions from those observations and requires all 15 Enrollment results to exactly
match the outer manifest. A valid hashed file with a different candidate, target, time window, transition order or result
therefore fails validation instead of being accepted as generic evidence. Fingerprints are compared but not projected into
the validation receipt.

Native test harnesses hand off `synara.stage6-desktop-enrollment-runtime-capture.v1`, which adds the enclosing
`exerciseWindow` to the same candidate, target, transition, request-count, credential-continuity and result facts. The
capture itself is not release evidence. `stage6:desktop:enrollment:prepare` reads it without following symlinks, normalizes
the credential-free Control Plane URL, executes the same semantic validator, removes the enclosing capture-only field and
publishes the canonical runtime evidence plus SHA-256 sidecar as exclusive `0600` files. Existing outputs are never
overwritten, and a validation or sidecar-publication failure leaves no new evidence file.

## Approval and non-claims

Engineering, Security and Release use distinct pseudonymous approver references and approve only after all four target
runs finish. `scripts/stage6-desktop/prepare_desktop_native_acceptance.py` materializes and validates the protected bundle;
`scripts/stage6-desktop/validate_desktop_native_acceptance.py` remains available for an already materialized manifest. The receipt emits
`eligibleForHumanGateReview`, but its assessment is permanently `evidence-validated-not-desktop-ga-passed`.

Local Apple Development evidence in the 2026-07-31 reports proves the implementation fixes for native arm64 startup and
Keychain deferral. It does not satisfy Developer ID/notarization, native Intel, Windows, Linux or candidate-environment
requirements and cannot be imported as a passing v1 bundle.

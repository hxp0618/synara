# Desktop native release acceptance

Use this runbook after the exact candidate passes `.github/workflows/release.yml` and before advancing a Desktop update
channel or checking the Stage 6 GA Desktop rows.

For an Enterprise GA candidate, start the workflow's protected same-run publication mode before testing. Use artifacts from
that run while `stage6_enterprise_approval` waits; record its GitHub run ID in the release evidence. Do not rebuild or push
the planned tag manually after acceptance.

## Prepare the candidate

1. Freeze release ID, source commit/tag, `bun.lock` SHA-256, environment class/ID, Migration tail and candidate Control
   Plane HTTPS URL.
2. Download all four workflow artifact sets without renaming files. Retain each
   `artifact-<platform>-<arch>.provenance.json` and the CI artifact-attestation verification output.
   Verify the primary installer with `gh attestation verify PATH/TO/INSTALLER -R OWNER/REPOSITORY` and retain the bounded
   verification result together with the workflow's `.attestation.json` bundle.
3. Confirm the macOS provenance says `verified/apple-developer-id`; Apple Development is not accepted. Confirm both the
   extracted update ZIP app and the read-only-mounted DMG app passed deep signing, requested-architecture and nested Team ID
   checks, and that both app and DMG passed Gatekeeper and stapler validation.
4. Confirm Windows provenance contains `verified/windows-authenticode`, the expected publisher/subject and timestamp
   identity. A version-scoped unsigned exception is not a Stage 6 GA candidate.
5. Verify Linux workflow provenance and artifact attestation before moving the AppImage to the test host.

Never copy signing credentials, Enrollment handles, Desktop credentials or customer data into the evidence directory.

Before a signed macOS build, run `bun run stage6:desktop:mac:preflight` in the same environment as the build. The
preflight requires the Developer ID credential source, password, Apple API key path/ID/issuer and Team ID; it rejects a
missing or malformed input, a symlinked key, a key owned by another user, or any notary private key readable by group or
other users. Its JSON output contains structural booleans only and never prints credential values. Passing this preflight
does not authenticate the certificate inside `CSC_LINK` and does not replace the post-build Developer ID, Gatekeeper,
notarization, stapling, architecture and nested Team ID checks.

## Execute each native host

Use a fresh machine/user profile and a fresh short-lived Enrollment for every attempt:

- macOS: install the DMG, register LaunchServices, then run `open 'synara://connect?...'`;
- Windows: install NSIS, then run `Start-Process 'synara://connect?...'`;
- Linux: install/register the AppImage desktop entry, then run `xdg-open 'synara://connect?...'`.

Record the real host architecture and prove `native` execution. Native Intel hardware is required for `macos-x64`; do not
record Rosetta as a pass. For every target:

1. launch with a clean profile and prove no backend/Cloud request occurs before the local/Cloud chooser is answered;
2. choose local, restart, and prove local restoration plus zero Cloud proxy traffic;
3. switch to Cloud, launch one disallowed-origin link and prove zero network requests, then redeem a fresh allowed link;
4. verify device proof plus complete authenticated hydration, restart and prove Cloud restoration;
5. switch Cloud → local → Cloud, prove both backend transitions and prove the protected credential survives the local switch;
6. inspect only bounded metadata to prove Keychain, DPAPI or Secret Service protection and plaintext-scan absence;
7. rotate, replay the previous credential and require denial;
8. revoke remotely, reconnect with a fresh Enrollment, then explicitly disconnect locally;
9. prove the Cloud Panel credential document is removed while local SQLite/project state remains; and
10. record that no missing credential-store or architecture compatibility warning appeared.

Write the Enrollment machine record as `synara.stage6-desktop-enrollment-runtime-evidence.v1`, not as free-form prose.
Capture the six canonical mode transitions in the order defined by the contract, with strictly increasing UTC timestamps.
Record backend/Cloud request counts before the first choice and Cloud request count while Local. Hash a stable,
non-secret credential identity before switching Cloud → Local and after resuming Local → Cloud; never copy credential
plaintext into the record. Bind the record to the exact candidate, environment, normalized Control Plane URL, target,
runner, host architecture and execution mode. Set the outer manifest's Enrollment booleans from this record rather than
editing them independently.

Have the native harness write `synara.stage6-desktop-enrollment-runtime-capture.v1` with the enclosing exercise window,
then create the immutable evidence file through the preparer rather than hand-editing the final schema:

```bash
bun run stage6:desktop:enrollment:prepare -- \
  --capture /secure/desktop-acceptance/<target>-enrollment-capture.json \
  --output /secure/desktop-acceptance/evidence/<target>-enrollment.json
```

Retain both the evidence file and its `.sha256` sidecar. A failed command must leave both absent; an existing path means
the attempt already has an immutable identity and must not be overwritten.

Screenshots and logs must be redacted before hashing. Keep positive evidence for each subsystem separate so a single broad
log cannot be reused as proof of every control.

## Validate and approve

Create the path-only draft defined by `desktop-installed-acceptance-v1.md`, keep it and every evidence file below one
protected root, then run:

```bash
bun run stage6:desktop:native:prepare -- \
  --evidence-root /secure/desktop-acceptance \
  --draft desktop-native-draft.json \
  --manifest-output desktop-native-manifest.json \
  --receipt-output desktop-native-receipt.json
```

Do not calculate or paste evidence hashes into the draft. The preparer stable-reads and scans every attachment, derives the
hashes, validates the complete four-platform graph, and publishes the manifest and receipt as immutable `0600` pairs with
sidecars. Use a fresh attempt path for every rerun. `bun run stage6:desktop:native:validate -- --manifest ...` is reserved
for checking an already materialized manifest; it does not make unsigned or non-native artifacts eligible.

Failed, blocked, translated, unsigned, unnotarized or incomplete runs are retained with
`eligibleForHumanGateReview=false`; do not delete them and retry until only the successful attempt remains. A true review
flag still requires distinct Engineering, Security and Release review, the release evidence manifest, and the copied Stage
6 GA checklist. The candidate-bundle validator derives `candidate.desktopArtifactSetSha256` from the four provenance
references; copy that exact value into `SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256` on the protected GitHub environment.
After the environment reviewer releases the job, the workflow recomputes it from the same-run payloads before writing its
publication-authorization record and again before publication. Publish or advance the update channel only after those
authorities approve the same immutable digests.

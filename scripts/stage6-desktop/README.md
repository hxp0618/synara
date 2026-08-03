# Stage 6 Desktop native acceptance validator

The validator binds one production or production-like candidate and deployment-unique environment ID to all four release targets: macOS arm64, native macOS
x64, Windows x64 and Linux x64. It parses each workflow-generated `artifact-<platform>-<arch>.provenance.json`, verifies
the candidate source/tag/lockfile and primary installer digest, and then validates native installation, protocol dispatch,
OS credential protection, Desktop Enrollment lifecycle, secret scan, artifact attestation and three-party approval evidence.

The Enrollment evidence file is parsed as
`synara.stage6-desktop-enrollment-runtime-evidence.v1`; it cannot be arbitrary prose. It binds the exact candidate, runner
and native target, stays inside the enclosing exercise window, records the canonical first-choice/restore/switch sequence,
and supplies request counts plus protected-credential fingerprints from which the connection-mode assertions are derived.
The derived results must exactly match the outer target assertions.

Use the path-only draft preparer for the protected bundle. The draft has schema
`synara.stage6-desktop-native-acceptance-draft.v1` and the same shape as the final manifest, except every target and
approval evidence reference is a POSIX path relative to the evidence root rather than a caller-supplied `{path, sha256}`
object:

```bash
bun run stage6:desktop:native:prepare -- \
  --evidence-root /secure/desktop-acceptance \
  --draft desktop-native-draft.json \
  --manifest-output desktop-native-manifest.json \
  --receipt-output desktop-native-receipt.json
```

The preparer derives all 35 hashes from stable bounded non-symlink reads, scans every input for prohibited secret
material, validates the complete candidate graph, and publishes exclusive `0600` manifest/receipt pairs with SHA-256
sidecars. It rolls the manifest pair back if receipt publication fails. For validation of an already materialized manifest,
run `bun run stage6:desktop:native:validate -- ...`; the validator uses the same bytes for each evidence hash and structured
JSON parse, rejects duplicate JSON fields, and caps both individual and aggregate evidence sizes.

Rosetta, Wine, QEMU, unsigned build-only packages, Apple Development signing, missing notarization/stapling, unsigned
Windows exceptions, wrong Linux Secret Service, missing Keychain/credential-store prompts, failed replay denial or any
failed lifecycle assertion remain valid failed evidence but cannot become review-eligible. Linux uses the workflow
provenance plus a separately hashed CI artifact-attestation result; `not-applicable` native package signing alone is not the
GA trust claim.

Prepare each native harness capture before building the four-target manifest:

```bash
bun run stage6:desktop:enrollment:prepare -- \
  --capture /secure/desktop-acceptance/macos-arm64-enrollment-capture.json \
  --output /secure/desktop-acceptance/evidence/macos-arm64-enrollment.json
```

The capture schema is `synara.stage6-desktop-enrollment-runtime-capture.v1`. The preparer reads a bounded regular
non-symlink file, normalizes its candidate URL, validates its exercise window and all runtime observations, then publishes
exclusive `0600` evidence and SHA-256 sidecar files. It never overwrites an earlier attempt or prints credential
fingerprints. The resulting evidence path/hash becomes that target's `evidence.enrollment` reference.

The downstream candidate-bundle validator combines every target's `evidence.provenance.sha256`,
`evidence.attestationBundle.sha256` and `evidence.artifactAttestation.sha256` into one stable Desktop artifact-set digest.
This binds every payload named by those provenance manifests and its workflow trust evidence, not only the four primary
installer digests.

The receipt always states `evidence-validated-not-desktop-ga-passed`. Even
`eligibleForHumanGateReview=true` still requires Engineering, Security and Release approval in the copied GA checklist.
The four-platform manifest, receipt and SHA-256 sidecars are exclusive `0600` outputs from the shared Stage 6 immutable
publisher. A rerun must use a new attempt path; collision or sidecar failure cannot replace or half-publish evidence.

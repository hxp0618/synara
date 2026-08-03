# Production secret, certificate, domain and key rotation acceptance v1

> Historical pre-internal-self-hosted schema. It used `billing-provider-credential`. New exercises use
> [production rotation acceptance v2](production-rotation-acceptance-v2.md), which replaces that retired control with
> versioned internal incident Publisher HMAC rotation.

## Authority and boundary

This contract turns the Stage 6 production-rotation checklist item into candidate-bound, fail-closed evidence. It covers
the exercise shape and immutable evidence bytes; it does not authenticate the deployment, provider audit system,
operators, approvers or signatures. A structurally eligible receipt therefore remains
`evidence-validated-not-production-rotation-passed` until the real Security, Operations and Release authorities accept it.

Never store secret values, private keys, bearer tokens, credential-bearing URLs, database DSNs or customer payloads in
the manifest or evidence files. Use opaque secret-manager versions, certificate fingerprints, DNS change IDs, provider
audit IDs and irreversible digests. The validator scans the manifest and every referenced file for high-confidence secret
patterns and fails without echoing the matched value.

## Candidate identity

The manifest uses schema `synara.stage6-production-rotation-evidence.v1` and binds:

- candidate ID and exact 40-character source commit;
- Control Plane, Web and Platform Admin SHA-256 digests;
- a unique `production` or `production-like` environment ID;
- a sorted, non-empty Region set; and
- the exact current Migration tail name and SHA-256 from the candidate source.

A redeployment, different Artifact, changed Region set or Migration drift requires a new manifest and exercise.

## Required controls

The manifest contains each control exactly once:

1. `credential-envelope-kek`;
2. `runtime-secret-key`;
3. `worker-registration-token`;
4. `postgresql-credential`;
5. `artifact-storage-credential`;
6. `billing-provider-credential`;
7. `telemetry-relay-identity`;
8. `public-tls-certificate`; and
9. `public-domain-dns`.

Each control records a bounded change reference, distinct owner and approver subjects, different previous/new authority
IDs, a time window inside the overall exercise, `passed | failed | not-assessable`, unresolved-risk count and four derived
facts:

- every declared consumer received the intended configuration;
- the new client path passed;
- the old credential/key/certificate/origin reached its documented final denied, retired or decommissioned state; and
- rollback was exercised or safely verified inside the declared rollback window.

Each control references five unique immutable files: change record, rollout status, new-path probe, old-path final-state
proof and rollback result. A `passed` label alone is insufficient; every derived fact must be true and unresolved risks must
be zero before the receipt becomes eligible for human review.

## Secret scan and approvals

The manifest additionally binds a secret-scanner version, zero finding count and its evidence file. Security, Operations
and Release then provide three distinct subject decisions and evidence files after the exercise completes. The only
eligible decision is `approved-for-human-gate-review`; `reviewed-not-approved` preserves a valid non-pass result.

Evidence paths are relative to one non-symlink evidence root. They must be unique, regular, bounded files whose declared
SHA-256 matches a stable read. The validator publishes a private `0600` receipt and SHA-256 sidecar exactly once; existing
outputs are never overwritten.

## Fail-closed preparation

The preferred preparer accepts a `synara.stage6-production-rotation-evidence-draft.v1` document with the same semantic
fields, but every evidence reference is only a relative path string. It performs bounded stable reads and computes all 49
digests rather than trusting operator-transcribed hashes. The complete manifest is validated in a private temporary file;
only then are the manifest, receipt and two sidecars published. Invalid input creates no output, while failure publishing
the receipt rolls back the manifest pair.

```bash
bun run stage6:rotation:prepare -- \
  --evidence-root /secure/stage6-rc1 \
  --draft production-rotation-draft.json \
  --repository-root . \
  --manifest-output production-rotation-manifest.json \
  --receipt-output production-rotation-receipt.json
```

## Offline revalidation

```bash
python3 scripts/stage6-rotation/validate_production_rotation_evidence.py \
  --manifest /secure/stage6-rc1/production-rotation-manifest.json \
  --evidence-root /secure/stage6-rc1 \
  --repository-root . \
  --output /secure/stage6-rc1/production-rotation-revalidation-receipt.json
```

`eligibleForHumanGateReview=true` proves only that all nine controls, the zero-finding scan and three decisions form one
internally consistent candidate-bound evidence set. The receipt explicitly keeps external evidence authority, approver
authority and cryptographic signatures unverified. Attach the exact receipt and sidecar to the copied GA checklist; do not
add it to Candidate v5 or use it to bypass the separate protected release and Final Review decisions.

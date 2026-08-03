# Stage 6 third-party penetration evidence validator

The preparer and validator pin the Stage 5 dependency, independent assessor declaration, four candidate artifacts, six
required attack areas, finding disposition and nine evidence files into a deterministic receipt. They preserve incomplete
and failed tests instead of turning them into a pass.

Use a path-only `synara.third-party-penetration-evidence-draft.v1` whose nine `evidence` values are relative paths below one
access-controlled evidence root. The preparer performs stable bounded reads, rejects duplicate JSON fields, symlinks,
traversal and common credential material, derives exact hashes, validates the materialized manifest, and atomically
publishes immutable `0600` manifest/receipt pairs and SHA-256 sidecars:

```bash
bun run stage6:penetration:prepare -- \
  --evidence-root /secure/engagement \
  --draft penetration-draft.json \
  --manifest-output penetration-manifest.json \
  --receipt-output penetration-receipt.json

python3 -m unittest discover -s scripts/stage6-penetration -p 'test_*.py'
```

Sensitive reports belong in an access-controlled evidence store, not this repository. The receipt always states
`evidence-validated-not-penetration-passed`; `eligibleForHumanGateReview` is a machine-readiness signal, not a security or
release approval.
`stage6:penetration:validate` remains available for independent revalidation of an already materialized manifest. Existing
paths are rejected, and receipt publication failure rolls back the new manifest pair. Never overwrite a prior
assessor-bound attempt.

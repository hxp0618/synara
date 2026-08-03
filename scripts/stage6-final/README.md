# Stage 6 final GA review archive validator

This validator runs after the internal-self-hosted Candidate v5 bundle, protected same-run release approval and Platform Release Governance
review exist. It binds the completed candidate checklist, customer change notice, protected approval, canonical final asset
set, Platform Audit export and every functional decision to one candidate. It never asserts that Synara authenticated an
external assessor, corporate approver, publication channel or evidence repository.

The preferred preparer accepts a `synara.stage6-final-ga-review-draft.v1` path-only draft. Candidate-level references are
relative path strings; each control has a list of evidence paths and each final approval has one evidence path. The
preparer computes every digest, materializes the final v1 manifest in a private temporary file, runs the complete validator,
and only then publishes the manifest, receipt and both sidecars:

```bash
bun run stage6:final:prepare -- \
  --evidence-root /secure/stage6-rc1 \
  --draft final-ga-review-draft.json \
  --manifest-output final-ga-review.json \
  --receipt-output final-ga-review-receipt.json
```

All final manifest references are traversal-free relative paths below one non-symlink evidence root and include the exact
`sha256:` digest. The manifest must contain exactly the 32 control IDs defined by the v1 contract. Failed or blocked
controls and denied final approvals remain valid historical evidence, but make
`eligibleForExternalGAAuthorityReview=false`. Personal-data, Provider-terms, regulated-customer, Residency or
security-incident impact requires a fifth, distinct Privacy/Legal final approver.

The lower-level validator remains available for offline revalidation to a new output path:

```bash
bun run stage6:final:validate -- \
  --manifest /secure/stage6-rc1/final-ga-review.json \
  --evidence-root /secure/stage6-rc1 \
  --output /secure/stage6-rc1/final-ga-review-revalidation-receipt.json

python3 -m unittest discover -s scripts/stage6-final -p 'test_*.py'
```

All preparer and validator outputs and SHA-256 sidecars are exclusive `0600` files. Existing output is never replaced; a
failed validation publishes nothing and a second-output failure rolls the first pair back. The permanent assessment is
`final-review-consistent-not-ga-authority-verified`; even an eligible receipt must still be accepted by the real external
GA authority and archived in the access-controlled evidence repository.

# Stage 6 internal usage and cost evidence

This validator binds one release candidate to a bounded reporting period, reconciled Token totals, complete Provider-cost
coverage, actual-over-estimated platform allocation, currency-safe known-cost totals and four immutable source files.
It rejects payment, Stripe, Checkout, invoice, tax, card and settlement semantics.

For candidate evidence, put one path-only draft and the four source exports below an access-controlled bundle root. The
draft uses `synara.stage6-internal-cost-evidence-draft.v1`; each evidence row contains only its required `id` and a
relative `path`. Do not calculate or paste hashes manually. Materialize, validate and atomically publish with:

```bash
bun run stage6:cost:prepare -- \
  --evidence-root /secure/internal-cost \
  --draft internal-cost-draft.json \
  --manifest-output internal-cost-manifest.json \
  --receipt-output internal-cost-receipt.json
```

The preparer reads stable bounded non-symlink inputs, rejects duplicate JSON fields and obvious credential material,
materializes SHA-256 references, validates Token/cost coverage and arithmetic, and publishes the manifest/receipt plus
sidecars as exclusive `0600` files. Validation failure publishes nothing; receipt publication failure removes the
manifest pair. It never infers reporting completeness, source authority or reviewer approval.

The lower-level validator remains available for an already materialized manifest:

```bash
bun run stage6:cost:validate -- \
  --manifest /secure/internal-cost/internal-cost-manifest.json \
  --evidence-root /secure/internal-cost \
  --output /secure/internal-cost/internal-cost-receipt.json
```

The strongest assessment is `evidence-validated-not-internal-cost-approved`. The receipt still requires independent
Operations and cost-owner review inside Release Governance.

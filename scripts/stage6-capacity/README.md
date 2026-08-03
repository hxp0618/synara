# Stage 6 capacity and long-duration evidence

Use the preparer for a real run. Its draft contains paths only for the seven evidence fields, all relative to one evidence
root. The preparer stable-reads each bounded regular file, rejects symlinks, traversal, duplicate JSON fields and common
credential material, derives hashes, validates the materialized manifest, then exclusively publishes manifest/receipt pairs
and their SHA-256 sidecars as `0600` files.

```bash
bun run stage6:capacity:prepare -- \
  --evidence-root /secure/evidence/capacity-attempt-001 \
  --draft capacity-draft.json \
  --manifest-output capacity-manifest.json \
  --receipt-output capacity-receipt.json
```

Use draft schema `synara.capacity-soak-evidence-draft.v1`; it otherwise has the manifest's exact fields, with every
`evidence.<field>` value replaced by a POSIX relative path string. Output paths are also relative to the evidence root and
must be new. Receipt publication failure removes the newly published manifest pair, so a bundle cannot appear complete
with only half its outputs.

The validator remains available for an already materialized manifest. It checks the exact v1 shape,
forecast-plus-headroom load, required phases, duration, external-probe sample coverage, SLO volumes/ratios,
saturation/fairness declarations, bounded aggregate evidence, and hashes for seven distinct evidence files:

```bash
bun run stage6:capacity:validate -- \
  --manifest /secure/evidence/capacity-attempt-001/capacity-manifest.json \
  --evidence-root /secure/evidence/capacity-attempt-001 \
  --output /secure/evidence/capacity-attempt-001/validation-receipt.json
```

Minimum continuous durations are 72 hours for production, 24 hours for production-like, 8 hours for staging, and five
minutes for fixtures. Fixture/staging receipts are never release-eligible. The output assessment is always
`evidence-validated-not-capacity-passed`: validated declarations and hashes still require Operations/Engineering review and
the copied Stage 6 release checklist.

Do not put credentials, prompts, Provider payloads, Presigned URLs, database DSNs or Tenant identifiers in evidence. Raw
Prometheus range results must use the bounded aggregate labels permitted by the observability contract.
Existing attempt paths are never overwritten. These tools verify bytes and declared semantics only; they do not authenticate
the environment, telemetry source, operators, duration, execution, reviewer authority or signatures.

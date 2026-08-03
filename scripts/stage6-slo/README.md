# Stage 6 SLO evidence validator

The preparer and validator recompute one Stage 6 SLO window from declared ratios, sample counts and error budgets; verify
external probe coverage, companion review and six evidence files; and preserve failed or not-assessable windows.

Use a path-only `synara.slo-window-evidence-draft.v1` whose six `evidence` values are relative source paths below one
evidence root. The preparer performs stable bounded reads, rejects duplicate JSON fields, symlinks, traversal and common
credential material, derives the hashes, validates the materialized manifest, then atomically publishes immutable `0600`
manifest/receipt pairs and SHA-256 sidecars:

```bash
bun run stage6:slo:prepare -- \
  --evidence-root /secure/slo-window \
  --draft slo-window-draft.json \
  --manifest-output slo-window-manifest.json \
  --receipt-output slo-window-receipt.json

python3 -m unittest discover -s scripts/stage6-slo -p 'test_*.py'
```

The receipt always states `evidence-validated-not-slo-passed`. Real production telemetry, incident review and release
approval remain external GA evidence.
`stage6:slo:validate` remains available for an independent recheck of an already materialized manifest. Existing attempt
paths are rejected; if receipt publication fails, the preparer rolls back the newly published manifest pair.

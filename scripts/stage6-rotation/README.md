# Stage 6 production rotation evidence

The preferred preparer consumes a `synara.stage6-production-rotation-evidence-draft.v1` JSON document. It has the same
fields as the v1 manifest except every control evidence value, `secretScan.evidence` and every approval evidence value is a
path-only string relative to one private evidence root. The preparer computes all 49 SHA-256 references, scans the draft
and attachments for high-confidence secret material, validates the fully materialized manifest, then publishes manifest,
receipt and both sidecars:

```bash
bun run stage6:rotation:prepare -- \
  --evidence-root /secure/stage6-rc1 \
  --draft production-rotation-draft.json \
  --repository-root . \
  --manifest-output production-rotation-manifest.json \
  --receipt-output production-rotation-receipt.json
```

Use the exact candidate fields and nine-control inventory from
`docs/contracts/production-rotation-acceptance-v1.md`. Each control's `evidence` object contains these path strings:
`changeRecord`, `rolloutStatus`, `newPathProbe`, `oldPathFinalState`, and `rollbackResult`. Draft, evidence and output paths
cannot traverse symlinks or leave the evidence root.

Invalid semantics or evidence publishes nothing. Existing outputs are never replaced, and receipt publication failure
rolls back the manifest pair. All four outputs are exclusive `0600` files.

The lower-level validator remains available for offline revalidation to a new output path:

```bash
bun run stage6:rotation:validate -- \
  --manifest /secure/stage6-rc1/production-rotation-manifest.json \
  --evidence-root /secure/stage6-rc1 \
  --repository-root . \
  --output /secure/stage6-rc1/production-rotation-revalidation-receipt.json
```

An eligible receipt still has assessment `evidence-validated-not-production-rotation-passed`; the real production evidence
authority, approver authority and cryptographic signatures remain external GA checks.

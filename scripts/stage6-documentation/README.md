# Stage 6 enterprise documentation validator

The validator fixes the required end-user, Tenant-administrator, deployment-operator and support-operator documentation
inventory, checks required source markers and rejects broken local or non-HTTPS external links.

```bash
bun run stage6:documentation:validate -- \
  --repository-root . \
  --matrix docs/release-matrices/stage-6-documentation-v1.json \
  --output /secure/stage6-rc1/documentation-source-receipt.json

python3 -m unittest discover -s scripts/stage6-documentation -p 'test_*.py'
```

The receipt always states `source-documentation-validated-not-release-verified`. Only a deployed candidate review with the
named roles and approved public wording can check the documentation item in the GA checklist. When `--output` is used,
the receipt and SHA-256 sidecar are exclusive `0600` files; an existing path is never overwritten and sidecar publication
failure rolls the receipt back. The matrix and five Markdown sources are read as bounded regular non-symlink files; the
validator rejects duplicate matrix fields and prohibited secret material, caps aggregate source bytes, and hashes the same
captured document bytes used for marker and link validation. Source validation does not prove that the deployed candidate's
labels, roles, immutable configuration or approved internal wording match those files.

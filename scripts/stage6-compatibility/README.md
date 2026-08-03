# Stage 6 compatibility matrix validator

The validator compares the checked-in internal-self-hosted matrix with current Tenant Web, Platform Admin, shared client/UI and
contract package versions, Control Plane Migration numeric-version uniqueness, tail and checksum, `/v1` API source,
Worker/Runtime Event constants, Worker Manifest storage schema, and produced/accepted Provider Host Protocol versions.
Drift fails closed.

```bash
bun run stage6:compatibility:check
```

A successful result is `source-compatible-not-release-approved`. Release approval still requires exact artifact digests,
clean commit evidence, prior-build forward-schema proof, Worker canary/promote/rollback evidence, and SLO/Audit review.
The validator stable-reads the matrix, eight package manifests, protocol/API sources and every migration in the numeric
lineage as bounded regular non-symlink files. It rejects duplicate JSON fields and prohibited secret material, uses the
same captured bytes for source interpretation and the migration-tail digest, and reports the total file/byte inventory.
This closes source-snapshot ambiguity; it does not execute the previous build on the target schema or exercise rollback.

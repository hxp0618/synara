# Stage 6 Tenant isolation local and PostgreSQL evidence — 2026-08-02

## Scope and assessment

This report records one full execution of the checked-in focused Tenant-isolation matrix, including every locally
executable row and every PostgreSQL-gated row. It is implementation evidence from a dirty development worktree and a
temporary local database. It is not an independent security audit, a clean release-candidate receipt or production
boundary evidence.

Assessment: `focused-tenant-isolation-evidence-validated-not-audit-passed`.

## Subject

- Branch: `codex/saas-tenancy-user`
- Observed Git HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Worktree: dirty; no clean-commit or candidate-artifact claim
- Matrix: `docs/release-matrices/stage-6-tenant-isolation-v1.json`
- Matrix SHA-256: `37822fbccaee140699a1aef90626305bcca0fd0d1540a44a74e31de1db12c0b5`
- Completion observed at: `2026-08-02T15:56:49Z`
- Go: `go1.26.5 darwin/arm64`
- Docker Server: `29.4.0`
- PostgreSQL image: `postgres:17`
- PostgreSQL image digest: `postgres@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d`

## Execution

The environment-independent pass ran:

```bash
PYTHONDONTWRITEBYTECODE=1 \
  python3 scripts/stage6-security/validate_tenant_isolation_matrix.py --run-tests
```

The PostgreSQL pass supplied one local connection authority to both historical test environment names through the
validator and ran:

```bash
TEST_POSTGRES_URL='postgres://<local-test-authority>' \
PYTHONDONTWRITEBYTECODE=1 \
  python3 scripts/stage6-security/validate_tenant_isolation_matrix.py \
  --run-tests --run-postgres-tests
```

The PostgreSQL container used `max_locks_per_transaction=256`. The validator assigned a separate process-and-test-scoped
schema to each selected test and treated every skip, missing test and failure as a failed run.

## Result

- Evidence rows: `112`
- Environment-independent rows executed: `95/95`
- PostgreSQL-gated rows executed: `17/17`
- Operation groups inventoried: `620`
- Go packages inventoried: `47`
- Test source files inventoried: `79`
- Exported `identity.Principal` service entrypoints: `175`
- Explicitly classified entrypoints: `174`
- Source-contract-verified trusted internal transaction hooks: `1`
- Unclassified entrypoints: `0`
- Final execution marker: `focused-and-postgres-go-tests-passed`
- PostgreSQL evidence marker: `postgres-behavior-executed-not-production-boundary-verified`

The temporary PostgreSQL container was removed after the successful run. The validator completed its per-test schema
cleanup before returning success.

## Deliberate limits

- The database URL and host authority were operator supplied and were not attested.
- The worktree was dirty and the run was not bound to release artifacts, a candidate manifest or a protected build.
- The matrix is deliberately focused and non-exhaustive; it does not prove every branch, concurrent interleaving, object
  store policy, deployment network boundary or identity type.
- No production logs, traces, backup access, cloud IAM/KMS policy or real Support/Region annex were inspected.
- Independent security review, SSRF/command/path/container acceptance and third-party penetration evidence remain open
  Stage 6 gates.

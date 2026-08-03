# Stage 6 route authentication boundary guard

The guard parses every `http.ServeMux` route registration in the Control Plane and fails closed unless it belongs to one
explicit outer boundary: public health/SSO bootstrap, signed Platform publisher,
Worker registration token, Worker token, Service Account, scoped Artifact content token, or Login Session.
It also rejects payment-path variants (`/billing`, `/commercial-billing`, `/checkout`, `/payment`, `/payments`, and
`/stripe`) by pattern, not only by a fixed route list. Internal Provider cost imports under
`/cost-accounting/*/actual-invoices/*` remain allowed because they are cost evidence, not payment collection.

```bash
python3 scripts/stage6-security/validate_route_auth_boundaries.py
python3 -m unittest scripts/stage6-security/test_validate_route_auth_boundaries.py
```

This is an outer-authentication inventory, not an object-authorization or Tenant-isolation proof. Its receipt is therefore
always `route-auth-boundaries-validated-not-tenant-audit-passed`.

The internal self-hosted product-boundary guard separately proves that the current runtime, typed client, Tenant UI,
Platform Admin, Go dependency graph, deployment templates and monitoring rules contain no payment provider,
Checkout/Portal, Stripe or Billing-exercise capability. Legacy migration tables remain outside that scan and are locked
read-only by Migration 154 and the SQLite safety layer. The validator also scans every non-test Control Plane Go source
file: historical payment types/table markers are accepted only in the explicit SQLite lock, authority cleanup and
persistence compatibility seams, and a new active-service reference fails closed:

```bash
python3 scripts/stage6-security/validate_internal_self_hosted_boundary.py
python3 -m unittest scripts/stage6-security/test_validate_internal_self_hosted_boundary.py
```

The focused Tenant-isolation matrix makes existing high-risk behavioral evidence discoverable. A routine run executes
only rows marked for local focused execution. Environment-gated PostgreSQL evidence remains marked
`source-reference-only` in the checked-in matrix and appears separately in every receipt; an operator must explicitly opt
in before those rows execute. The validator also inventories every production Go
package and exported `Service` method that accepts `identity.Principal`; adding a new user-facing service package without
at least one classified Tenant-isolation evidence row fails closed, and any new unclassified matching method also fails.
Every classified method must be directly invoked by its named test. Exact method classifications are emitted separately so
inventory closure cannot be mistaken for operation-level completeness. A trusted internal transaction hook may be
excluded only through an explicit matrix entry whose source contract marker is present; it is reported separately from
behavioral evidence:

```bash
python3 scripts/stage6-security/validate_tenant_isolation_matrix.py
python3 scripts/stage6-security/validate_tenant_isolation_matrix.py --run-tests
TEST_POSTGRES_URL='postgres://...' \
  python3 scripts/stage6-security/validate_tenant_isolation_matrix.py \
  --run-tests --run-postgres-tests
python3 -m unittest scripts/stage6-security/test_validate_tenant_isolation_matrix.py
```

PostgreSQL execution accepts `TEST_POSTGRES_URL` or the historical `SYNARA_TEST_DATABASE_URL`; if both are present they
must be byte-for-byte identical. The runner exports that one authority under both names because the exact evidence tests
predate the naming convergence. Each selected test creates a process-and-test-scoped schema, writes there with `public`
available for shared extension functions, and drops the schema after the test. A skipped, missing, or failed test fails
the receipt. The database URL and server/cluster authority are operator supplied and are not attested by the validator,
so even a 17/17 PostgreSQL result remains local database behavior rather than production-boundary evidence.

Its scope is deliberately non-exhaustive. Even when all referenced Go tests pass, the assessment remains
`focused-tenant-isolation-evidence-validated-not-audit-passed` until every resource family, identity type, deployment
boundary and external security gate in the Stage 6 audit plan is complete.

The Kubernetes observability deployment guard keeps enterprise trace export disabled by default and requires the
production manifest to source endpoint/protocol/Region/retention from the ConfigMap while mounting the mTLS client
identity from a read-only Secret volume. Generated native/Warm Worker Pods and the sandbox-operator standard template use
only a Target-local fixed optional ConfigMap: client identity remains outside agentd in an external relay/service mesh.
Workload registration rejects ConfigMap substitution, required references, Header/client credentials and insecure
overrides:

```bash
python3 scripts/stage6-security/validate_observability_deployment.py
python3 -m unittest scripts/stage6-security/test_validate_observability_deployment.py
```

Its receipt is permanently `enterprise-otel-deployment-wiring-validated-not-collector-accepted`: source wiring does not prove the
collector's IAM, network path, storage Region, retention enforcement or production availability.

The Artifact storage deployment guard fixes the source-controlled object-store boundary. Compose must bootstrap a dedicated
non-root MinIO user with an exact `tenants/*` object policy and exact browser CORS origin; the Control Plane cannot receive
MinIO root credentials. The Kubernetes base contains no static Artifact access key and names the exact ServiceAccount that a
production overlay must bind to a bucket-scoped workload identity. Runtime configuration keeps enterprise endpoints on HTTPS,
requires a session token for temporary static credentials, and caps remote presigned URLs at 15 minutes:

```bash
python3 scripts/stage6-security/validate_artifact_storage_deployment.py
python3 -m unittest discover -s scripts/stage6-security -p 'test_validate_artifact_storage_deployment.py'
```

Its receipt is permanently `artifact-storage-deployment-wiring-validated-not-cloud-iam-accepted`. The guard does not inspect a
real cloud IAM attachment, bucket policy, KMS key, network path, Region, retention rule, access log, or production negative
probe.

The Worker supply-chain evidence validator binds one clean Stage 6 release manifest to the existing production Registry and
Vault/KMS admission reports. It checks the exact Registry report byte hash, commit and Worker digest across all three files;
requires reproducible amd64/arm64 builds with SPDX/SLSA attestations; enforces production KMS/Rekor signing, the checked-in
Trivy policy and fresh dual-platform scans; and requires positive signed admission plus every unsigned, wrong-key, tag-drift
and controller negative probe:

```bash
bun run stage6:worker-supply-chain:validate -- \
  --release-manifest /evidence/stage6-release.json \
  --registry-report /evidence/worker-registry-release-gate.json \
  --admission-report /evidence/vault-kms-admission-gate.json \
  --output /evidence/worker-supply-chain-receipt.json
python3 -m unittest discover -s scripts/stage6-security -p 'test_validate_worker_supply_chain_evidence.py'
```

Its receipt is permanently `evidence-validated-not-worker-supply-chain-approved`. It validates supplied evidence but does not
approve the release or establish the authority of the production Registry, KMS, Rekor, cluster, or operators. Candidate
bundle v4 now requires this receipt as its tenth input; v2/v3 remain readable for historical audit but cannot authorize a new
or active release candidate.
All three input JSON documents are read through the shared stable bounded-file boundary. Symlinks, files changing during
read, duplicate JSON fields and common credential material are rejected; hashes and semantic validation use those same
captured bytes rather than reopening the reports.
The receipt and SHA-256 sidecar use the shared Stage 6 immutable publisher as exclusive `0600` files. A second attempt must
use a new path; collisions fail and sidecar publication failure rolls back the new receipt.

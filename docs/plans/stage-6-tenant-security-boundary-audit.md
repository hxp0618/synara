# Stage 6 Tenant security boundary audit plan

## Current assessment

Status: implementation evidence in progress; Tenant security audit not passed.

The Control Plane currently registers 304 routes, including 122 explicit `/v1/tenants/{tenantID}` routes. The Stage 6
route guard proves that every explicit Tenant route has the Login Session wrapper and that all other routes remain in an
explicit public, signed Platform, Worker registration, Worker, Service Account, Artifact content-token, or Login Session
boundary. This is necessary but not sufficient: authenticating an actor does not prove that every nested resource belongs
to the requested Tenant.

## Required layers

1. Outer authentication: route wrapper or exact token/signature handler; anonymous expansion fails CI.
2. Actor state: Login Session expiry/revocation, Tenant lifecycle, SSO enforcement, Service Account scope, Worker
   revocation/incarnation, or Support Access expiry is rechecked server-side.
3. Permission: the fixed RBAC v1 action is authorized before resource mutation or sensitive read.
4. Ownership: Tenant plus nested Organization/Project/Session/Execution/Artifact/Credential IDs are constrained in the
   same query/transaction. Fetch-by-global-ID followed by a caller-side comparison is not a sufficient write boundary.
5. Runtime fencing: Worker identity, Target, Execution, Generation, Lease token, and immutable Grant are revalidated
   together before any event, Artifact, Credential, or control acknowledgement.
6. Storage: object keys and content tokens bind Artifact ID, operation, expiry, and expected metadata; Workspace paths,
   Provider output, URLs, and Git/SSH inputs use their dedicated traversal/SSRF/command boundaries.
7. Exceptional access: Platform operators are not customer members. Support Access is four-eyes, reasoned, expiring,
   read-only, Tenant-visible, and audited on every request.
8. Diagnostics: logs, traces, metrics, recovery evidence, and exports exclude Secret payloads; Tenant IDs are forbidden
   metric labels and cross-Tenant trace search follows the Support policy.

## Stage 6 surfaces already carrying focused evidence

- Tenant lifecycle and home-Region updates use versioned Tenant authority and fail-closed state/placement checks.
- Domain, SSO enforcement, IdP/SCIM offboarding, and RBAC paths have cross-role and revocation tests.
- Entitlement, usage, soft quota, and cost reads aggregate under the requested Tenant and use PostgreSQL concurrency
  evidence where writes can race.
- Platform operations and Support Access keep internal operator authority separate from customer Membership.
- Legal Hold, DSAR, personal erasure, and Tenant export bind all reads/deletes to the exact Tenant and protect immutable
  evidence; exported bundles omit credential/SSO/KMS/Login secrets.
- Execution traceparent is diagnostic-only and immutable; it is never Tenant, authorization, scheduling, idempotency, or
  fencing authority.
- `docs/release-matrices/stage-6-tenant-isolation-v1.json` currently fixes 112 high-risk evidence rows across 47 Go
  packages, 79 test files and 620 operation groups. Ninety-five environment-independent rows pass in the routine receipt;
  17 PostgreSQL composite-FK, cross-Tenant interaction-renewal, Desktop offboarding, Support and Stage 6 governance rows remain explicitly
  source-only unless an operator supplies one PostgreSQL authority and requests their execution. On 2026-08-02 the unified
  runner executed all 17 exact rows against temporary PostgreSQL 17 using a separate auto-cleaned schema for every test
  process/name; skipped, missing and split-database execution are fail closed. The receipt still identifies the database
  as operator supplied and not attested, so this is repeatable local database behavior rather than a production security
  audit. The execution record is
  [`stage-6-tenant-isolation-local-postgres-20260802.md`](../reports/stage-6-tenant-isolation-local-postgres-20260802.md).
  The matrix's validator also inventories all 26 production Go service packages and 174 exported `Service` methods that directly accept
  `identity.Principal`. Every package has at least one classified evidence row; 174 methods are explicitly mapped to named
  tests that directly invoke them, one internal transaction hook is separately allow-listed with a source-verified caller
  contract, and none remain unclassified. This exported-method inventory closure is not operation-level completeness; the
  matrix's explicit non-exhaustive scope prevents the focused evidence from being promoted to a complete Tenant audit.

## Findings fixed during this audit

- Active Tenant matching had been duplicated across user-facing services, with inconsistent 403/404 disclosure semantics
  and no shared zero-ID rule. `identity.RequireActiveTenant` is now the single fail-closed guard for these service and HTTP
  boundaries; missing, zero and substituted contexts uniformly return `404 tenant_not_found` before RBAC or mutation.
- Tenant, Organization and Audit administration accepted a path Tenant for any active Membership without requiring it to
  equal the Login Session's active Tenant. The shared tenancy authorization entry now rejects that context substitution;
  invitation acceptance and deletion recovery remain separate token/owner-authorized exceptions.
- Write handlers could parse and reject JSON before reaching their service-level active Tenant check. Login middleware now
  rejects all 121 non-recovery explicit Tenant routes before handler/body processing; the Owner-only deletion recovery
  route is the single documented exception because a deleting Tenant cannot remain the active Tenant.
- Tenant Outbox list/replay accepted a supplied Tenant independently of the Login Session's active Tenant. Both operations
  now reject the mismatch before loading or mutating a message.
- Project service methods accepted a supplied Tenant whenever the user held a Membership there, even when another Tenant
  was active in the Login Session. All Project create/list/get/update/archive entries now reject that context substitution
  before authorization, validation or mutation; ID-only HTTP routes continue deriving the Tenant from the active session.
- Identity mapping list returned an empty list when the Connection ID belonged to another Tenant. It now proves the parent
  Connection belongs to the exact Tenant before listing mappings.
- Credential Binding list returned an empty list for a Project or Execution Target owned by another Tenant. It now proves
  the exact owner first and returns the resource-specific not-found boundary.
- Tenant `admin | owner` invitations automatically grant the matching root Organization role. Tenant-role downgrade now
  removes only that matching automatic grant in the same transaction, preventing stale root authority while preserving an
  Organization role that was independently changed.
- Entitlement projection, Tenant/Project Resource Lifecycle, Agent Memory publication, Worker Pool Autoscaling,
  Retention, Tenant Usage/soft-quota projection, and Tenant Support policy/history now have explicit entrypoint tests that
  substitute a different active Tenant and require `404 tenant_not_found` before validation or storage access. The tests
  deliberately use no database handle, so any future reordering that reaches authorization, reads, alert projection, or
  writes before the shared guard fails immediately instead of silently weakening the boundary.
- Provider Credential create/rotate/auto-select/scope-policy operations, Credential Binding create/disable, and Enterprise
  Identity connection/domain/SSO-policy/mapping operations now have the same pre-secret, pre-DNS and pre-storage active
  Tenant rejection evidence. Their tests use no database, KMS, Credential service or DNS resolver, so a reordered boundary
  fails before it can validate or touch those dependencies.
- Execution Target lifecycle/runtime-policy, Worker Pool mutation, Tenant quota, Service Account inventory, Privacy list,
  and Worker Release policy operations now reject an inactive Tenant before policy parsing, idempotency validation or
  storage. Artifact evidence now also covers completion plus a Session-scoped list substitution and proves the original
  Tenant still sees exactly its own completed Artifact.
- The exported-entrypoint guard caught the new runtime-isolation policy update and generation-decision read paths before
  they reached CI. Both are now directly exercised with inactive/cross-Tenant substitution, and the same aggregate Stage 6
  engineering gate runs this inventory on ordinary CI and release preflight.
- Live Session event sanitization previously trusted the Event Tenant for non-interaction events and only reauthorized
  interaction events. It now checks the Login Session's active Tenant before either path. The same cross-Tenant regression
  exercises all exported Session create/turn/idempotency/archive/status/model/history/subscription methods and proves no
  Session, Turn, Event or subscription state changes.
- Service Account authentication previously revalidated only the account and token, so an already-issued SCIM Bearer could
  survive Tenant suspension, closure, deletion, or trial expiry. Authentication now joins the owning Tenant, rejects every
  non-operational lifecycle state without revealing which check failed, and leaves `last_used_at` untouched on denial.
  Focused evidence also presents real Login Session, Service Account, and Worker credentials to every HTTP authentication
  wrapper and proves that only the matching credential type reaches the protected handler.
- Member offboarding previously revoked Login Sessions and user-scoped Provider Credentials but left pending Desktop
  Enrollments intact. A pending link failed only while Membership was suspended and could become usable again after
  reactivation if it had not expired. The shared manual/SCIM offboarding transaction now terminally revokes every pending
  Enrollment for the exact subject/Tenant, revokes all Web/Desktop Login Sessions, increments the Enrollment version fence,
  and records separate counts in the parent Audit event. Reactivation restores only Tenant Membership; it cannot restore
  the old Enrollment, Session, Organization access, or Credential. The user-global Desktop Device registration is
  deliberately not revoked by a customer Tenant; it carries no independent access and only Platform administration or the
  user's authenticated Disconnect owns that cross-Tenant action. SQLite manual and SCIM paths plus PostgreSQL 17
  connected-device/pending-link behavior pass.
- Billing tariff, configured invoice, reconciliation, shared-ledger, sweep and allocation administration now rejects an
  inactive Platform Billing Tenant before configuration parsing, adapters or storage. Platform Support operations likewise
  reject a customer Tenant context before Grant lookup. Self-service creation, Platform provisioning, membership-filtered
  Tenant inventory, deletion recovery and final cleanup now carry exact authority-specific evidence.
- A User who owns two Tenants cannot read the first Tenant's Session Usage or effective Agent Memory while the second
  Tenant is active, even though Membership exists in both. A customer administrator likewise cannot revoke another
  Tenant's active Support Grant by substituting its global ID; the service returns `tenant_not_found` and the Grant status
  and version remain unchanged. These tests exercise real SQLite ownership queries rather than only the shared context guard.
- Support Access no longer treats approval-time Platform membership as sufficient for the full Grant lifetime. Migration
  `000110` binds every new Grant to an immutable Platform Operator Tenant; authorization, Tenant inventory and Login
  Session authentication recheck the requester is still an active `owner|admin|security_admin` of an active Operator
  Tenant. Offboarding or role loss immediately removes customer read access and clears the stale active-Tenant Session
  context. Historical unbound pending/active Grants fail closed instead of guessing their authority provenance.
- The HTTP identity matrix now enumerates all 122 explicit customer Tenant routes. An active Support Grant reaches every
  GET route, every attempt is audited, and every write is rejected before its handler. A Platform operator who remains an
  active Operator-Tenant administrator but has no customer Grant has the stale customer context cleared and cannot reach
  any non-recovery handler. The one deletion-recovery middleware exception is exercised through the real handler and still
  returns `tenant_not_found` because only a customer Owner membership can authorize recovery.
- Enterprise trace export now validates its external diagnostic boundary before creating an exporter. Enterprise Control
  Plane requires absolute HTTPS OTLP/HTTP and complete mTLS client identity; non-Local agentd instead accepts only
  credential-free HTTPS or an explicit IP-loopback relay and forbids Header/client identity injection. Both require bounded
  Region and 1–90 day retention declarations; URL credentials/query/fragment, insecure transport overrides and protocol
  drift fail startup. Region and retention are projected only as bounded resource metadata, so the external collector's IAM, Network,
  actual storage Region and deletion behavior remain deployment evidence rather than a source-level claim.
- Prometheus label serialization now has one fail-closed runtime boundary shared by ordinary labels and histogram/quantile
  additions. Identifier-shaped keys, UUID/Commit/Digest/URL/Email/known credential-shaped values and unbounded values
  panic before rendering; the panic is constant and never copies the rejected key/value into logs. This prevents a future
  metric from silently turning Tenant or operational identifiers into a scrape-visible, high-cardinality label even when
  its individual collector test forgot an explicit sentinel assertion.

Focused tests also cover Project, Session/Turn/Event, Artifact, Credential, Service Account and nested resource ID
substitution, plus database composite foreign keys, Support read-only enforcement and Worker runtime fencing. These fixes
and tests are implementation evidence, not an independent security review.

## Automated outer-boundary guard

`scripts/stage6-security/validate_route_auth_boundaries.py` parses all route registrations rather than sampling names. Its
explicit public allow-list contains only health/readiness/metrics, sanitized Platform profile, dev-login/SSO bootstrap,
signed routing publication, Worker registration, and scoped Artifact content endpoints. A new unwrapped `/v1` route,
changed token handler, missing Worker/SCIM wrapper, duplicate route, or unparseable registration fails closed.

The receipt deliberately states `route-auth-boundaries-validated-not-tenant-audit-passed`. The script cannot see dynamic
SQL ownership predicates, authorization ordering, response filtering, object-store policy, or deployment networking.

`validate_artifact_storage_deployment.py` now fixes the source-controlled object-store boundary: Compose uses a private
bucket, exact CORS origin and dedicated non-root MinIO user whose object authority is confined to `tenants/*`; Kubernetes
injects no static Artifact key and names the ServiceAccount to be bound by a production workload-identity overlay. Runtime
configuration rejects enterprise HTTP/credential-bearing endpoints, long-lived static enterprise keys and presign TTLs over
15 minutes. The validator's eight mutation tests and a local MinIO run also prove allowed production-prefix lifecycle plus
denial of an outside-prefix write, bucket creation and Admin API access. Its assessment remains
`artifact-storage-deployment-wiring-validated-not-cloud-iam-accepted` because no real cloud IAM, bucket policy, KMS, Region,
network or access-log authority was inspected.

## Focused behavioral evidence guard

`scripts/stage6-security/validate_tenant_isolation_matrix.py --run-tests` validates every source/test reference in the
focused matrix and executes the exact named locally runnable Go tests grouped by package. The current source matrix contains
112 evidence rows, 620 operation groups, 47 packages and 79 test files: 95 environment-independent rows and 17 explicitly
source-only PostgreSQL rows. `--run-postgres-tests` requires one operator-supplied PostgreSQL authority, aliases both
historical environment names to that exact URL and isolates every selected test in an auto-cleaned schema. All 17 rows
passed together on temporary PostgreSQL 17 on 2026-08-02; the routine receipt still reports those classes separately
and is permanently
`focused-tenant-isolation-evidence-validated-not-audit-passed`: adding a test row improves traceability but does not prove
unlisted operations, identities, concurrency paths, object storage, deployment policy or independent attack testing. A
new production service package that accepts `identity.Principal` now fails validation until the matrix classifies at least
one Tenant-isolation test for that package, and any new unclassified matching method also fails. The receipt additionally
lists all 175 matching exported `Service` methods as 174 classified, one source-contract-verified trusted internal
transaction hook and zero unclassified entrypoints. It
rejects a classification unless the named test directly invokes the method. This closes the exported-method inventory
without pretending the current evidence covers every branch, identity, concurrent interleaving or deployment boundary.

Execution Worker receipts now carry an immutable Tenant, Execution, Generation and Target authority tuple added by
Migration `000133`. Every lease-scoped idempotent replay locks the current Execution and rejects a successor Generation or
Session Target move before decoding the stored success response; Worker re-registration is independently fenced by the current
Worker incarnation and instance UID. Workspace cleanup and storage-scrub receipts remain on their separate dispatch and
materialization fencing contracts rather than borrowing Execution authority.
Migration `000133` and the successor-Generation/Session-Target replay cases also passed against a temporary PostgreSQL 17
instance with per-test schema cleanup. This is local database evidence, not attestation of the production Worker fleet.

The applicable offboarding identity matrix now covers Web Login Sessions, Desktop Bearer credential families, pending
Desktop Enrollments, user-scoped Provider Credentials and their immutable Execution Grant
revalidation, pre-existing SSO attempts, and Platform Support authority. Service Accounts and Workers are Tenant-owned
non-user identities with independent revoke/incarnation lifecycles; a Desktop Device is a user-global key registration, not
a Tenant actor credential; Artifact content tokens and object-store presigned URLs are bounded capability grants rather
than actor identities and remain part of the resource/object-store audit gate below.

## Remaining audit gates

- Expand the current focused two-Tenant matrix to every resource family, including nested-ID substitution on list, get,
  create, update, delete, export, presign, replay, and concurrent paths. Preserve exact 403/404 disclosure semantics.
- Run SSRF, command injection, path traversal, supply-chain, and container-escape acceptance only after the exact Stage 5
  dependency gate is confirmed for the target deployment. Self-managed Kubernetes evidence does not prove managed-cloud
  IAM, Region, or admission controls.
- Review PostgreSQL RLS decision (currently application-authority model), attach and negatively test the candidate cloud
  object-store IAM/bucket/KMS policy, and review backup/trace/log access plus the deployment's real Support and Region annex.
- Obtain independent security review/penetration evidence and close or explicitly accept every high-severity finding.

Only those gates plus the copied GA checklist can change this assessment to passed.

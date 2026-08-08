# Cross-component release compatibility v1

## Authority and scope

`docs/release-matrices/stage-6-compatibility-v1.json` is the machine-readable current-source baseline for managed
internal self-hosted Enterprise deployments. It covers Control Plane Migration, `/v1` API, Tenant Web, Platform Admin, Desktop, shared
client/UI/contract packages, Worker Protocol, Worker Manifest, Runtime Event, and Provider Host Protocol. It does not replace Worker
canary/promote/rollback implemented in Stage 3/4.

Every release candidate copies this baseline into its v5 evidence bundle, binds its exact bytes to the exact clean Git commit, five self-hosted
service artifact digests and four native Desktop artifact digests, and validates it with
`scripts/stage6-compatibility/validate_compatibility_matrix.py`. The checked-in
file always says `design-current-not-release-approved`; validation proves source consistency, not a successful rollout.
The validator preserves the caller-visible matrix path so a top-level symlink cannot be hidden by path resolution. It
stable-reads bounded regular non-symlink inputs for the matrix, eight package manifests, API/protocol sources and the full
numeric Migration lineage, rejects duplicate JSON fields and prohibited secret material, and derives the tail digest from
the same captured bytes used for the source snapshot. The receipt reports the exact source file and byte counts. These
properties protect the evidence input boundary but do not authenticate Git cleanliness or artifact provenance.

## Current compatibility decisions

- Control Plane exposes API major `v1`. Web, Platform Admin, Desktop, server, contracts, `@synara/control-plane-client`, and
  `@synara/enterprise-ui` at `0.7.0` are a same-release unit because there is not yet an independently versioned
  browser compatibility handshake.
- Managed Worker registration is exact Worker Protocol `2`. Worker Protocol v1 is not registration-compatible.
- Control Plane can read persisted Runtime Event v1 and v2. Managed Workers and Provider Host v2 emit Runtime Event v2.
- Managed Provider Host emits Protocol `2.2`; Control Plane accepts major `2` with minor `>= 1`. Major mismatch and older
  minor are non-schedulable. The Worker manifest's provider/runtime compatibility checks remain the per-image authority.
- Worker Manifest storage schema is `3` and is independent from Workspace layout v3.
- The current Control Plane Migration tail is `000167_stage7_execution_target_provisioning.sql` with its exact
  SHA-256 frozen in the machine matrix. Migrations 165-167 add the Stage 7 developer-platform authorities while retaining
  the same forward-only application rollback boundary. Migration 164 adds the authoritative Session settlement marker
  used by the Activity lifecycle. Migration 163 makes Candidate v5 the only active release authority, binds the
  source-current compatibility matrix projection at PostgreSQL and SQLite boundaries, and retains terminal v2-v4 bytes
  as audit history. Migration 162 upgrades current Operations evidence to v2 and retains v1 history without authority.
  Earlier migrations remain immutable. Migrations are forward-only expand/contract; application rollback is allowed only
  after the previous build has been tested against the target schema. Database rollback is never an automatic release
  action.
- A managed Worker image must report the release Git commit, immutable image digest, protocol ranges, Provider runtime
  versions, SBOM, and containment evidence already enforced by Worker Manifest and Worker Release admission.

## Rollout and rollback

The only accepted order is:

1. collect and approve immutable Control Plane, Tenant Web, Platform Admin, Worker, Provider Host, and four native
   Desktop artifact digests;
2. verify a usable restore point under the recovery contract;
3. apply additive migrations and validate their checksums/invariants;
4. roll Control Plane and same-release Tenant Web, Platform Admin, client/UI packages and contracts, then publish the
   signed Desktop candidate artifacts without advancing their update channel before installed acceptance;
5. canary then promote the compatible Worker Release using the existing Target-scoped mechanism; and
6. verify SLO, Audit, compatibility inventory, and the bounded rollback observation window.

Rollback stops at the newest safe layer. A Web or Control Plane artifact may roll back only when the prior version's
forward-schema test is evidence-backed. Worker rollback uses the existing release revision and drain/fencing behavior.
Provider Host is part of the Worker artifact and cannot be swapped independently after its manifest is approved.
Migration down/undo and direct Worker row edits are forbidden recovery shortcuts.

## Change rules

- Additive `/v1` fields require tolerant readers and focused old/new fixture tests.
- Removing or changing a required field requires a new API/protocol major and an explicit coexistence window.
- Increasing Worker Protocol, Runtime Event write version, Provider Host major, or Worker Manifest schema requires a
  dual-version rollout design before constants change. Updating only the JSON matrix is insufficient.
- A migration that makes the previous Control Plane unsafe blocks application rollback and must be split into expand,
  backfill, reader switch, and later contract releases.
- Package-version drift, migration-tail drift, or protocol-constant drift makes the checker fail closed until this
  contract and matrix are reviewed together.

## Release evidence

The copied matrix must add release commit, artifact digests, candidate ID, execution time, operator/approver, migration
receipt, previous-build forward-schema test, Worker canary/promote/rollback exercise, and SLO/Audit links outside this
source baseline. Only the Stage 6 release checklist records the final decision.

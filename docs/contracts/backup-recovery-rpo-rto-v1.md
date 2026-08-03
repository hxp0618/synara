# Backup, recovery, RPO, and RTO contract v1

## Promise boundary

This contract defines the minimum Stage 6 backup authorities, recovery procedures, and provisional recovery objectives.
It is not evidence that backups exist or that a production restore has passed. Every deployment must bind the profiles
below to named services, Regions, retention, encryption authorities, and immutable evidence in a private deployment annex.

RPO is measured from the instant the source is isolated from new writes (`sourceCutoffAt`) to the latest mutation proven
present in the recovered authority (`restoredThroughAt`). RTO is measured from the authorized start of recovery
(`recoveryStartedAt`) to successful service and real-client validation (`serviceReadyAt`). Backup-job completion time is
neither measurement.

## Component objectives

| Component/profile                           | Required authority                                                                                                                 | Provisional RPO | Provisional RTO |
| ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- | --------------: | --------------: |
| PostgreSQL / `postgresql-pitr`              | Encrypted physical base backup plus continuously archived WAL in another failure domain.                                           |           5 min |          60 min |
| Object storage / `object-versioned-replica` | Versioning, deletion protection, inventory, and an encrypted replica or backup in another failure domain.                          |          15 min |         120 min |
| KMS / `kms-cloud-multi-region`              | Provider-supported durable multi-Region key authority, alias/policy inventory, and decrypt canary. Key material is never exported. |               0 |          60 min |
| KMS / `kms-vault-raft-snapshot`             | Encrypted Raft snapshot, Shamir custody, isolated restore, audit devices, and key/role/decrypt validation.                         |            24 h |         120 min |
| Queue / `queue-postgres-outbox-replay`      | PostgreSQL Outbox is the authority; the broker is rebuilt and idempotently replayed from restored metadata.                        |           5 min |          60 min |

The offer must publish the selected KMS profile and the worst applicable objective for each data class. It must not blend
the stronger PostgreSQL target with a weaker Vault target into one misleading platform-wide RPO.

## PostgreSQL

- Back up the exact production major version with encrypted physical base backups and WAL archiving. A logical dump is a
  portability aid, not the sole disaster-recovery authority.
- Store backup catalog, checksums, encryption-key reference, WAL continuity, retention expiry, and deletion-lock evidence
  outside the source database.
- Restore into a clean isolated instance, replay to a selected timestamp, run all migration invariants, and prove Tenant,
  Audit, Outbox, Legal Hold, export receipt, and immutable execution evidence can be read.
- The client validation must use normal authenticated API paths. Direct SQL row counts alone do not establish service
  readiness.

## Object storage

- Enable versioning and a retention/deletion-control policy appropriate to the deployment. Replicate the production
  Artifact, Checkpoint, Memory, Audit-export, and evidence prefixes named by the annex.
- Inventory object key/version, size, checksum, encryption metadata, retention status, and replication watermark. Never
  include presigned URLs or plaintext secrets in evidence.
- Restore selected objects plus a statistically meaningful inventory sample into an isolated bucket and validate access
  through Synara using the database's exact object version/checksum authority.
- A bucket list or replication configuration is not restore proof; the bytes must be read and checked by a real client.

## KMS and secrets

Cloud KMS key material must never be exported for backup. Recovery validates the provider's configured multi-Region or
durability primitive, alias/policy inventory, least-privilege identities, and a ciphertext canary produced before source
isolation. Decrypting with a broad root/operator identity does not prove the application path.

Vault uses the existing isolated Raft snapshot restore drill and Shamir custody runbook. A snapshot file alone is not a
recovered KMS: the restored authority must unseal independently, expose the required audit sinks, retain expected
transit/AppRole configuration, and decrypt a pre-isolation canary through the intended application identity.

## Queue and delivery

Synara's durable Outbox in PostgreSQL is the source of truth. An external broker carries delivery, not unique business
state, and therefore has no independent backup promise. Recovery must:

1. restore the Outbox with PostgreSQL;
2. start with an empty or isolated broker destination;
3. resume publishing under a single fenced dispatcher authority;
4. prove pending messages are delivered and repeated messages are idempotent; and
5. leave no unexplained dead letters or skipped sequence.

If a future queue contains authoritative state not reconstructible from PostgreSQL, it requires a new version of this
contract before production use.

## Scheduling, retention, and encryption

- PostgreSQL base backup at least daily; WAL archive continuously with a five-minute maximum verified gap.
- Object inventory daily and replication continuously with a 15-minute maximum verified lag.
- Vault Raft snapshot at least daily; the cloud KMS profile follows provider durability and tests a decrypt canary daily.
- Retain 35 daily and 13 monthly recovery points unless a stricter customer/legal policy applies. Legal Hold governs
  business records, while backup expiry follows the deployment retention annex; neither is silently substituted for the
  other.
- Encrypt in transit and at rest. Backup/evidence credentials are separate, short-lived, audited, and unable to modify
  the source workload.

## Evidence and pass criteria

The operator follows `docs/runbooks/backup-recovery-drill.md` and uses
`scripts/stage6-recovery/prepare_recovery_evidence.py` in `subject` then `final` mode. The path-only draft is materialized
and validated before immutable manifest/result pairs are published. The v2 manifest binds the drill to the canonical candidate ID,
commit, lockfile, environment ID/class, sorted Regions, HTTPS origins, self-hosted service/Desktop Artifact digests and Migration tail. The
restored deployment repeats its observed commit/lockfile/Artifact/Migration identity and must match exactly; restoring a
prior Artifact under a current drill ID fails closed.

The validator checks schema, closed drill/component/approval/validation time order, timestamp-derived RPO/RTO, source and
restore Regions, distinct failure domains, exact evidence hashes, and an explicit real-client canary result. It derives a
`recoverySubjectSha256` over the normalized candidate, drill, restored identity, sorted component measurements and evidence.
Database, KMS, Operations, Security and Storage approvals are separate strict JSON files with distinct roles/identities and
must bind that exact subject digest.

Subject generation requires empty approvals. Final preparation requires all five approval files, so an approval cannot be
silently manufactured before the exact component evidence subject is frozen. Direct validation remains available for an
independent recheck of an already materialized immutable manifest.

Failed canaries, missed objectives and explicit `reviewed-not-approved` decisions remain valid non-pass receipts with
`eligibleForHumanGateReview=false`; malformed or cross-candidate evidence is rejected. Every receipt deliberately says
`evidence-validated-not-control-passed`: file shape and self-consistency cannot prove that an operator used a real production
backup or that the recorded approver identity/signature is authentic.

Only the copied release checklist's authorized reviewers can mark the control passed after they verify source-system
backup IDs, independent restore targets, real client output, no Secret leakage, and measured objectives.

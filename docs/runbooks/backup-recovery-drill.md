# Production backup and recovery drill

## Safety boundary

Run restores only into a new account/project, network, cluster, database, bucket, and queue namespace that cannot write to
the source. Resolve every target explicitly before mutation. Never restore over the source, reuse the source broker topic,
or run cleanup against a broad path/account. Production backup credentials must be read-only; restore credentials must
have no authority over the source.

The drill needs an incident commander, database operator, storage operator, KMS custodian, application validator, and an
independent reviewer. The private operations annex names people and access paths; this repository does not.

## Before the drill

1. Select an exact release commit, environment class and deployment-unique environment ID, source failure domain, restore failure domain, maintenance/incident ID,
   and the KMS profile from `docs/contracts/backup-recovery-rpo-rto-v1.md`.
2. Confirm current backup catalogs, retention locks, encryption-key availability, WAL/object replication lag, Vault or
   KMS canary, Outbox depth, and public SLO state. Do not repair evidence just before recording it.
3. Write a unique harmless canary through normal authenticated application paths. Record its Audit/request reference and
   UTC commit time without copying its plaintext into the evidence bundle.
4. Capture immutable source backup/version IDs and checksums. Record `sourceCutoffAt` when source writes are fenced or the
   simulated failure is injected.

## Restore sequence

1. Create the isolated restore target and prove it has a different failure-domain and authority identifier.
2. Restore PostgreSQL base backup and WAL to the selected recovery point. Record the latest recovered canary/mutation UTC
   time as `restoredThroughAt`; run migration discovery and database invariants.
3. Restore object versions into an isolated bucket. Match key/version/checksum authority from PostgreSQL and retrieve the
   canary plus the selected inventory sample through Synara.
4. Restore the selected KMS profile. For cloud KMS, validate the configured replica using the application's least-
   privilege identity. For Vault, execute the isolated Raft snapshot drill and validate audit, roles, transit key, and
   application decrypt.
5. Start an empty isolated broker and one fenced Outbox dispatcher. Verify pending delivery, replay idempotency, ordered
   Session/Event behavior, and zero unexplained dead letters.
6. Start Control Plane, Worker, Provider Host, and Web artifacts matching the recorded release identity. Validate login,
   SSO recovery path, Tenant listing, Session replay, one new Execution, Event streaming, Artifact download, Audit query,
   and the harmless canary through real client paths.
7. Record `serviceReadyAt` only after all required client checks pass. Continued partial availability is not recovery.

## Evidence bundle

Create separate non-secret files for each component's backup, restore, and client proof. Freeze the exact candidate before
the drill; do not regenerate or substitute an Artifact while evidence is being collected. The v2 manifest uses:

```json
{
  "schemaVersion": "synara.recovery-drill-evidence.v2",
  "candidate": {
    "candidateId": "stage6-rc1",
    "sourceCommit": "0000000000000000000000000000000000000000",
    "lockfileSha256": "0000000000000000000000000000000000000000000000000000000000000000",
    "environmentClass": "production",
    "environmentId": "production/stage6-rc1",
    "regions": ["region-a", "region-b"],
    "origins": {
      "controlPlaneBaseUrl": "https://control.example.com/v1",
      "webBaseUrl": "https://app.example.com",
      "adminBaseUrl": "https://admin.example.com"
    },
    "artifacts": {
      "controlPlaneImage": "sha256:...",
      "workerImage": "sha256:...",
      "providerHostImage": "sha256:...",
      "webArtifact": "sha256:...",
      "adminArtifact": "sha256:...",
      "desktopArtifacts": {
        "linux-x64": "sha256:...",
        "macos-arm64": "sha256:...",
        "macos-x64": "sha256:...",
        "windows-x64": "sha256:..."
      }
    },
    "migrationTail": {
      "name": "000133_worker_request_receipt_execution_authority.sql",
      "sha256": "sha256:..."
    }
  },
  "drill": {
    "drillId": "00000000-0000-4000-8000-000000000000",
    "startedAt": "2026-01-01T00:00:00Z",
    "completedAt": "2026-01-01T02:00:00Z"
  },
  "restoredReleaseIdentity": {
    "sourceCommit": "0000000000000000000000000000000000000000",
    "lockfileSha256": "0000000000000000000000000000000000000000000000000000000000000000",
    "artifacts": {
      "controlPlaneImage": "sha256:...",
      "workerImage": "sha256:...",
      "providerHostImage": "sha256:...",
      "webArtifact": "sha256:...",
      "adminArtifact": "sha256:...",
      "desktopArtifacts": {
        "linux-x64": "sha256:...",
        "macos-arm64": "sha256:...",
        "macos-x64": "sha256:...",
        "windows-x64": "sha256:..."
      }
    },
    "migrationTail": {
      "name": "000133_worker_request_receipt_execution_authority.sql",
      "sha256": "sha256:..."
    }
  },
  "components": [
    {
      "component": "postgresql",
      "profile": "postgresql-pitr",
      "sourceAuthority": "prod-postgres-primary",
      "restoreTarget": "drill-postgres-20260101",
      "sourceRegion": "region-a",
      "restoreRegion": "region-b",
      "sourceFailureDomain": "region-a/account-prod",
      "restoreFailureDomain": "region-b/account-drill",
      "sourceCutoffAt": "2026-01-01T00:10:00Z",
      "restoredThroughAt": "2026-01-01T00:08:00Z",
      "recoveryStartedAt": "2026-01-01T00:15:00Z",
      "serviceReadyAt": "2026-01-01T00:50:00Z",
      "restoreServedCanary": true,
      "backupEvidence": { "path": "postgresql-backup.json", "sha256": "sha256:..." },
      "restoreEvidence": { "path": "postgresql-restore.json", "sha256": "sha256:..." },
      "clientEvidence": { "path": "postgresql-client.json", "sha256": "sha256:..." }
    }
  ],
  "approvals": {
    "database": { "path": "database-approval.json", "sha256": "sha256:..." },
    "kms": { "path": "kms-approval.json", "sha256": "sha256:..." },
    "operations": { "path": "operations-approval.json", "sha256": "sha256:..." },
    "security": { "path": "security-approval.json", "sha256": "sha256:..." },
    "storage": { "path": "storage-approval.json", "sha256": "sha256:..." }
  }
}
```

Include all four components exactly once. The KMS component selects either `kms-cloud-multi-region` or
`kms-vault-raft-snapshot`. Copy the exact candidate Artifact and Migration objects into `restoredReleaseIdentity`; do not
retype or normalize them independently. Calculate every evidence SHA-256 from the exact retained file.

After the component evidence is frozen, create a path-only draft with schema
`synara.recovery-drill-evidence-draft.v2`: replace each component evidence reference with its relative source-file path,
set `approvals` to `{}`, and let the two-stage preparer derive the canonical subject. All paths below are relative to the
single evidence root:

```bash
bun run stage6:recovery:prepare -- \
  --mode subject \
  --evidence-root /absolute/path/recovery-evidence \
  --draft drill-subject-draft.json \
  --expected-candidate-binding-sha256 sha256:<canonical-candidate-digest> \
  --manifest-output recovery-subject-manifest.json \
  --result-output recovery-approval-subject.json
```

The subject-only artifact is not a recovery receipt. Copy its `recoverySubjectSha256` into each approval. Every approval
reference in the final manifest must point to unique strict JSON using `synara.recovery-drill-approval-evidence.v1`:

```json
{
  "schemaVersion": "synara.recovery-drill-approval-evidence.v1",
  "approval": {
    "role": "database",
    "approverId": "database-reviewer-reference",
    "decision": "approved-for-human-gate-review",
    "approvedAt": "2026-01-01T02:30:00Z",
    "expiresAt": "2026-02-01T00:00:00Z",
    "subjectSha256": "sha256:..."
  },
  "verificationBoundary": {
    "contentValidation": "strict-schema-and-subject-validated",
    "externalVerification": "approver-identity-and-signature-required-not-verified"
  }
}
```

Use the matching role for Database, KMS, Operations, Security and Storage, with five distinct approver identities. The
machine validator does not authenticate those identities or verify cryptographic signatures; the independent release review
must do so. When recording the five Platform Admin decisions, bind each external approval file's exact bytes using its own
non-zero lowercase `sha256:<64 hex>` digest.

Migration `000143` preserves URL-only legacy decisions as superseded history and reopens any Recovery Drill they previously
approved back to `recorded`. Database, KMS, Operations, Security and Storage must submit replacement byte-bound decisions
before either the Drill or any forward Release transition can be approved again. Superseded rows remain immutable and no
longer occupy active role/operator uniqueness slots.

Put the five approval file paths into a final path-only draft, then validate and freeze the final manifest/receipt pair:

```bash
bun run stage6:recovery:prepare -- \
  --mode final \
  --evidence-root /absolute/path/recovery-evidence \
  --draft drill-final-draft.json \
  --expected-candidate-binding-sha256 sha256:<canonical-candidate-digest> \
  --manifest-output recovery-final-manifest.json \
  --result-output recovery-receipt-v2.json
```

The output and `.sha256` sidecar are inputs to release approval. A successful validator exit confirms self-consistent
evidence, not production control passage. Validation must happen after all five approvals and no later than seven days after
drill completion. Failed canaries, missed objectives, or `reviewed-not-approved` decisions produce an ineligible non-pass
receipt rather than disappearing from evidence.

The preparer rejects symlinks, traversal, duplicate JSON fields, unstable/oversized sources and common credential material;
it publishes both outputs with exclusive `0600` permissions and rolls the manifest pair back if result publication fails.

## Review and cleanup

The independent reviewer compares source backup IDs to provider control-plane truth, recomputes hashes, inspects failed
and retried steps, confirms RPO/RTO, checks for Secret/personal-data leakage, and records residual risk. Keep the isolated
restore until review completes, then delete only the explicitly recorded drill resources under the approved retention
policy. Preserve the receipt and redacted evidence in the access-controlled evidence repository.

Run database/object/KMS/Queue restore at least semiannually and a Region evacuation/failover exercise at least
semiannually, alternating failure domains. A tabletop, backup-success notification, or validator-only fixture does not
count.

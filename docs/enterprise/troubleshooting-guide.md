# Enterprise troubleshooting guide

Start with the narrowest user-visible symptom, then correlate the deployed UI, `/ready`, bounded metrics, Audit records and
redacted logs. Support Access is the supported Tenant diagnostic path. Direct production database access or state mutation
is not an ordinary troubleshooting tool.

## Five-minute triage

1. Record the UTC start time, affected Tenant/Session/Execution IDs, candidate version/digests, visible error code, request
   ID and trace ID. Do not capture prompts, response bodies, Provider payloads, cookies, tokens or presigned URLs.
2. Check the internal Status Board and current maintenance/release notice.
3. Check `/health` and `/ready`; a live process can correctly be unready because PostgreSQL, object storage, schema or KMS
   is unavailable.
4. In Platform Tenant overview, check offline Targets/Workers, active/queued/oldest Executions, recent failures, Artifact
   pressure, Credential availability and disabled identity connections.
5. From the Tenant Support destination, download the redacted Support diagnostic snapshot when a byte-bound handoff is
   useful; it includes aggregate health, Token/Network usage and internal cost coverage but no secrets or payloads.
6. If Tenant detail is required, request time-bounded read-only Support Access with a concrete reason and independent
   approval. Exit or revoke it after diagnosis.

## Symptom routing

| Symptom                            | First checks                                                                                     | Canonical procedure                                                                |
| ---------------------------------- | ------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------- |
| Login or SSO fails                 | Tenant lifecycle, verified Domain, active connection, required-SSO policy, recovery owner, clock | [Control Plane operations](../runbooks/control-plane-operations.md)                |
| Browser repeatedly reconnects      | SSE headers, every proxy's buffering/idle timeout, last event sequence                           | [Control Plane operations](../runbooks/control-plane-operations.md)                |
| Execution stays queued             | Target/Worker availability, queue age, quota, capacity reservation, Tenant lifecycle             | [Administrator daily operations](../runbooks/enterprise-admin-daily-operations.md) |
| Provider authentication/rate limit | Selected Credential scope/status/expiry, Provider incident and quota                             | [Control Plane operations](../runbooks/control-plane-operations.md)                |
| Artifact upload/download fails     | Metadata state, object-store readiness, CORS origin, presigned URL clock/expiry                  | [Control Plane operations](../runbooks/control-plane-operations.md)                |
| Outbox is retrying/dead-lettered   | Upstream health, idempotency, attempt/age; replay one confirmed item in UI                       | [Control Plane operations](../runbooks/control-plane-operations.md)                |
| Worker release regresses           | Canary signals, protocol compatibility, image digest/signature, rollback policy                  | [Worker release rollout](../runbooks/worker-release-rollout.md)                    |
| Region or dependency outage        | Readiness publication, routing authority, replication and recovery evidence                      | [Backup/recovery drill](../runbooks/backup-recovery-drill.md)                      |
| Unexpected usage/cost              | Per-Turn generations, missing vs zero telemetry, tariff/Provider currency, shared allocation     | [Administrator daily operations](../runbooks/enterprise-admin-daily-operations.md) |
| Delete/export/erasure blocked      | Active Legal Hold, retention, active Execution/Workspace and immutable dependencies              | [Administrator daily operations](../runbooks/enterprise-admin-daily-operations.md) |

## Safe interventions

Use versioned product operations: revoke a Session, Credential, Worker or Support grant; retry a confirmed idempotent Outbox
message; drain/rollback a Worker release; or invoke the documented recovery/rotation command with its approval reference.
Preserve evidence before restart, failover, replay or revocation.

Never manually advance event sequence, Lease generation, migration checksum, Outbox published state, lifecycle version,
privacy state or immutable evidence receipts. Never disable Tenant predicates, signature checks or fail-closed admission to
make a request succeed. If recovery appears to require one of these actions, stop and escalate it as an incident/change.

## Escalation packet

Send the owner a redacted packet containing scope, timeline, candidate digests, Region/Target, request and trace IDs, relevant
Audit references, the verified Support diagnostic snapshot when available, metric query/window, safe log excerpts, actions
already taken, current internal user impact and next decision.
State separately what is observed in production, reproduced in a production-like environment, or only inferred from source.

SEV-0/1 events enter the [incident-response runbook](../runbooks/enterprise-incident-response.md), including paging, Status
Page timing and recovery observation. A successful rollback does not erase a security, data-integrity or isolation incident.

# Stage 4 OrbStack Resource Lifecycle E3 — Final 2

Date: 2026-07-27 (Asia/Shanghai)
Result: **PASS**
Evidence class: **E3 local real-Kubernetes/PostgreSQL**

This run proves the Stage 4 `waiting-for-approval` lifecycle path against the real local OrbStack Kubernetes API,
kubelet, PostgreSQL, MinIO, two Control Plane replicas, and two distinct physical Worker Pods. It does not claim a
managed-cloud or production E4 result.

## Provenance

- Git HEAD observed after the run: `82a64f8012ff696433684910d00448475e12d427`.
- The worktree was dirty; the commit alone is not source provenance. Relevant source digests are recorded below.
- Kubernetes server and kubelet: `v1.34.8+orb1`.
- Node/container runtime: `orbstack`, `docker://29.4.0`.
- Control Plane image: `synara-control-plane:stage4-session-authority-20260726`,
  `sha256:3db485ea86d0701580ed6f3a3ca599cd62b05844258301fc582095449ab13f95`.
- Worker image: `synara-worker-acceptance:stage4-lifecycle-20260727`,
  `sha256:714a8f026c883e350b7ce12bca0738a1a15856204684802db0200b04b4a63f05`.
- The fixture bundled in that Worker image has SHA-256
  `a87229592e40d08ea8c9dcbe815ca67f37339371fa650ee5e9f223e03b38da37`.

The final run used fresh, acceptance-owned namespaces and a dedicated ClusterRole/Binding. The existing
`synara-system` deployment was not used as the test baseline and remained `2/2` Ready.

## Final run

Authoritative evidence:
[`stage-4-orbstack-resource-lifecycle-20260727-final2.json`](stage-4-orbstack-resource-lifecycle-20260727-final2.json)

The parent resilience runner started from an absent namespace, bootstrapped the isolated Stage 2 dependencies, ran the
baseline failure and RBAC checks, then invoked the `resource-lifecycle` case.

| Check | Result |
| --- | --- |
| Overall run | PASS, 248 seconds |
| Fresh baseline | PASS, 124 seconds |
| PostgreSQL outage and recovery | PASS |
| MinIO outage and recovery | PASS |
| Two-replica Control Plane replacement/readiness | PASS |
| Sensitive-log audit | PASS |
| Least-privilege RBAC spot audit | PASS |
| Resource lifecycle case | PASS, E3, 102 seconds |

## Lifecycle proof

The case first completed an artifact-producing Turn and persisted a ready Workspace Checkpoint. A second Turn then
entered `waiting-for-approval` on Generation 1 with one durable pending approval.

- Frozen `waitingKeepAliveSeconds`: `60`.
- Generation 1 reached `kubernetes-pod-terminal-v1` completion with Pod phase `Succeeded` and agentd exit zero.
- The Worker Lease count was zero at the lease-free `suspended` boundary.
- The exact Generation 1 Pod UID was absent before recovery was permitted.
- The approval was resolved while suspended and stored as `resume-recorded`; no callback was replayed into the fenced
  Generation 1 Provider.
- Resolution triggered Generation 2 and a different physical Pod UID.
- Generation 2 restored the Workspace Checkpoint, verified the original artifact contents, and completed the same
  Session/Turn. The final Session resource state was `idle`.

Only SHA-256 digests of Target, Session, Execution, interaction, Pod name, and Pod UIDs are retained in the report.
The final evidence contains no raw UUID and no credential-, token-, password-, or secret-valued field.

## Recovery Bundle proof

Exactly two immutable Bundles were observed:

1. Generation 1 with `recoveryReason=initial-claim`.
2. Generation 2 with `recoveryReason=suspend-resume` and `previousBundleId` equal to the first Bundle.

The resumed Bundle passed strict checks for:

- Execution, Session, Generation, Kubernetes Target, and Scheduling Decision identity;
- Provider configuration and current Turn input;
- authoritative conversation/interaction snapshot;
- explicit Memory references field;
- ready Workspace Checkpoint identity;
- the exact accepted approval resolution; and
- canonical SHA-256 payload digests for both Bundles.

The replacement Worker emitted one verified recovery-evidence Event with `artifactContentVerified=true`.

## Repeatability and negative evidence

A retained-baseline debug run (`debug4`) passed the same physical-Pod replacement and Bundle checks in 94 seconds
before the clean final run. An earlier `debug3` attempt observed an HTTP 409 while creating the second Turn; that run's
original harness incorrectly read a top-level `code` instead of the Control Plane's `error.code`, so the exact problem
code was not preserved. The parser now records the bounded operation name and nested stable problem code. No product
source change was required for the two subsequent passes, so the earlier 409 is retained as negative evidence rather
than assigned an unsupported root cause.

Earlier `final1` also remains negative harness evidence: PostgreSQL JSON parsing included `psql` transaction command
tags. The runner now uses quiet tuples-only output. These failed reports are not counted as acceptance passes.

## Verification

- Provider Host fixture: `20/20` tests passed.
- Resource lifecycle runner: `9/9` tests passed, including Bundle target scope, problem-envelope parsing, cleanup
  ownership/UID preconditions, and failure-report redaction.
- Complete resilience asset validation: PASS (`24` existing Python tests, `9` lifecycle tests, shell/static/fake-cluster
  validation).
- The clean final run used the checked lifecycle script and automatically removed its Worker namespace with an exact
  namespace UID precondition.

Relevant source SHA-256 values:

| File | SHA-256 |
| --- | --- |
| `deploy/kubernetes/resource-lifecycle-acceptance.py` | `91ffcf86cafdac9d64f54db77ad1f9cfe2e63259154b44b14277211e03b235e0` |
| `deploy/kubernetes/test_resource_lifecycle_acceptance.py` | `4890f81fc176146d58b2e55aacbd6e65485312bd5cee400c4bb3ebbaee0a2171` |
| `deploy/kubernetes/resilience-acceptance.sh` | `6c2034e77fb15fd199406e2ad7635ab9dd3201e360cd16f8ffde39e4c69a5c59` |
| `scripts/stage3-provider-acceptance/provider-host-fixture.ts` | `a092ba770f880fcffea5a77ec337bb8bf0e84d9d17a42d9bc5951ca3380a156e` |
| `scripts/stage3-provider-acceptance/provider-host-fixture.test.ts` | `f3073d8d16822ce7d839c1c48a9c3cbdbd222f877a29dbc9e86c021d00eaff9a` |
| Final JSON | `6fa4ab923a6f5de1bfcfc930ed7ac0813049cda4348c2101fedd839aa6404220` |

## Cleanup

After the final report was emitted, all run-owned resources were confirmed absent:

- `namespace/synara-stage4-lifecycle-final2`;
- `namespace/synara-stage4-worker-lifecycle-final2`;
- `clusterrole/synara-control-plane-reconciler-synara-stage4-lifecycle-final2`; and
- the matching ClusterRoleBinding.

## Evidence boundary

This closes the local E3 gap for real kubelet-backed `Suspend -> Pod termination -> resolve while suspended -> new
Generation resume`, including atomic recovery state consumption. OrbStack is still one local node. The result does not
prove managed-cloud multi-AZ behavior, production-duration soak, real cross-Region backing-store replication, external
PostgreSQL failover, cloud Workload Identity, or real AWS/GCP/Azure billing exports. Those Stage 4 E4 gates remain open.

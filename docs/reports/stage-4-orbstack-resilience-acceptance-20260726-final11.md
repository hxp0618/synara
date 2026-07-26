# Stage 4 OrbStack Kubernetes resilience acceptance — 2026-07-26 final11

This run supersedes final10 for the current dirty Stage 4 worktree. It validates the exact Control Plane image that
contains SSH operation fencing and bootstrap authority Migrations `000071`–`000073`, after the separate real-SSH
final4 lane proved the corresponding install, restart, recovery, and revoke behavior.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T20:19:33Z` to `2026-07-25T20:21:18Z`
- Total duration: `105` seconds
- Baseline: skipped; the retained deployment was upgraded and inspected before the run
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, local single-node OrbStack cluster
- Planned cases: `rbac`, `leader-takeover`, `control-plane-failover`
- Case totals: `passed=3`, `failed=0`, `skipped=0`
- Final evidence:
  [stage-4-orbstack-resilience-acceptance-20260726-final11.json](stage-4-orbstack-resilience-acceptance-20260726-final11.json)
- Progress journal:
  [stage-4-orbstack-resilience-acceptance-20260726-final11.json.journal.jsonl](stage-4-orbstack-resilience-acceptance-20260726-final11.json.journal.jsonl)
- Atomic progress snapshot:
  [stage-4-orbstack-resilience-acceptance-20260726-final11.json.partial.json](stage-4-orbstack-resilience-acceptance-20260726-final11.json.partial.json)
- Paired real-SSH evidence:
  [final4 acceptance report](stage-4-ssh-atomic-ready-acceptance-20260726-final4/acceptance-report.md)
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-orbstack-final11-20260726`
- Test image ID: `sha256:9ff3e4dafcc67e318ed4c7f11566115edab370ca659fcfeb7d8fec59d3c5bef5`

This is local real-kubelet evidence from an uncommitted worktree. It is not a pushed artifact, managed-cloud
deployment, or production release gate.

## Frozen acceptance inputs

| Asset                                                                          | SHA-256                                                            |
| ------------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| `deploy/kubernetes/resilience-acceptance.sh`                                   | `c2171e0f0fcef6ae1c6d826e29d4df326db6a4478a7882b012b0fab086fded63` |
| `deploy/kubernetes/validate-resilience-assets.py`                              | `75e666fe4f8e7ff607acd4d25c1978c3c165316fa068b35a85419c1bc4c6e5f4` |
| `services/control-plane/Dockerfile`                                            | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |
| `services/control-plane/go.mod`                                                | `65c8f3ad8b88e3730fa2ad79b288178e057bec34f90afa1435cb7dbb0e3dde47` |
| `services/control-plane/migrations/000071_ssh_target_operation_fencing.sql`    | `db979123f806bcab43ce3fd2997330932052ff83b3fd7e93c9505bc45cfabb39` |
| `services/control-plane/migrations/000072_ssh_bootstrap_authority.sql`         | `975171705f3f8af8f6fe214589f9bd6a9166dbd0403ef9276672640c97fb64ce` |
| `services/control-plane/migrations/000073_ssh_worker_bootstrap_generation.sql` | `7aa8fe173a3faba337093eae88166d2efe4d4e1306c2d73045f4fc0882dbb2da` |
| `services/control-plane/internal/executiontargets/ssh_provisioner.go`          | `691d03aed54f6f97ae0edc6a40886cd4bda9e97f9e2b80db3199573bedd6c3b4` |
| `services/control-plane/internal/executions/service.go`                        | `ced4fd11f0a025d7b89fb4956ce076ae620aee3fb6ee2f593ff49bcd523627f4` |
| `services/control-plane/internal/executions/worker_revocation.go`              | `1d99cb3349cb100e74dc5bf3b85402e7e25fcb982d852d71784b8c172a8a63c6` |
| `scripts/stage3-provider-acceptance/acceptance_runner.py`                      | `16d24a0545ffda6b21594a517982da3ba8481a6b31069071ea14f52de334c187` |
| SSH final4 JSON evidence                                                       | `ce39688cf2706505577055d254bca92e581e680d7ee15e82e5ad720efff9ec01` |
| Final11 JSON evidence                                                          | `61f409fe76e075c038b8a060e9685a1d0f019c4beeb01dc454d16d27202a976b` |
| JSONL progress journal                                                         | `190f1000499c6a4320e596c2169c7566f9848d9668009a05d38831ba5579d05d` |
| Atomic progress snapshot                                                       | `c58c63e568430cc648b7fff58c9e6409b1d7359784c9cf26547044b738289499` |

The hashes identify the dirty files used for this local run; they do not make the uncommitted source tree
reconstructible. The exact final11 image remains in OrbStack for follow-up inspection.

## Rollout and schema evidence

- A PostgreSQL custom-format backup was created, checked with the PostgreSQL container's `pg_restore --list`, and
  restricted to mode `0600` before the image rollout. It is a local emergency artifact and is intentionally not stored
  in the repository.
- The Deployment rolled from final10 to final11 with `2/2` Ready replicas. After the old replica terminated, both
  replicas used the exact final11 image ID and had zero restarts.
- `/ready` converged from the old binary's `expectedVersion=70, appliedVersion=73` to
  `expectedVersion=73, appliedVersion=73`.
- Migration ledger rows `71`, `72`, and `73` exactly matched the frozen SQL hashes above.
- PostgreSQL exposed the Target operation generation/kind/start/expected-instance columns and the Worker persisted
  `ssh_bootstrap_generation` column required by the final SSH authority contract.

## Resilience evidence

- All `28/28` RBAC expectations matched: `21` required permissions were allowed and `7` prohibited permissions were
  denied.
- The exact reconciler leader Pod was deleted. The holder changed from
  `synara-control-plane-c5bb4b89d-gm28k` to `synara-control-plane-c5bb4b89d-rr5gm`, the fencing token advanced from
  `16` to `17`, and the scenario recorded `0` readiness probe failures.
- The independent Control Plane failover case deleted `synara-control-plane-c5bb4b89d-wsddt` and observed
  `synara-control-plane-c5bb4b89d-n4sfm` as its replacement, again with `0` readiness probe failures.
- Final live inspection found two final11 replicas Ready with zero restarts, `/ready` healthy, and one unexpired
  database-authoritative reconciler lease at fencing token `17`.

## Paired real-SSH final4 evidence

The real-SSH fixture lane created an owned disposable OrbStack Ubuntu 24.04 VM and passed `16/16` cases through the
real Control Plane, agentd, Worker Protocol, SSH provisioning, systemd restart, and recovery paths:

- SSH Target creation remained `offline`; activation required a post-registration heartbeat, compatible current
  Manifest, and an exact persisted Worker bootstrap generation equal to the Target operation generation.
- SSH upgrade kept the logical Worker ID stable, advanced incarnation `1 -> 2`, changed the physical instance UID,
  and preserved remote-filesystem Workspace continuity.
- Revoke left the Target `disabled`, revoked the Worker, removed every Execution and Workspace-cleanup lease, and
  reported `liveWorkerAuthorities=0`.
- The owned VM, state, and local key material were removed. The secret scan covered `13` report/log files and found
  no known or high-confidence secret pattern.

The fixture uses a deterministic Provider Host rather than a real Codex App Server or Claude Agent SDK. It proves the
product SSH control path locally, not production KMS availability, real-host containment, or abrupt host-power-loss
recovery.

## Warm-pool continuity

The retained warm namespace was outside the resilience harness mutation scope. Pod
`synara-warm-3a5601e801-v1-unmanaged-s0` retained UID
`9eb6984a-b500-4676-80f5-ca66e80d95dd`, remained Running and Ready, and stayed at zero restarts through the final11
rollout and both disruption cases.

## Post-run local fixture hygiene

Final log inspection found nine historical PostgreSQL integration-test Targets repeatedly reporting missing
Kubernetes configuration. Each row was independently verified as `Execution test tenant` / `exec-*` / `test-target`,
`offline`, with a zero-length configuration and no corresponding Kubernetes resource. An exact UUID-guarded
transaction changed only those nine rows to the recoverable `disabled` state; no Tenant or history row was deleted.
The active Reconciler then stopped emitting the missing-configuration error and `/ready` remained healthy.

## Evidence boundary

This pass proves local real-kubelet RBAC, two-replica PostgreSQL-backed leader fencing, exact leader takeover, Control
Plane Pod replacement, schema-73 readiness, warm-pool continuity, and the paired local real-SSH lifecycle. OrbStack is
single-node: the Kubernetes control plane, both application replicas, PostgreSQL, MinIO, networking, storage, and warm
Pod share one host. It cannot prove Node partition, multi-AZ behavior, managed-cloud Workload Identity, external
database/object-store failure, actual cross-Region data replication, cloud-billing exports, or production-duration
soak. Stage 4 therefore remains **IN PROGRESS**.

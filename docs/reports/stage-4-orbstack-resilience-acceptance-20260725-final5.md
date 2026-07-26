# Stage 4 OrbStack Kubernetes resilience acceptance — 2026-07-25 final5

This run supersedes the OrbStack final4 evidence for the current dirty Stage 4 worktree. It rebuilt the Control Plane
after Migration `000068`, exercised the durable progress journal/partial snapshot harness, and retained the acceptance
resources long enough to inspect the migrated PostgreSQL schema before exact cleanup.

## Result

- Status: **pass**
- Evidence window: `2026-07-25T13:53:32Z` to `2026-07-25T13:56:31Z`
- Total duration: `179` seconds
- Baseline duration: `69` seconds
- Kubernetes context: `orbstack`
- Kubernetes server: `v1.34.8+orb1`, local single-node OrbStack cluster
- Planned cases: `rbac`, `leader-takeover`, `control-plane-failover`
- Case totals: `passed=3`, `failed=0`, `skipped=0`
- Final evidence: [stage-4-orbstack-resilience-acceptance-20260725-final5.json](stage-4-orbstack-resilience-acceptance-20260725-final5.json)
- Progress journal: [stage-4-orbstack-resilience-acceptance-20260725-final5.json.journal.jsonl](stage-4-orbstack-resilience-acceptance-20260725-final5.json.journal.jsonl)
- Atomic progress snapshot: [stage-4-orbstack-resilience-acceptance-20260725-final5.json.partial.json](stage-4-orbstack-resilience-acceptance-20260725-final5.json.partial.json)
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Test image: `synara-control-plane:stage4-orbstack-final5-20260725`
- Test image ID: `sha256:ad949f2bda824351ef724dc663cfd0bd84134d6651387a1e9c5fdde1c0293775`

This is local real-kubelet evidence from an uncommitted worktree. It is not a pushed artifact, managed-cloud
deployment, or production release gate.

## Frozen acceptance inputs

| Asset                                                             | SHA-256                                                            |
| ----------------------------------------------------------------- | ------------------------------------------------------------------ |
| `deploy/kubernetes/acceptance.sh`                                 | `476b38842dc09d416b4abbebb30e12916c06270f936bf22402b07faa96c6d3ac` |
| `deploy/kubernetes/resilience-acceptance.sh`                      | `2ac6b34b45ae6963defb9ac5a1ebd5bbc07d54770466c24b9bbd40768f8c54e5` |
| `deploy/kubernetes/validate-resilience-assets.py`                 | `9c4b0de9ac95ee986fc49d28e803e9941d5dc01b05341dafe2b67de62dfe64f2` |
| `services/control-plane/Dockerfile`                               | `cd8747438e2b1ef24bbcaf23f58a4cd589e5e89776f319052fd8ee3afe561a4c` |
| `services/control-plane/migrations/000068_worker_claim_facts.sql` | `876614d31c087bdc1081e38d159c35cd4293324120a590578821b6be177ee09d` |
| Final JSON evidence                                               | `46782327e1bd43ae8d4e264fb50242acc84e09fccf80fa04b6d2b58ae50e7d96` |
| JSONL progress journal                                            | `ff11ec7a58fadbdd30f0781eafe81c9a40ce23b8553df51aea992d7739d6c68d` |
| Atomic progress snapshot                                          | `e774f94e336eb52b72caa8612117e839333c93d526e36568924366c26c838e5a` |

The hashes were captured after the proof and before exact test-image cleanup. The acceptance inputs were unchanged
during the run.

After this frozen run, a validator-only high-load replay exposed that managed partition start and stop hooks shared one
artificially short timeout. The current harness now supports independent start/stop timeout overrides with the original
timeout as a compatibility fallback. That post-run hardening is intentionally not represented by the frozen hashes or
test image above and does not add a managed-cloud validation claim.

## Baseline and resilience evidence

- PostgreSQL, MinIO, bucket initialization, and two Control Plane replicas became Ready.
- The `/ready` schema check reported `expectedVersion=68` and `appliedVersion=68`.
- A normal Control Plane Pod replacement caused `0` readiness probe failures, and the registered Worker token remained
  valid.
- Complete PostgreSQL and MinIO outages recovered.
- Sensitive generated credentials were absent from the Control Plane log audit.
- All `28/28` RBAC expectations matched: `21` required permissions were allowed and `7` prohibited permissions were
  denied.
- The active `synara:kubernetes-execution-reconciler` holder changed from
  `synara-control-plane-64cccc5d5b-779c8` to `synara-control-plane-64cccc5d5b-9lkfv` after the exact holder Pod was
  deleted.
- The reconciler fencing token advanced exactly once, from `3` to `4`; leader takeover recorded `0` readiness probe
  failures.
- The independent Control Plane failover case replaced `synara-control-plane-64cccc5d5b-px9zs` with
  `synara-control-plane-64cccc5d5b-n5rkp` and recorded `0` readiness probe failures.
- `unexpectedExitStatus` remained `null`; the final atomic snapshot also recorded `status=passed` and the same `3/0/0`
  case totals.

## Migration 68 inspection

The retained PostgreSQL deployment was inspected before cleanup. It reported:

- schema migration row `68|worker_claim_facts`;
- table `worker_claim_facts`;
- billing and request evidence indexes;
- business uniqueness indexes `uq_worker_claim_facts_execution_generation` and
  `uq_worker_claim_facts_cleanup_dispatch`;
- scope and immutability triggers `trg_worker_claim_facts_scope` and `trg_worker_claim_facts_immutable`.

Before the Kubernetes run, both same-request claim concurrency regressions passed against a disposable real
PostgreSQL instance. Each path produced one original response, one replay, one claim-count increment, one ledger row,
and one receipt. The full Control Plane Go suite and the Kubernetes resilience asset validator also passed.

## Cleanup

- The gate-owned `synara-system` Namespace was verified absent after asynchronous deletion completed.
- The gate-owned reconciler ClusterRole and ClusterRoleBinding were verified absent.
- The exact final5 test image tag and image ID were removed.
- The OrbStack Kubernetes node remained Ready.
- The reusable OrbStack Kubernetes cluster was preserved; no broad Docker or Kubernetes cleanup was used.

## Evidence boundary

This pass proves the current two-replica manifests, RBAC contract, database-backed leader lease fencing, exact leader
takeover, Pod replacement, dependency outage recovery, Migration 68 application, durable harness evidence, and cleanup
paths against OrbStack's real local Kubernetes API and kubelet. OrbStack is single-node, so it cannot prove cross-node
placement, drain, partition, or a meaningful long soak. The paired disposable Kind final4 report covers local
multi-node disruption and a 601-second soak. Managed-cloud multi-AZ behavior, real workload identity, external billing
exports, and production-duration soak remain external deployment gates.

# Stage 4 OrbStack isolated resilience — schema 87 final1

Date: 2026-07-27
Repository branch: `codex/saas-tenancy-user`
HEAD observed at build and verification: `79a9067a2b92d91e70c76023c55fa8761d128064`
Evidence class: **E3 local real-Kubernetes runtime**
Kubernetes context: explicit `orbstack` (`v1.34.8+orb1`)
Node: one Ready OrbStack control-plane node

## Result

The current dirty-worktree Control Plane image passed an ownership-isolated, real-kubelet resilience run at schema 87.
The runner created a fresh namespace and exact owner-labelled ClusterRole/Binding, deployed two Control Plane replicas
with owned PostgreSQL and MinIO dependencies, ran three required top-level cases, and completed a bounded 120-second
disruption soak.

```text
status:                    passed
total duration:            253 seconds
baseline:                  passed in 94 seconds
top-level cases:           3 passed / 0 failed / 0 skipped
configured soak duration:  120 seconds
actual soak duration:      132 seconds
soak cycles:               8 passed
idle readiness failures:   0
required skipped cycles:   0
schema:                     87
```

## Current-source image

```text
tag:       synara-control-plane:stage4-orbstack-schema87-final1-20260727
image ID:  sha256:206455e4d00754c42c18c6710801b269cfcf8f2c44b56374f32c1442f6755856
revision:  79a9067a2b92d91e70c76023c55fa8761d128064
labels:    synara.io/acceptance=stage4-orbstack-schema87-final1
           synara.io/source-worktree=dirty-verification
```

The image was built directly from the current dirty worktree after combining the fast-cloud merge with the preserved
Stage 4 changes. The revision is a base-HEAD identity, not proof of a clean or production-approved image.

## Baseline

The baseline proved against the real OrbStack API and kubelet:

- two Ready Control Plane replicas with `/ready` and PostgreSQL both reporting all 87 migrations;
- one Control Plane Pod deletion with no readiness interruption;
- the registered Worker token remained valid after replacement;
- PostgreSQL outage and readiness recovery;
- MinIO outage and readiness recovery;
- bounded sensitive-log audit; and
- the least-privilege Kubernetes RBAC contract.

The schema tail included Migration `000086_worker_reconciliation_drains.sql` and
`000087_worker_pool_min_idle.sql`, so this is current-schema evidence rather than a replay of the earlier schema-81
report.

## Top-level cases

### RBAC

All 28 allow/deny checks passed. The exact isolated ServiceAccount could create TokenReview, get/create/patch Namespace,
and get/list/create/patch/delete Pods, plus the required get/create/patch operations for ServiceAccount, Secret,
ResourceQuota, and NetworkPolicy. It was denied Namespace deletion, destructive deletion of those supporting resources,
Pod update, and Pod watch.

### Leader takeover

The exact active holder Pod UID was deleted with a Kubernetes UID precondition while the same nonce-bound PostgreSQL
guard session held the advisory lock. The holder changed and the fencing token advanced exactly `3 -> 4`; readiness
failures remained zero.

### Control Plane failover and Session authority

One selected Control Plane Pod was deleted and replaced while the peer stayed ready. The PostgreSQL-backed Session
sentinel had exactly one row and the canonical before/after row digest was byte-identical:

```text
before = 0d0aae1e0c573944b592ff21edf6044b3420f90ae6ac86b8f60c137d3190f17a
after  = 0d0aae1e0c573944b592ff21edf6044b3420f90ae6ac86b8f60c137d3190f17a
rowCount = 1
readiness failures = 0
```

## Soak

The configured 120-second window completed in 132 seconds because the final in-flight disruption was allowed to finish.
Eight of eight cycles passed, alternating four `leader-takeover` and four `control-plane-failover` cases.

The guarded leader transitions were `4 -> 5`, `6 -> 7`, `8 -> 9`, and `9 -> 10`; no fencing token rolled back or was
reused. Every leader and Pod failover cycle recorded zero readiness failures. Each failover cycle created a fresh
PostgreSQL Session sentinel, retained exactly one row, and produced an identical before/after canonical digest.

## Commands

Static safety validation:

```text
bash -n deploy/kubernetes/acceptance.sh deploy/kubernetes/resilience-acceptance.sh
bash deploy/kubernetes/validate-resilience-assets.sh
24 tests passed
9 tests passed
Kubernetes resilience assets validation passed
```

Runtime lane:

```text
SYNARA_K8S_CONTEXT=orbstack \
SYNARA_K8S_NAMESPACE=synara-stage4-schema87-final1 \
SYNARA_K8S_ACCEPTANCE_RBAC_NAME=synara-control-plane-reconciler-synara-stage4-schema87-final1 \
SYNARA_K8S_ACCEPTANCE_OWNER=stage4-schema87-final1 \
SYNARA_K8S_ACCEPTANCE_IMAGE=synara-control-plane:stage4-orbstack-schema87-final1-20260727 \
SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1 \
SYNARA_K8S_RESILIENCE_CASES=rbac,leader-takeover,control-plane-failover \
SYNARA_K8S_RESILIENCE_SOAK_SECONDS=120 \
SYNARA_K8S_RESILIENCE_SOAK_CASES=leader-takeover,control-plane-failover \
SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS=5 \
SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS=15 \
SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS=1 \
SYNARA_K8S_RESILIENCE_EVIDENCE_FILE=docs/reports/stage-4-orbstack-isolated-schema87-20260727-final1.json \
bash deploy/kubernetes/resilience-acceptance.sh
```

## Evidence hashes

| Artifact | SHA-256 |
| --- | --- |
| `stage-4-orbstack-isolated-schema87-20260727-final1.json` | `7ebbf83d04a599e6730b7cacf4bab8a29bb801319111a0809ed93e117fa0315d` |
| `stage-4-orbstack-isolated-schema87-20260727-final1.json.journal.jsonl` | `4e3553f3c41146a053dc738baa2542d45e03eb059ae9e118c809de950770101e` |
| `stage-4-orbstack-isolated-schema87-20260727-final1.json.partial.json` | `4a093366e6527b70784660aba3a6a128f4807b387c747e72b30b4369cb3fc1b0` |
| `deploy/kubernetes/acceptance.sh` | `fda9401340eb1e71c2b0206676d0cd066131be33ae8d58a474f1a32944041780` |
| `deploy/kubernetes/resilience-acceptance.sh` | `6c2034e77fb15fd199406e2ad7635ab9dd3201e360cd16f8ffde39e4c69a5c59` |
| `deploy/kubernetes/validate-resilience-assets.py` | `02a9c4d63eb70e33c91de7f478587af2872c0bf6b45b42a6db6e9953b8b2cb28` |
| `migrations/000086_worker_reconciliation_drains.sql` | `5a64b0ab6a921dfd8773cbd53f1b343a75cb1f6def372547ff19367979ebc411` |
| `migrations/000087_worker_pool_min_idle.sql` | `29ab98cf37b830146a5013ff62163b875ab714e8d4788b9dc23ab6977f94aefe` |

## Cleanup proof

The exact acceptance namespace, ClusterRole, and ClusterRoleBinding were deleted and verified absent. The locally built
acceptance image tag and image ID were removed. The pre-existing `synara-system/synara-control-plane` deployment remained
`2/2` Ready and Available on its original image. The pre-existing `synara-stage4-worker-lifecycle-debug2`,
`synara-stage4-worker-lifecycle-debug3`, and `synara-warm-final6b` namespaces remained Active and were not modified.

## Boundary

OrbStack is a real Kubernetes API/kubelet/runtime path, but it has one physical node and one local failure domain. This
run proves current-schema local RBAC, multi-replica handoff, Session authority continuity, repeated Pod disruption, and
bounded soak. It deliberately does **not** claim a real multi-node Node partition, cloud node-controller behavior,
multi-AZ scheduling, managed load balancer/storage behavior, cross-Region data-plane recovery, managed-cloud identity,
or production-duration soak. Those remain E4 gates and require a disposable multi-node or managed cluster.

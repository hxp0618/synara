# Stage 4 OrbStack Isolated Resilience E3 — Final 2

Date: 2026-07-26  
Evidence class: `E3 local-runtime`  
Kubernetes context: `orbstack` (`v1.34.8+orb1`)  
Isolated namespace: `synara-stage4-isolated-final2`  
Isolated ClusterRole/Binding: `synara-control-plane-reconciler-synara-stage4-isolated-final2`

## Result

The schema-81 Control Plane image passed a namespace-, RBAC-, and ownership-isolated two-replica resilience run without
modifying the existing `synara-system` deployment. The final evidence recorded `status=passed`, total duration 306
seconds, a 138-second baseline, three passed top-level cases, and a 121-second bounded soak with six passed disruption
cycles.

The reused immutable image was:

```text
synara-control-plane:stage4-orbstack-isolated-final1-20260726
sha256:c3d3b6707331ab0f3f2aa19da0bcfebbe49575379e215807e26c159b909e3d7f
```

## Ownership safety change

This second run validates the final ownership-safe runner, not only namespace/RBAC parameterization:

- `acceptance.sh` atomically creates a fresh `synara-*` namespace and refuses an existing live namespace;
- it refuses an existing selected ClusterRole or ClusterRoleBinding instead of applying over it;
- the namespace, ClusterRole, and ClusterRoleBinding carry the exact `synara.ai/acceptance-owner` label;
- `resilience-acceptance.sh` deletes an identity only when its current owner label exactly matches this run;
- cleanup never infers ownership merely from a caller-selected name;
- startup waits for an asynchronously terminating namespace before attempting a fresh create.

The checked-in fake-`kubectl` regression matrix separately proves that both a pre-existing namespace collision and a
pre-existing RBAC collision fail without overwrite or delete. Those are static safety tests; this runtime run proves the
positive owned-resource path against the real OrbStack API.

## Baseline

The 138-second baseline proved:

- two ready Control Plane replicas on schema 81;
- one Control Plane Pod deletion with zero readiness failures;
- the registered Worker token remained valid after replacement;
- PostgreSQL outage/readiness recovery;
- MinIO outage/readiness recovery;
- bounded sensitive-log audit;
- 28 least-privilege RBAC allow/deny checks against the isolated ClusterRole.

## Top-level cases

| Case                   | Result | Key evidence                                                                              |
| ---------------------- | ------ | ----------------------------------------------------------------------------------------- |
| RBAC                   | passed | all 28 allow/deny checks matched the isolated ClusterRole contract                        |
| Leader takeover        | passed | exact old Pod UID deleted, holder changed, fencing token `3 -> 4`, readiness failures `0` |
| Control Plane failover | passed | deleted Pod replaced while the second replica stayed ready, readiness failures `0`        |

## Soak

Configured duration: 120 seconds  
Actual duration: 121 seconds  
Idle readiness failures: 0  
Required skipped cycles: 0

Six of six cycles passed:

- three `leader-takeover` cycles observed guarded fencing-token transitions `4 -> 5`, `5 -> 6`, and `7 -> 8`, with a
  different holder and zero readiness failures each; the intervening snapshot began at 7, so no token rollback or reuse
  occurred across the alternating failover cycle;
- three `control-plane-failover` cycles replaced the selected Pod with zero readiness failures each.

## Command

```bash
SYNARA_K8S_CONTEXT=orbstack \
SYNARA_K8S_NAMESPACE=synara-stage4-isolated-final2 \
SYNARA_K8S_ACCEPTANCE_RBAC_NAME=synara-control-plane-reconciler-synara-stage4-isolated-final2 \
SYNARA_K8S_ACCEPTANCE_OWNER=stage4-isolated-final2 \
SYNARA_K8S_ACCEPTANCE_IMAGE=synara-control-plane:stage4-orbstack-isolated-final1-20260726 \
SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1 \
SYNARA_K8S_RESILIENCE_CASES=rbac,leader-takeover,control-plane-failover \
SYNARA_K8S_RESILIENCE_SOAK_SECONDS=120 \
SYNARA_K8S_RESILIENCE_SOAK_CASES=leader-takeover,control-plane-failover \
SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS=5 \
SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS=15 \
SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS=1 \
SYNARA_K8S_RESILIENCE_EVIDENCE_FILE=docs/reports/stage-4-orbstack-isolated-resilience-20260726-final2.json \
bash deploy/kubernetes/resilience-acceptance.sh
```

Final output:

```text
Kubernetes resilience acceptance passed: context=orbstack
evidence=docs/reports/stage-4-orbstack-isolated-resilience-20260726-final2.json
cases=rbac,leader-takeover,control-plane-failover
```

The current static gate and shell syntax gate passed:

```text
bash -n deploy/kubernetes/acceptance.sh deploy/kubernetes/resilience-acceptance.sh
bash deploy/kubernetes/validate-resilience-assets.sh
Ran 24 tests in 1.834s — OK
```

## Cleanup proof

Kubernetes namespace deletion is asynchronous: the first immediate post-exit lookup could still observe the selected
namespace while deletion completed, and the following lookup returned `NotFound`. Final lookups returned no namespace,
ClusterRole, or ClusterRoleBinding for this run. The pre-existing `synara-system/synara-control-plane` deployment
remained `2/2` ready and available.

## Evidence files and hashes

| Artifact                                                                  | SHA-256                                                            |
| ------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| `stage-4-orbstack-isolated-resilience-20260726-final2.json`               | `73cb28b3705612ecc1eee9badb0b7c5b9857eab47c169dc7b263f436a75415ef` |
| `stage-4-orbstack-isolated-resilience-20260726-final2.json.journal.jsonl` | `c27e7b1c7c11c29a683b91e53a57d70d7ff80925042ba456ff885d6d07f415f6` |
| `stage-4-orbstack-isolated-resilience-20260726-final2.json.partial.json`  | `6eb77cb302369875fba32fe83a50e7b23b48a6a75e438af8fe8fd20a27d826d1` |
| `deploy/kubernetes/acceptance.sh`                                         | `fda9401340eb1e71c2b0206676d0cd066131be33ae8d58a474f1a32944041780` |
| `deploy/kubernetes/resilience-acceptance.sh`                              | `79c697b8a4eac516c19cb01710ef4b1baa4e84c494338f54447bed426609ca76` |
| `deploy/kubernetes/validate-resilience-assets.py`                         | `248dc9f24edf9343f6221af4e701a4716f3d008aa87f684917e1939eebee9331` |

## Boundary

This run materially strengthens local multi-replica, exact-ownership cleanup, and repeated-fault E3 evidence. OrbStack
still has one physical Kubernetes node, one local failure domain, and local storage/networking. It does not prove
multi-AZ scheduling, a real Node partition, cross-Region data-plane recovery, managed-cloud identity, or
production-duration soak. Those remain E4 deployment gates.

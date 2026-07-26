# Stage 4 OrbStack Isolated Resilience E3 — Final 1

Date: 2026-07-26  
Evidence class: `E3 local-runtime`  
Kubernetes context: `orbstack` (`v1.34.8+orb1`)  
Isolated namespace: `synara-stage4-isolated-final1`  
Isolated ClusterRole/Binding: `synara-control-plane-reconciler-synara-stage4-isolated-final1`

## Result

The current Control Plane image passed a namespace- and RBAC-isolated two-replica resilience run without modifying the
existing `synara-system` deployment. The final report recorded `status=passed`, total duration 228 seconds, a 78-second
baseline, three passed top-level cases, and a 120-second bounded soak with six passed disruption cycles.

The image included schema version 81:

```text
synara-control-plane:stage4-orbstack-isolated-final1-20260726
sha256:c3d3b6707331ab0f3f2aa19da0bcfebbe49575379e215807e26c159b909e3d7f
```

## Isolation change

`deploy/kubernetes/acceptance.sh` now accepts `SYNARA_K8S_NAMESPACE` and
`SYNARA_K8S_ACCEPTANCE_RBAC_NAME`. A non-default namespace derives a unique ClusterRole name when no explicit name is
provided. The runner:

- accepts only a `synara-*` DNS-label namespace;
- refuses to reuse a live namespace;
- renders namespaced resources through a temporary Kustomize overlay;
- rewrites the ClusterRole, ClusterRoleBinding, role reference, and ServiceAccount subject together;
- records `rbacName` in dry-run, journal, partial, and final evidence;
- deletes only the selected namespace and exact isolated ClusterRole/Binding.

`resilience-acceptance.sh` forwards the same namespace/RBAC identity into baseline bootstrap, reads that exact
ClusterRole during RBAC acceptance, and cleans the same exact resources. This removes the previous reason that an
OrbStack run had to reuse or destroy the fixed `synara-system` baseline.

## Baseline

The 78-second baseline proved:

- two ready Control Plane replicas on schema 81;
- one Control Plane Pod deletion with zero readiness failures;
- the registered Worker token remained valid after replacement;
- PostgreSQL outage/readiness recovery;
- MinIO outage/readiness recovery;
- bounded sensitive-log audit;
- least-privilege RBAC spot audit.

## Top-level cases

| Case | Result | Key evidence |
| --- | --- | --- |
| RBAC | passed | 28 allow/deny checks against the isolated ClusterRole |
| Leader takeover | passed | exact old Pod UID deleted, holder changed, fencing token `3 -> 4`, readiness failures `0` |
| Control Plane failover | passed | deleted Pod replaced while the second replica stayed ready, readiness failures `0` |

## Soak

Configured duration: 120 seconds  
Actual duration: 120 seconds  
Idle readiness failures: 0  
Required skipped cycles: 0

Six of six cycles passed:

- three `leader-takeover` cycles advanced the fencing token `4 -> 5 -> 6 -> 7` with a different holder and zero
  readiness failures each;
- three `control-plane-failover` cycles replaced the selected Pod with zero readiness failures each.

## Command

```bash
SYNARA_K8S_CONTEXT=orbstack \
SYNARA_K8S_NAMESPACE=synara-stage4-isolated-final1 \
SYNARA_K8S_ACCEPTANCE_IMAGE=synara-control-plane:stage4-orbstack-isolated-final1-20260726 \
SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1 \
SYNARA_K8S_RESILIENCE_CASES=rbac,leader-takeover,control-plane-failover \
SYNARA_K8S_RESILIENCE_SOAK_SECONDS=120 \
SYNARA_K8S_RESILIENCE_SOAK_CASES=leader-takeover,control-plane-failover \
SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS=5 \
SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS=15 \
SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS=1 \
SYNARA_K8S_RESILIENCE_EVIDENCE_FILE=docs/reports/stage-4-orbstack-isolated-resilience-20260726-final1.json \
bash deploy/kubernetes/resilience-acceptance.sh
```

Final output:

```text
Kubernetes resilience acceptance passed: context=orbstack
evidence=docs/reports/stage-4-orbstack-isolated-resilience-20260726-final1.json
cases=rbac,leader-takeover,control-plane-failover
```

The checked-in static gate passed after the isolation change:

```text
bash deploy/kubernetes/validate-resilience-assets.sh
Ran 24 tests in 1.719s — OK
Python resilience validation passed
Kubernetes resilience assets validation passed
```

## Cleanup proof

Final lookups returned no namespace, ClusterRole, or ClusterRoleBinding for the isolated run. The pre-existing
`synara-system/synara-control-plane` deployment remained `2/2` ready.

## Evidence files and hashes

| Artifact | SHA-256 |
| --- | --- |
| `stage-4-orbstack-isolated-resilience-20260726-final1.json` | `41b709c2340c5debfe5a3dcc583d0c7d1c857b2ad5c1666c00ab36b6e0458e95` |
| `stage-4-orbstack-isolated-resilience-20260726-final1.json.journal.jsonl` | `6e98452f63ad3e23e7e5e960a044d9cb111446da82ebf717e84649d70d2725b0` |
| `stage-4-orbstack-isolated-resilience-20260726-final1.json.partial.json` | `31638cbdc7e032f080098461ffd71157d66b9e363935bb6718b73c64d9b33f30` |
| `deploy/kubernetes/acceptance.sh` | `914a309491fc38bd3e5fe87b230c8f3baf22a50c369b7ef0d2370088a89e1fd7` |
| `deploy/kubernetes/resilience-acceptance.sh` | `19949ebb6a95be40198eb5d97999e4562c040a8c42ec9ad83e38a7e5ce213f47` |
| `deploy/kubernetes/validate-resilience-assets.py` | `3f258ad39277d03bff9b8092a764768a023eff4bccdc8da1002ea2d50b2ab87d` |

## Boundary

This run materially strengthens local multi-replica and repeated-fault E3 evidence. OrbStack still has one physical
Kubernetes node, one local failure domain, and local storage/networking. It does not prove multi-AZ scheduling, a real
Node partition, cross-Region data-plane recovery, managed-cloud identity, or production-duration soak. Those remain E4
deployment gates.

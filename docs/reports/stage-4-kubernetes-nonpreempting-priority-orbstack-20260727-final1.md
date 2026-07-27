# Stage 4 Kubernetes non-preempting Priority — OrbStack final1

Date: 2026-07-27 (Asia/Shanghai)

Status: PASS for the bounded local E3 claim below. This report is immutable once generated.

## Source identity and scope

- Branch: `codex/saas-tenancy-user`
- HEAD: `79a9067a2b92d91e70c76023c55fa8761d128064`
- The worktree was intentionally dirty with 140 status entries before this report was created. This evidence validates
  the current working-tree source, not only HEAD, and does not claim any commit, push, PR, or deployment.
- In scope: Kubernetes Worker Pool scheduling-template validation, default/custom PriorityClass authority, cold and
  warm PodSpec construction, fail-closed Reconciler validation, least-privilege PriorityClass RBAC, deployment asset,
  real API admission, and real kubelet execution.
- Out of scope: real agentd registration/claim, PostgreSQL scheduling state, scheduler contention between workloads,
  managed-cloud behavior, multi-node failure, and production-duration soak.

## Implemented authority

1. Pool create/update accepts Kubernetes priority only as non-preempting scheduler order. An explicit
   `preemptionPolicy` accepts only canonical `Never`; `PreemptLowerPriority` is rejected at write time and Pod build.
2. Every execution-pinned and warm Pod uses `preemptionPolicy=Never` and a PriorityClass. An omitted custom class maps
   to the pre-created `synara-worker-nonpreempting-v1` class.
3. The default deployment asset freezes value `0`, `globalDefault=false`, and `preemptionPolicy=Never`. The Target
   credential receives only cluster-scoped `get priorityclasses.scheduling.k8s.io`, never create/patch/delete.
4. Before the first use of a class in each reconcile pass, the Reconciler reads the real cluster object. Missing class,
   uncertain/forbidden read, actual policy other than `Never`, or default-class value/global drift fails before Pod
   apply. Successful and failed conclusions are cached for that pass.
5. A Pod-spec revision participates in the reconciliation hash, so older Pods created under implicit Kubernetes
   defaults are not accepted as current-spec capacity.

## Why the live API changed the design

The first bounded live attempt intentionally used a PriorityClass whose cluster policy was
`PreemptLowerPriority` while the generated Pod requested `Never`. OrbStack Kubernetes Priority admission returned
HTTP 403 and explained that Pod policy cannot disagree with the policy computed from the named PriorityClass.

That result disproved the initial Pod-only override design. The final implementation treats the PriorityClass object as
cluster authority, reads it before apply, and still sends the matching Pod-level `Never` as an admission-time fence.
If the class changes between read and apply, Kubernetes rejects the mismatch instead of silently enabling preemption.

## Real OrbStack environment

- Explicit context: `orbstack`; the user's default current context was not changed.
- Kubernetes API server: `v1.34.8+orb1`; kubectl client: `v1.33.9`.
- Node UID: `84cfd1d9-2365-4e37-92f7-a056a0e2c5f9`.
- Kubelet: `v1.34.8+orb1`; runtime: `docker://29.4.0`.
- Acceptance Namespace UID: `064942f9-4599-43d5-beea-2cb462ac9ed6`.
- Namespaced ServiceAccount/Role/RoleBinding UIDs:
  - `258ab247-1c02-4d31-8add-261ae063fae3`
  - `579dc868-abbf-4594-b512-9cc7d805d249`
  - `a2ecc593-8612-4d78-ab37-34d3b32c9468`
- Cluster read Role/Binding UIDs:
  - `6fb32c8a-3982-4db7-a1a4-3489c5e5928e`
  - `12658bd3-425e-4ff3-8707-73cdc7201592`
- Accepted class: `synara-stage4-nonpreempting-final1`, UID
  `987a81f5-7126-4595-be92-8b3ab84a7072`, value `700000`, policy `Never`.
- Rejected class: `synara-stage4-preempting-reject-final1`, UID
  `00bfa303-d432-48e4-9b5a-b1bfb527673c`, value `710000`, policy `PreemptLowerPriority`.
- Short-lived TokenRequest material and CA data were passed only through the test process environment and were not
  printed or persisted in this report.

## Live acceptance result

Command target:

```text
TestKubernetesWorkerPriorityOrbStackIntegration
```

Final result: PASS, test `1.60s`, package `2.282s`.

The test used the production `executionPod`, `warmPoolPod`, scheduling-template normalizer,
`validateKubernetesWorkerPodPriorityClass`, and `kubernetesHTTPClient` against the real API. It replaced only the
generated container command with bounded BusyBox `sleep` so the real kubelet could hold both Pods Running without an
agentd/control-plane endpoint.

Admitted Running Pods:

- Target: `85a258ea-ae9b-4bcf-8127-8ee4af58dae0`
- Cold execution-pinned Pod:
  `synara-exec-a018a736c495463bb8bbef824f51-g1`, UID
  `bf4c4cd3-b28a-4e20-b80c-c3ae183f0ec7`
- Warm Pod:
  `synara-warm-3719a798ca-v1-unmanaged-s0`, UID
  `0a7dd268-f4de-4f84-99f0-13b1eebd936b`
- API-observed fields on both Pods: class `synara-stage4-nonpreempting-final1`, priority `700000`,
  `preemptionPolicy=Never`.
- The live `PreemptLowerPriority` class was read and rejected before apply with
  `worker_pool_preemption_unsupported`.
- A template that explicitly requested `PreemptLowerPriority` was separately rejected during Pod construction with
  the same bounded problem code.

## Deterministic verification

- `go test ./... -count=1` from `services/control-plane`: PASS across all packages. Representative fresh times include
  `internal/agentd 32.929s`, `internal/executions 18.342s`, `internal/executiontargets 17.393s`,
  `internal/httpapi 15.630s`, `internal/placement 8.858s`, and `internal/sessions 14.630s`.
- Focused write-time, PodSpec, cache, missing-class, forbidden-read, default drift, preempting-class, cold-path, and
  warm-path tests: PASS.
- `python3 deploy/kubernetes/validate-resilience-assets.py`: `Python resilience validation passed`.
- Real API server-side dry run of `deploy/kubernetes/worker-priority-class.yaml`: accepted
  `synara-worker-nonpreempting-v1:0:Never`.
- Kustomize render includes the PriorityClass and the read-only `priorityclasses` RBAC rule.
- `git diff --check`: PASS.
- Per repository instruction, `bun fmt`, `bun lint`, and `bun typecheck` were not run because the user did not request
  them. No `bun test` command was run.

## Cleanup proof

The two Pods were deleted with their exact kubelet-authored UIDs. The Namespace, both PriorityClasses, and the
cluster-scoped Role/Binding were then deleted through raw Kubernetes `DeleteOptions` carrying API-server-enforced UID
preconditions after rechecking each run label and UID. The original Namespace UID and every cluster-scoped acceptance
name are absent. The fixed production default PriorityClass was validated with server-side dry run only and was not
created in the user's cluster.

After cleanup, the pre-existing `synara-system/synara-control-plane` Deployment remained `2/2` Ready. Its two Pods
remained Running with UIDs `4ec30a1c-9adb-445c-9878-73c1e6f4001a` and
`31838baf-4a8c-49b9-8d60-1f67e9d52062`.

## Evidence boundary

This closes the local implementation and E3 real-API/kubelet gate for non-preempting Kubernetes Worker priority. It
does not substitute for E4 EKS/GKE/AKS RBAC/admission validation, multi-node scheduling contention, managed-cloud
eviction behavior, or long-duration production soak.

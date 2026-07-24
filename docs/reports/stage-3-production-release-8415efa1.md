# Stage 3 Production Release Report — 8415efa1

## Result

**PASS / Stage 3 COMPLETE**

- Runtime release SHA: `8415efa15cebc48a23723dbdb147d3bafd7071bf`
- Runtime source state: clean
- Provider capability catalog SHA-256:
  `c75cd9113831dc4df5cb0ed1e27a2c19ca5f5ef8f9cf38e9362b11fcbb55e567`
- DDL boundary: `000041`; the closure commit adds documentation plus two static-check compatibility corrections:
  Web `spaces` test fixtures and omission of an unset optional URL in `ServerConfig.layerTest`. It does not change
  production configuration, runtime behavior or DDL.
- Production profile accepted by the operator: retained local `synara-stage3-prod` and
  `synara-stage3-worm` resources.

This report closes the Stage 3 Provider Runtime / Remote Worker release boundary. It indexes local
operator-owned evidence under `.tmp/stage3-production-8415efa1`; credentials, credential values, real Provider
origins and repository-external identity paths are intentionally omitted.

## Accepted validation policy

Remote agents use a third-party API Key, optional Base URL and custom model as the formal production path.
Adapter compatibility is established with contract, product-path and controlled-failure evidence. Expensive
production-duration load/soak, multi-node scheduling and immutable rollout are infrastructure proofs and require
one representative API-key Provider, not repeated execution for every model. Existing evidence happened to pass
both configured Adapters and is retained, but it must not be used as precedent for future duplicate heavy gates.

Subscription/OAuth login compatibility is a low-priority post-Stage-3 item and is not a release blocker.
Third-party endpoint disconnects are external-dependency diagnostics when Synara fails closed, produces one safe
terminal outcome, scans output and performs exact owner cleanup.

## Production-profile evidence

| Gate                                               | Result | Evidence                                                                                         |
| -------------------------------------------------- | ------ | ------------------------------------------------------------------------------------------------ |
| TLS Registry, multi-arch, signing and retention    | PASS   | `.tmp/stage3-production-8415efa1/registry/worker-registry-release-gate.json`                     |
| Three-node Vault Transit KMS and Kyverno admission | PASS   | `.tmp/stage3-production-8415efa1/vault-kms-admission-final-2/vault-kms-admission-gate.json`      |
| Vault Raft snapshot/restore                        | PASS   | `.tmp/stage3-production-8415efa1/vault-snapshot-restore-final/vault-snapshot-restore-drill.json` |
| Vault audit to SIEM and immutable WORM             | PASS   | `.tmp/stage3-production-8415efa1/siem-worm-production/vault-audit-siem-delivery-gate.json`       |

The Vault profile had one leader and two follower voters, remained initialized/unsealed, and preserved exactly two
PVC-backed file audit devices. Registry evidence used the checked-in production Vault Transit KMS signing profile,
verified transparency-log inclusion, digest-only promotion and fail-closed Kyverno policy.

The operator explicitly authorized one irreversible 365-day COMPLIANCE Object Lock write:

- Bucket: `synara-vault-audit`
- Object version: `3c3e85d2-7502-47b3-b3ce-542d2b8fb1bc`
- Retain until: `2027-07-23T17:03:58.342Z`
- Delete blocked: yes
- Retention shortening blocked: yes

That object version cannot be deleted or have its retention shortened before expiry. It was written once and was
not repeated during closure.

## Target release gates

| Target     | Result | Key evidence                                                                                                      |
| ---------- | ------ | ----------------------------------------------------------------------------------------------------------------- |
| Local      | PASS   | Product: each Adapter `22 pass + 1 expected unsupported`; failure: each `16/16`; Node.js `24.13.1`                |
| SSH        | PASS   | Four child reports passed; pinned Host Key/runtime ownership; disposable VMs precisely removed                    |
| Docker     | PASS   | Six child reports passed; two load runs exceeded 1800 seconds; unexpected errors `0`                              |
| Kubernetes | PASS   | Six child reports passed from one shared immutable image; product, failure, load, restart, SLA and cleanup passed |

Evidence files:

- `.tmp/stage3-production-8415efa1/local-release-final/local-release-gate.json`
- `.tmp/stage3-production-8415efa1/ssh-release/ssh-release-gate.json`
- `.tmp/stage3-production-8415efa1/docker-release/docker-release-gate.json`
- `.tmp/stage3-production-8415efa1/kubernetes-release-final/kubernetes-release-gate.json`

Docker load details:

- Codex: `1828.640s`, 28 waves, 112 Executions, five Control Plane restarts.
- Claude adapter: `1845.336s`, 32 waves, 128 Executions, six Control Plane restarts.
- Both: zero unexpected errors and exact shared-image cleanup.

Kubernetes aggregate details:

- Shared image ID:
  `sha256:565b21747f43a43b74d1de8970a1fc90d702d5a39049a93b9159407d025c62fd`.
- Codex load: `1966.765s`, 11 waves, 44 Executions, two Control Plane restarts.
- Claude adapter load: `1844.669s`, 10 waves, 40 Executions, one Control Plane restart.
- All six child reports passed the standard artifact loader and shared source/catalog/image consensus.
- Aggregate scan: 61 files / 19,844,048 bytes / zero findings.
- The shared image and every disposable child cluster were precisely removed.

The second Adapter's heavy results exceed the accepted minimum and are retained only as historical evidence; no
future heavy matrix should repeat them when a representative API-key Provider has already passed.

## Immutable rollout, promote and rollback

The deterministic three-node gate passed all 15 cases:

- target preparation and Control Plane startup;
- identity, Targets, Credential, Project and Session setup;
- immutable baseline/candidate Manifest and Revision creation;
- initial promote, baseline active, cross-node canary overlap, promote and rollback;
- release history, Audit and Outbox verification;
- exact cluster/Registry/volume/image cleanup and output Secret scan.

Evidence:
`.tmp/stage3-production-8415efa1/kubernetes-rollout/fixture-final/kubernetes-worker-release-rollout-gate.json`.
It used one control-plane and two schedulable Worker nodes, two distinct Registry digests, bounded resources and no
broad cleanup.

A representative real-provider rollout reached target preparation, seed, revisions, initial promote,
baseline-active and canary-overlap, then the third-party `/responses` stream disconnected during promote. The gate
failed closed with `provider_unavailable`, preserved safe terminal state, passed output scanning and completed exact
cleanup. This is retained as external Provider availability diagnostics; it does not invalidate the already passing
API-key product/failure/load gates or deterministic promote/rollback proof and was not rerun with another model.

## Security, cleanup and retained profile

- Final aggregate scan across `.tmp/stage3-production-8415efa1`: 455 files / 102,386,020 bytes / zero findings.
- Known Provider Key/Base URL values were registered with the scanner; no values are reproduced in this report.
- No acceptance Kind cluster, rollout Registry, temporary volume or acceptance Worker image remained.
- The only retained cluster is the operator-approved `synara-stage3-prod` production profile.
- All three Vault Pods were Ready with restart count `0`; production Registry, MinIO/WORM and Kind node containers
  were running with restart count `0` at final inspection.
- Kyverno was Ready; its historical restart counters were retained and are not represented as zero.
- `synara-stage3-prod` and `synara-stage3-worm` remain running by operator request and are not temporary cleanup
  targets.

## Known non-blocking limits

- Subscription/OAuth Provider authentication remains a post-Stage-3, low-priority compatibility item.
- Third-party Provider availability and latency are not Synara-controlled SLI; safe failure behavior is controlled.
- Cloud-specific multi-region/multi-cluster scheduling, CNI/Eviction differences, RWX cache, Warm Pool and regional
  disaster recovery belong to Stage 4.
- A production deployment into a different cloud or Registry must still perform that environment's change review,
  credential binding and targeted smoke; it must not repeat already-passing model-heavy infrastructure matrices.

## Closure decision

Registry/Vault/Kyverno/SIEM/WORM, four execution Targets, production-duration load, failure recovery, immutable
rollout/promote/rollback, cleanup and secret handling satisfy the Stage 3 plan and release checklist. Stage 3 is
therefore approved as complete at runtime SHA `8415efa15cebc48a23723dbdb147d3bafd7071bf`.

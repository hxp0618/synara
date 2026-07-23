# Stage 3 Real Provider Kubernetes Load Children

- Result: **PASS FOR BOTH PREVIOUSLY UNEXECUTED KUBERNETES LOAD CHILDREN**
- Target: Runner-owned disposable Kind, real Control Plane, agentd, Worker Protocol and Provider Host
- Resource profile: two execution-pinned Workers, one active slot per Worker, Tenant concurrency limit `2`, four Sessions
- SLA source: `deploy/worker/production-load-sla.json`
- Credential source: controlled repository-external environment; values and environment-variable names are not reproduced

## Results

| Provider | Source | Duration | Waves | Executions | Quota reject/retry | Overlap | Restart | Result |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| Codex | `cc546d3a` | `1869.164s` | `11` | `44/44` | `22/22` | `33` | `1` | pass |
| Claude Agent SDK | `46f99518` | `1835.697s` | `12` | `48/48` | `24/24` | `36` | `1` | pass |

Both children completed with `executionSuccessRate=1`, zero unexpected failure, zero duplicate terminal outcome and
zero double execution. Each restarted the Control Plane after wave `10`, then verified native-Cursor reuse, Session
sequence continuity, unique terminal paths and continued admission on the same four Sessions.

The enforced Synara-controlled SLA observations were:

| Provider | Admission P95 / P99 | Slot-reuse P95 / P99 | Required bounds | Error rate |
| --- | ---: | ---: | --- | ---: |
| Codex | `7ms / 10ms` | `4ms / 5ms` | `<= 1000/2000ms`, `<= 2000/3000ms` | `0` |
| Claude Agent SDK | `5ms / 16ms` | `5ms / 16ms` | `<= 1000/2000ms`, `<= 2000/3000ms` | `0` |

Provider-dependent interaction-ready and Turn-completion distributions remain capacity-planning evidence and are not
presented as Synara-controlled admission SLIs.

## Security, provenance and cleanup

- Codex report JSON SHA-256: `8b2cd24b79cb68526efc7f238842e175bcee10a74efc2da89b05d07d7c11a295`
- Codex report Markdown SHA-256: `b8c88072db77554b13f8d44452e51e4bdc9ba9ba5bb7ccb219265926d67d168e`
- Claude report JSON SHA-256: `070f2386caac139ee15ef6dee40bf0627b68567032e6ed9915eaf9efd380877f`
- Claude report Markdown SHA-256: `c879e74932c99fb15597fd734d3d1c6e537b3971b9aeca41abdd402900c9231e`
- Provider Capability Catalog SHA-256 in both reports:
  `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`.
- Both source worktrees were clean. No load-relevant file changed between `cc546d3a`, `46f99518` and the later
  `d0b379c8`; the intervening commit only adds Registry supply-chain proxy handling.
- Each child removed its exact Kind cluster, three owned Namespaces, Worker image and temporary state without broad
  cleanup. Post-run inventory contained only the pre-existing `synara-stage3-prod` cluster and zero labelled Stage 3
  acceptance container/image residue.
- Codex scanned `11` files / `7,134,243` bytes and Claude scanned `11` files / `6,946,566` bytes with zero Secret finding.

## Evidence boundary

This report closes the two Kubernetes `1800s` SLA/load children that the historical `3c523417` four-child aggregate
did not execute. It does not rewrite that historical aggregate or claim a same-SHA, shared-image six-child Kubernetes
release gate. Product/failure history plus these two load children are implementation evidence; the strict current
release aggregate, registry-pushed immutable rollout and production multi-node/soak boundary remain open.

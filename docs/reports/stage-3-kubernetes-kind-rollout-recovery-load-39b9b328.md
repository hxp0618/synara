# Stage 3 Kubernetes Kind Rollout Recovery and Load Acceptance — `39b9b328`

Date: 2026-07-18

Status: **PASS**

## Source and command

- Clean source SHA: `39b9b328a4e7f0d7ea16557a29171b321dbbe5ca`
- Worktree dirty at gate start: `false`
- Provider Capability Catalog SHA-256:
  `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`
- Formal command:

```sh
python3 scripts/stage3-provider-acceptance/kubernetes_worker_release_rollout_gate.py \
  --go-proxy https://goproxy.cn,direct \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 3600
```

- Gate result: `15/15` cases passed.
- Duration: `666979 ms`.
- Ignored raw evidence directory:
  `.tmp/stage3-provider-acceptance-results/20260717T213937Z-130fed00-kubernetes-worker-release-rollout/`
- JSON report SHA-256: `978a11eb4afabc8483bc9bee1a3973512d17a4de546cc64537c38e7772f1ceaf`
- Markdown report SHA-256: `2141e1fe8ad4556a9ec8bffacf837fb53c3190f2a86ea3a187abf4a9930703d1`

## Implementation boundary closed by this run

The clean SHA hardens the execution-pinned Kubernetes rollout path in four places:

1. Execution Lease renewal now ignores caller cancellation, retries transport errors, HTTP `429`, and HTTP `5xx`,
   while preserving immediate fatal fencing for other HTTP `4xx` responses such as `lease_not_current` and
   `lease_expired`.
2. A renewal request cannot occupy more than one configured renewal interval. This prevents the general API request
   timeout from crossing the authoritative Lease safety window before a retry can occur.
3. The rollout load aggregate distinguishes pooled Workers from Kubernetes execution-pinned Pods. Docker continues to
   require two stable Worker identities; Kubernetes requires one distinct Worker identity per load Execution while
   still enforcing exactly two simultaneous active Pods.
4. The deterministic rollout gate uses an explicit `12s` Worker Lease TTL and `24s` Worker heartbeat timeout. The
   failure matrix retains its shorter `6s` Lease boundary; production remains at its separate configured timing.

The Control Plane also treats `recovering` Executions as active release-transition blockers. A merely queued,
unleased Execution can still be atomically reassigned under the existing strict-CAS policy.

## Registry, topology, and resource evidence

- Disposable Kind topology: one control-plane Node plus two Worker Nodes.
- Node readiness: `3/3` Ready; schedulable Worker Nodes: `2/2`.
- The runner-owned loopback Registry pushed two different immutable digests from the same source SHA:
  - baseline: `sha256:b29370475b60f601b3dd3182eadb7fcda3eff66e42e63057f9a3c9b6cce51d7a`
  - candidate: `sha256:144839e444041e014a439ba830fc6ff2a06ade1518ed2edd10d7937d67766d27`
- Kind pulled both exact references through the run-specific containerd mirror with `imagePullPolicy=Always`; the gate
  did not use `kind load`.
- Main Target maximum active Pods: `2`; observer Target maximum active Pods: `1`.
- Every observed execution-pinned Pod matched:
  - requests: CPU `100m`, memory `128Mi`, ephemeral storage `128Mi`;
  - limits: CPU `1`, memory `1Gi`, ephemeral storage `2Gi`;
  - Workspace `emptyDir.sizeLimit`: `1Gi`;
  - ResourceQuota: `pods=2`, CPU request/limit `1/2`, memory request/limit `1Gi/2Gi`, ephemeral request `4Gi`.

During canary overlap, the baseline and candidate occupied different schedulable Worker Nodes while the Namespace
quota was fully used by exactly two Pods.

## Immutable rollout and recovery

The product Worker Release API completed the strict policy sequence:

1. baseline promote, policy version `1`;
2. candidate `100%` canary, policy version `2`;
3. candidate promote, policy version `3`;
4. baseline rollback, policy version `4`.

The candidate canary Pod was force-deleted while the promoted baseline remained pending:

- same Execution, Release Revision, Channel, Manifest, image digest, and Session were preserved;
- Pod UID and Worker ID changed;
- Generation advanced exactly `1 -> 2` and did not advance to Generation `3`;
- the stale Approval was retired and replaced with a new Generation-fenced request;
- baseline and candidate capacity remained exactly `2/2` before and after recovery;
- promote and rollback both returned `409 worker_release_active_executions` during the recovery window;
- the busy baseline Pod, Worker, Generation, Release pin, and Session events remained unchanged.

## Six-wave bounded load

The gate used four durable Sessions, two Providers, and a Tenant concurrency quota of two. Candidate promotion ran
waves `1..3`; baseline rollback ran waves `4..6`.

| Metric                                            | Candidate promoted | Baseline rollback | Total |
| ------------------------------------------------- | -----------------: | ----------------: | ----: |
| Load waves                                        |                  3 |                 3 |     6 |
| Completed load Executions                         |                 12 |                12 |    24 |
| Distinct execution-pinned Workers                 |                 12 |                12 |    24 |
| Quota rejections followed by successful admission |                  6 |                 6 |    12 |
| Two-Pod overlap observations                      |                  9 |                 9 |    18 |
| Active Release pin checks                         |                 12 |                12 |    24 |
| Terminal Release pin checks                       |                 12 |                12 |    24 |
| Worker binding checks                             |                 12 |                12 |    24 |
| Pod resource-profile checks                       |                 12 |                12 |    24 |

Across both phases, Codex and Claude Agent fixtures each completed `12` load Executions, and each of the four Sessions
completed `6`. There were no leaked Interactions, reused Executions, double executions, or duplicate terminal events.

## History, Audit, and Outbox

- Load history retained all `24/24` distinct Executions, split exactly `12` candidate-promoted and `12`
  baseline-promoted.
- The six seed/release Sessions retained six distinct Executions with one terminal event each.
- Audit pagination read `2097` entries over `11` pages and retained two Revision entries plus four ordered policy
  actions.
- The topic-filtered Outbox returned six published messages: two revision-created, two promoted, one canary-started,
  and one rolled-back.
- Session Event sequences remained contiguous under load.

## Cleanup, Secret scan, and DDL

- The owned Kind cluster, three owned Namespaces, Registry container, Registry network attachment, Registry volume,
  both Worker image slots, and isolated state were removed.
- Cleanup used exact ownership; no prune or broad cleanup was used.
- Post-run `kind get clusters` and scoped Docker inspection found no owned resources.
- Output Secret scan covered `16` files and `2842379` bytes with zero findings.
- No SQL or migration file changed. The forward migration boundary remains
  `services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Production boundary still open

This is deterministic Kubernetes rollout/load evidence with fixture Providers. It does not claim a real Codex or
Claude remote release pass.

- Third-party API keys and optional endpoints are supported through controlled Provider Credentials; Secret values
  and operator credential-source names must remain outside reports, commands, logs, and Git.
- A real external SSH gate still requires a repository-external private identity, a pinned Host Key source, and the
  existing ownership root. Password authentication is not an accepted release-gate substitute.
- Production concurrency is controlled by Tenant quota, Worker slots, and CPU/memory resource profiles. Numeric
  latency, error-rate, and duration SLA plus production-duration soak remain unapproved and unproven.
- Production signing is KMS-based and may use a self-hosted KMS. The concrete KMS reference, signer identity, tlog
  policy, and admission policy still require approval and production evidence.
- Production Registry TLS/authentication/retention, real-Provider remote rollout, cloud CNI/eviction behavior, and
  production multi-node soak remain open.

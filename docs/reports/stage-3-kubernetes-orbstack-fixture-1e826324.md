# Stage 3 OrbStack Kubernetes Fixture and Failure Matrix

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean implementation commit: `1e82632435239339212e7bc444b58698796da1ad`
- Gate run: `stage3-provider-acceptance-7ec9a4af-e55b-4a61-8549-4503ba1545ad`
- Result: **PASS FOR THE DETERMINISTIC ORBSTACK KUBERNETES FIXTURE/FAILURE SLICE; REAL-PROVIDER AND PRODUCTION GATES REMAIN OPEN**

## 1. Evidence boundary

The shared Stage 3 Acceptance Runner executed the deterministic Provider Host Protocol 2.1 fixture through the real
user API, Control Plane, Kubernetes Target reconciler, execution-pinned Pod, agentd and Worker Protocol on the
operator-approved reusable `orbstack` context. This is not a real Codex App Server or Claude Agent SDK report, does
not prove a registry-pushed immutable release, and does not close multi-node, production SLA or production KMS
admission requirements.

## 2. Context and image transport

The run used Kubernetes `v1.34.8+orb1`, Docker `29.4.0` on `linux/arm64`, and the Runner's explicit
`shared-local-container-engine` mode. The clean-SHA Worker image was built under a unique owner label and consumed
with `imagePullPolicy=Never`:

| Evidence                            | Value                                                                     |
| ----------------------------------- | ------------------------------------------------------------------------- |
| Provider Capability Catalog SHA-256 | `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`        |
| Worker image ID                     | `sha256:ae4fb087513e58467eefefd45b1ee3862d0c5f2ececd4aa0d7a5403b5b1ec961` |
| Worker version                      | `0.5.5`                                                                   |
| API route used by the gate          | `127.0.0.1:26443` with TLS server name `k8s.orb.local`                    |

OrbStack's generated hostname route was intermittently resolving to an unreachable private address while the local
listener remained healthy. A temporary mode-`0600` kubeconfig changed only the API server address to localhost and
preserved the original CA, Context and TLS server name. It was deleted immediately after the run; the user's default
kubeconfig and active Context were not modified.

## 3. Canonical cases

All `19/19` cases passed in `304,668 ms`:

- isolated Control Plane preparation/start, dev login, Kubernetes Target provisioning and bound Session resources;
- execution-pinned Worker discovery and Pending Approval Pod-loss recovery from Generation `1` to `2`;
- Approval, text/tool/usage, generated Artifact, segmented large Terminal log, Structured User Input and Provider
  error flows;
- eight-second Worker-only network interruption with `10` dropped Worker requests, no user-API interruption,
  Generation fencing and one terminal;
- `policy/v1` Pod Eviction scoped by exact Namespace, Pod name and UID precondition, followed by Generation `1` to
  `2` recovery and one terminal;
- isolated Canary Target/Namespace using a runner-owned local image alias, followed by baseline Target continuity;
- Control Plane restart and a second Turn with contiguous Session Event Sequence `1..119`;
- exact cleanup and output Secret scan.

Node cordon/drain was intentionally not authorized for this reusable single-node context and remains a separate
gate. The Canary proves Target/image isolation only; it is not an immutable Revision promotion/rollback.

## 4. Cleanup and security

The Runner removed the exact three owned Namespaces, ClusterRole/ClusterRoleBinding, main Worker image, Canary image,
Control Plane process and isolated state. `broadCleanupUsed=false`; post-run owner queries returned no resources or
images. Kubernetes cleanup now applies at most three bounded retries only to idempotent ownership `get` and exact
`delete --ignore-not-found` operations. Authorization failures, malformed responses and ownership mismatches still
fail immediately.

The output scan covered `10` JSON, Markdown, log and text files totaling `609,197` bytes. It registered six known
Secret canaries and found zero private-key, AWS access-key, GitHub-token or OpenAI-style key patterns. No Provider
API key, SSH authentication value, Kubernetes token or temporary kubeconfig was checked into the repository.

## 5. Report integrity

The ignored raw output directory is `.tmp/stage3-kubernetes-orbstack-fixture-1e826324-localhost-formal/`.

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `4a128f78771e71db2ad6e58921e29173ded3611228c0f8bd928bf9181926be5c` |
| Markdown | `cb66f8cf758e17f11e61066f207b37be081321594d3547b115920b58e9d1855c` |

## 6. Automated validation and DDL boundary

- Stage 3 Python tests: `259/259`.
- Provider Resume fallback focused test: `4/4`.
- Formal OrbStack Kubernetes fixture/failure gate: `19/19`.
- `git diff --check` and Python compilation passed.
- No database DDL changed. The checked-in forward migration boundary remains
  `000041_diff_artifact_kind.sql`.

## 7. Remaining completion boundary

This report closes the clean-SHA deterministic fixture, Pod-loss, Worker-network, exact Eviction, local-image Canary,
restart/continuity and cleanup slice on the approved `orbstack` context. It does not close:

1. real Codex/Claude product and controlled-failure reports using operator-owned Credential environment sources;
2. Node drain/PDB behavior, multi-node scheduling, registry-pushed immutable canary and rollback;
3. the authorized external SSH host release gate;
4. production resource-profiled concurrency plus numeric latency/error/duration SLA and soak;
5. approved KMS reference, signer identity, transparency-log and admission evidence.

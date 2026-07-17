# Stage 3 Owned Kind Kubernetes Drain and Failure Matrix at `fc9b2bf6`

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean implementation commit: `fc9b2bf6e044a765cb9ee5d827235cd12e82c459`
- Gate run: `stage3-provider-acceptance-5d3c6ff3-e59b-4755-9acd-fd739af3d24a`
- Result: **PASS FOR THE COMPLETE DETERMINISTIC OWNED-KIND MATRIX; REAL-PROVIDER, PDB AND MULTI-NODE GATES REMAIN OPEN**

## Evidence boundary

The shared Stage 3 Acceptance Runner created one disposable Kind cluster, built and loaded the exact clean-SHA
Worker image, and exercised the deterministic Provider Host fixture through the real user API, Control Plane,
Kubernetes Target reconciler, execution-pinned Pod, agentd and Worker Protocol. Because the cluster was fully owned
by the run, the Node Drain case was authorized. This is not real Codex/Claude or production multi-node evidence.

## Result

All `23/23` cases passed in `591,757 ms`, with no failed, skipped or unsupported case. Coverage included:

- target preparation, Worker discovery and Pending Approval Pod-loss recovery;
- approval, text/tool/usage, generated Artifact, Terminal and structured user-input paths;
- malformed, oversized and crashed Provider Host recovery;
- Worker-only network interruption;
- exact Node cordon/drain/uncordon;
- exact `policy/v1` Pod Eviction with UID precondition;
- isolated Worker image Canary and baseline continuity;
- Control Plane restart, second Turn continuity, exact cleanup and Secret scan.

The Capability Catalog SHA-256 was
`742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`.

## Node Drain evidence

The drain case completed in `51,006 ms` and used the exact Target plus Execution selector. The runner cordoned the
single owned node, used graceful Pod DELETE through `kubectl drain`, then uncordoned it in the recovery path.

- stale Execution Generation: `1`;
- replacement Generation: `2`;
- stale and replacement Worker/Interaction identities were distinct;
- `execution.recovering` was persisted before replacement resolution;
- `generationFenced=true`;
- `uncordoned=true`;
- exactly one terminal Event was persisted;
- the replacement Pod was Running with the expected non-root, read-only-root-filesystem security contract.

The separate Eviction case also recovered Generation `1 -> 2` through `policy/v1` with an exact Pod UID
precondition. Drain and Eviction therefore remain separate assertions rather than aliases.

## Canary boundary

The Canary used a runner-owned local image alias loaded into the owned Kind cluster and a separate
Target/Namespace/Session. Baseline continuity passed after Canary cleanup. This proves deterministic Target/image
isolation only; it is not registry-pushed immutable Revision promotion or rollback evidence.

## Cleanup and security

The Runner deleted the exact Kind cluster, main Worker image, Canary image, Control Plane process and isolated state.
Independent post-run checks returned no Kind cluster and no image matching owner
`ad56895105d447cb8e9b` or Git SHA `fc9b2bf6`. `broadCleanupUsed=false`.

The Secret scan covered `13` JSON, Markdown, log and text files totaling `946,483` bytes, registered six known
Secret canaries and found zero private-key, cloud-key, GitHub-token or OpenAI-style key patterns.

## Report integrity and DDL

The ignored raw output directory is `.tmp/stage3-kubernetes-kind-fixture-fc9b2bf6-formal/`.

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `99619bafafc4c537633bbd388b2779dc1f210aa0d58e2700ffa3d3b8735657f8` |
| Markdown | `4dd4b775d6b66af246b09acd750b1936d6a443c23ebf8faebdd22a2545920b21` |

No database DDL changed. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

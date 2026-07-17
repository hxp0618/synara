# Stage 3 Owned Kind Kubernetes PDB and Multi-Node Matrix at `aa1d0225`

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean implementation commit: `aa1d0225538febc40169952c0269740638fd5e3d`
- Gate run: `stage3-provider-acceptance-0f192b54-824a-467a-9524-d4804c2ccc31`
- Result: **PASS FOR THE COMPLETE DETERMINISTIC THREE-NODE OWNED-KIND MATRIX; REAL-PROVIDER AND PRODUCTION MULTI-NODE GATES REMAIN OPEN**

## Evidence boundary

The shared Stage 3 Acceptance Runner created one disposable Kind cluster with one control-plane and two Worker
Nodes, built and loaded the exact clean-SHA Worker image, and exercised the deterministic Provider Host fixture
through the real user API, Control Plane, Kubernetes Target reconciler, execution-pinned Pods, agentd and Worker
Protocol. This is deterministic owned-cluster evidence, not real Codex/Claude, a cloud-managed Kubernetes service,
registry-pushed immutable rollout, production CNI or production load/soak evidence.

## Result

All `24/24` cases passed in `580,352 ms`, with no failed, skipped or unsupported case. Cluster preparation did not
continue until the topology reported:

- one control-plane and two Worker Nodes;
- `3/3` Nodes Ready;
- two Ready schedulable Nodes.

The Capability Catalog SHA-256 was
`742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`.

Before the clean-SHA run, Acceptance Runner unit tests passed `159/159`, the complete Stage 3 Python discovery
passed `281/281`, and `bun fmt`, `bun lint` and `bun typecheck` all completed successfully. Lint retained only the
repository's existing warnings.

## Drain, PDB and Eviction separation

The existing exact Node Drain remained a separate `50,737 ms` case. It used the exact Target+Execution selector,
graceful Pod DELETE through `kubectl drain --disable-eviction`, Generation `1 -> 2`, one terminal Event and a
guaranteed uncordon.

The new PDB multi-node case completed in `62,754 ms`:

- an owner-labeled `policy/v1` PodDisruptionBudget selected only the exact Target+Execution Pod;
- `minAvailable=1`, `currentHealthy=1`, `desiredHealthy=1`, `expectedPods=1` and `disruptionsAllowed=0` were observed;
- the first Eviction-backed drain was blocked for about ten seconds while the original Pod UID remained on the
  source Node;
- the Runner deleted only that PDB, then performed the existing graceful Pod DELETE drain;
- while the source Node remained cordoned, the replacement Pod ran on the other Worker Node;
- the replacement preserved the same Execution with Generation `1 -> 2`, distinct Worker/Interaction identities,
  `generationFenced=true`, exactly one terminal Event and `uncordoned=true`.

The independent `policy/v1` Eviction case completed in `33,337 ms` with an exact Pod UID precondition and its own
Generation `1 -> 2` recovery. PDB blocking, graceful Node Drain and direct Eviction are therefore distinct assertions.

## Canary, restart and continuity

The isolated Worker image Canary, baseline Target continuity, Control Plane restart and post-restart second Turn all
passed. The Canary remains a runner-owned local image alias; it is not immutable registry promotion or rollback.

## Cleanup and security

The Runner deleted the exact Kind cluster, main Worker image, Canary image, Control Plane process and isolated state.
Independent post-run checks returned no Kind cluster and no image matching the run owner or clean SHA.
`broadCleanupUsed=false`.

The Secret scan covered `13` JSON, Markdown, log and text files totaling `1,060,841` bytes, registered six known
Secret canaries and found zero private-key, cloud-key, GitHub-token or OpenAI-style key patterns.

## Report integrity and DDL

The ignored raw output directory is `.tmp/stage3-kubernetes-kind-pdb-aa1d0225-formal/`.

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `3f9adf2a3cf36620ed7f1faf4d11335e664d3656f656f05e3c2ce5f9d659338f` |
| Markdown | `c5280baf735932b922da2679bd1af594455d7b8d9a62c8d90c70311c149b38e5` |

No database DDL changed. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

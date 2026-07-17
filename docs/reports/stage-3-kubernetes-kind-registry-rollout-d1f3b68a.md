# Stage 3 Kubernetes Kind Registry Worker Release Rollout at `d1f3b68a`

- Date: 2026-07-18
- Source: clean commit `d1f3b68a157b49708086ccbb34f0d21f74496e12`
- Gate: `kubernetes_worker_release_rollout_gate.py`
- Result: **PASS FOR DETERMINISTIC REGISTRY-PUSHED IMMUTABLE KUBERNETES ROLLOUT MECHANICS; PRODUCTION AND REAL-PROVIDER GATES REMAIN OPEN**
- Cases: `15/15`
- Duration: `224,804 ms`

## 1. Evidence boundary

The gate built and pushed two different single-platform Worker images from the same clean source SHA into one
runner-owned loopback Registry repository. It created a disposable Kind cluster with one control-plane and two
Worker Nodes, configured a containerd mirror for the run-specific `localhost:<port>` Registry authority, and used
the product Kubernetes Target and Worker Release APIs for:

1. baseline and candidate Manifest observation;
2. two immutable Revision registrations;
3. baseline promote;
4. `100%` candidate canary with a busy baseline Execution;
5. candidate promote; and
6. baseline rollback.

Every release-pinned Execution was checked through Session Event, managed Worker, Kubernetes Pod, Worker Manifest,
Revision, Channel, image reference, image digest, runtime image ID and terminal evidence. The deterministic Provider
Host fixture supplied the Approval boundary; this report is not a real Codex/Claude Adapter pass.

## 2. Runtime and topology

The owned Kind Kubernetes server was `v1.33.1`. Topology reached the required ready state before any Target was
created:

| Fact                | Observed |
| ------------------- | -------: |
| Nodes               |      `3` |
| Ready Nodes         |      `3` |
| Control-plane Nodes |      `1` |
| Worker Nodes        |      `2` |
| Schedulable Nodes   |      `2` |

The Registry used `registry:2.8.3`, loopback-only host publication, no authentication and no TLS. Kind pulled the
images with `imagePullPolicy=Always` through the containerd Registry mirror; the gate did not use `kind load`, a
shared host image store, or a same-content alias.

## 3. Immutable image, Manifest and Revision identity

| Slot      | Version                   | Registry digest                                                           | Local image ID                                                            | Manifest                               | Revision                               |
| --------- | ------------------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------------------- | -------------------------------------- | -------------------------------------- |
| baseline  | `0.5.5+rollout.baseline`  | `sha256:d5308e40de145f11706e5575a8a0eae41b4cce9ba9f242790a153a09cbae889a` | `sha256:3d18049f9ab088172401e894a49df0d5b53ccabc0322929d314d0337ee05daed` | `7aa40821-a25c-425b-b323-d02d49e390c0` | `e6a42e21-2f1d-455c-87ff-b86d6176e0bb` |
| candidate | `0.5.5+rollout.candidate` | `sha256:1274ad712ab7778d6e0a40bb338b78c051d77bb0cd07ea3381dac6da6765a543` | `sha256:fbf6678782e429fe680c71a5ff39e8e7584975eb6b5d8d12b54e6b1c925a6f67` | `303c9176-f3be-4861-9727-bdadad9b35e2` | `03ffd4f6-2995-4c47-95cb-b2ceb14988a7` |

The seed Pods independently pulled the exact baseline and candidate references. Each reported Generation `1`, the
expected digest in the Worker runtime, an equal Kubernetes runtime image ID, non-root/read-only security, the
target-scoped ServiceAccount, and no Release Revision/Channel before registration. Duplicate baseline registration
returned `409 worker_release_manifest_already_registered` without changing the two-Revision set.

## 4. Canary, promotion and rollback

The immutable Policy history was exactly:

| Policy Version | Action     | Promoted  | Canary    | Percent |
| -------------: | ---------- | --------- | --------- | ------: |
|            `1` | `promote`  | baseline  | none      |     `0` |
|            `2` | `canary`   | baseline  | candidate |   `100` |
|            `3` | `promote`  | candidate | none      |     `0` |
|            `4` | `rollback` | baseline  | none      |     `0` |

During Version `2`, the baseline/promoted and candidate/canary Approval Executions were simultaneously pending.
The baseline Pod retained the same Pod UID, Generation `1`, Node, exact image, digest, runtime image ID, Revision and
Channel after the canary policy change. The candidate used a distinct Pod UID and its candidate/canary identity.

The negative controls returned the exact expected Problems:

- stale candidate promote: `409 worker_release_policy_version_conflict`, current Version `2`, supplied Version `1`;
- candidate promote while baseline remained active: `409 worker_release_active_executions` identifying the baseline
  promoted Revision and one active Execution;
- baseline rollback while candidate remained active: `409 worker_release_active_executions` identifying the
  candidate canary Revision and one active Execution.

After both pending Executions reached one terminal each and their Pods were deleted, the candidate promoted
Execution used the candidate digest/Revision with Channel `promoted`. The final rollback Execution used the baseline
digest/Revision with Channel `promoted`. All four release-pinned Executions kept one Worker, one Manifest, one
Revision, one Channel and Generation `1` from lease through terminal.

Both overlapping Pods happened to schedule on the same Kind Worker Node. This report therefore proves a rollout on
a fully ready multi-node cluster, but does **not** claim cross-Node rollout distribution. The separate `aa1d0225`
PDB/Drain report remains the evidence for forced cross-Worker replacement while the source Node is cordoned.

## 5. Durable history and Session outcomes

- `6` isolated Sessions produced `6` distinct Executions: two Manifest seeds and four release-pinned Executions.
- Every Session retained the exact contiguous Sequence `1..12`.
- Every Execution emitted exactly one terminal Event; duplicate terminal and double execution were both absent.
- Audit pagination read `553` total entries across `3` pages and retained exactly `2` Revision plus `4` ordered
  Worker Release policy entries.
- Topic-filtered Outbox history contained exactly `6` published Worker Release messages: two Revision creations,
  two promotes, one canary start and one rollback.
- Final overview contained exactly the two original immutable digests and Policy Version `4` pointing to baseline.

## 6. Secret scan and exact cleanup

The output scan covered `16` JSON/Markdown/log/text files and `340,710 B`. It registered six known runtime Secrets
with the redactor and found zero known-Secret or pattern finding.

Cleanup confirmed:

- owned Kind cluster removed;
- isolated state removed;
- Registry container and Registry storage volume removed;
- Registry network attachment removed with the owned container;
- baseline and candidate host images removed after ownership verification;
- Registry base image retained;
- no prune or broad cleanup used.

Post-run checks returned no Kind cluster, no container with the rollout ownership label and no rollout-owned Docker
volume. No migration or DDL changed; the embedded migration boundary remains `000041_diff_artifact_kind.sql`.

Raw report hashes:

- JSON: `858cc7da13e819a337977a1af9153f5910cdae5cb7b7e8dd0dd53b42bc533d63`
- Markdown: `26e67f7b9e4e06b172f30dbfbd848daf1b1f9560d97752403a90175f369a21d2`

## 7. Remaining gates

This pass does not close:

1. real Codex/Claude Kubernetes product/failure/rollout behavior with qualifying third-party Provider profiles;
2. production Registry TLS, authentication, retention and private pull Credential;
3. approved production KMS reference, signer identity, tlog and admission policy;
4. cloud CNI, cloud Eviction, scheduler distribution or production multi-node behavior;
5. failure injection or sustained load during Kubernetes rollout;
6. numeric production latency/error-rate/duration SLA and production-duration soak; or
7. the external SSH clean-SHA four-matrix gate using a repository-external private key and pinned Host Key.

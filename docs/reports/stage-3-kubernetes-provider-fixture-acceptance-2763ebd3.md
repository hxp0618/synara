# Stage 3 Kubernetes Provider Fixture Acceptance — `2763ebd3`

- Date: 2026-07-14
- Branch: `codex/saas-tenancy-user`
- Commit: `2763ebd332179a2f7fca1a03ddcc7e11b0de9e6f`
- Source state: clean worktree; generated report recorded `worktreeDirty=false`
- Target: owned disposable Kind Kubernetes cluster
- Provider descriptor: `codex`, Experimental
- Host runtime: deterministic Provider Host Protocol 2.1 fixture
- Result: PASS, 13/13 cases

This report fixes the Kubernetes shared-protocol and orchestration evidence to one immutable source commit. It does
not promote Codex to a remote release tier and does not substitute for real Codex App Server or Claude Agent SDK
acceptance.

## Command

```bash
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target kubernetes \
  --provider codex \
  --kind-bin "$PWD/.tmp/bin/kind" \
  --kind-cluster-name synara-stage3-2763ebd3-1784001991 \
  --timeout 1200 \
  --output-dir "$PWD/.tmp/stage3-provider-acceptance-results/2763ebd3-kubernetes-1784001991"
```

The runner rebuilt the current checkout's `worker-acceptance` image before loading it into Kind. It did not use
`--kubernetes-skip-worker-build`, a reusable Kubernetes context, or `--keep`.

## Environment and build evidence

```text
Docker Engine:      29.4.0
Kind:               v0.29.0
Kubernetes server:  v1.33.1
Cluster:            synara-stage3-2763ebd3-1784001991
Worker image:       synara-stage3-provider-acceptance:2763ebd33217-kubernetes-1fe370552f2f
Worker image ID:    sha256:77dafdff0e1ef1a3046745ea2cb6483a9eb04add983935fdae0e2ff9b6f2d9ab
Worker Protocol:    2
Runtime Event:      2
```

The Control Plane and Worker were built from the same clean commit. The Worker manifest reported an available,
compatible Codex CLI runtime at `0.144.1`, while release policy kept the Provider explicitly Experimental.

## Case results

| Case | Result |
| --- | --- |
| `environment.target-prepare` | pass |
| `environment.control-plane-start` | pass |
| `identity.dev-login` | pass |
| `runtime.target-provision` | pass |
| `resources.credential-project-session` | pass |
| `runtime.worker-discovery` | pass |
| `recovery.pending-approval-runtime-loss` | pass |
| `fixture.approval-resolution` | pass |
| `fixture.text-tool-usage-artifact` | pass |
| `fixture.user-input-resolution` | pass |
| `fixture.provider-error` | pass |
| `recovery.control-plane-restart` | pass |
| `fixture.second-turn-continuity` | pass |

## Pending Approval Pod-loss recovery

The readiness barrier created one Approval on Execution
`8cb787ed-5065-4d0b-a77e-8e66ab48f964`, Generation 1. The runner force-deleted its exact execution-pinned Pod and
observed `execution.recovering` at Session Event Sequence 7.

Recovery preserved the Execution but replaced every Generation-owned identity:

| Evidence | Obsolete Generation | Replacement Generation |
| --- | --- | --- |
| Generation | `1` | `2` |
| Request ID | `fixture-approval-generation-1-1` | `fixture-approval-generation-2-1` |
| Interaction ID | `5f4d0021-ede8-47c3-8eb8-d447b020c3c9` | `a44d84df-bb5e-4ddd-a9b1-635df2d0d1b2` |
| Pod UID | `c9701409-99d0-42b0-b6d0-f39a680da967` | `7a7c3f19-a883-47ac-9d12-707c922b3dc8` |

Only the replacement Request was resolved. The Turn then emitted one valid terminal path over Sequence 2 through
16, and the generated report observed the execution-pinned terminal Pod as absent.

## Protocol and continuity evidence

- Text, Tool, Usage, Workspace dirty state, Checkpoint and ready Generated File Artifact completed over Sequence
  17 through 30. The Artifact was hash-verified and the anonymous Credential FD returned only the approved payload
  key name plus `credentialVerified=true`.
- Structured User Input resolved through the user API over Sequence 31 through 41.
- The deterministic `provider_rate_limited` failure produced one classified terminal path over Sequence 42 through
  46.
- The isolated Control Plane restarted from process Generation 1 to 2 with persisted state at Sequence 46.
- A second post-restart Turn completed at Sequence 57. The Session range remained contiguous from 1 through 57;
  the new execution-pinned Worker ID changed as expected without changing Session authority.

## Kubernetes isolation and post-run checks

The execution-pinned Pod ran as UID/GID `10001`, disallowed privilege escalation, used a read-only root filesystem,
dropped all Linux capabilities and mounted only the `home`, `tmp` and `workspace` ephemeral volumes. The Target used
an isolated Namespace, ServiceAccount, ResourceQuota, NetworkPolicy, short-lived Kubernetes credential and
`imagePullPolicy=Never`.

The generated JSON report does not embed cleanup commands. Immediately after it was written, the operator ran
narrow read-only checks against the exact owned cluster and image identities:

```text
kind get clusters
  synara-stage3-driver

docker images | match exact run tag or image ID
  <no rows>
```

Those post-run checks showed:

- `synara-stage3-2763ebd3-1784001991` no longer appeared in `kind get clusters`.
- The exact auto-built Worker image tag and image ID no longer appeared in the local image list.
- The pre-existing `synara-stage3-driver` cluster remained the only listed cluster; the acceptance report records a
  separate owned context and kubeconfig for this run.

A separate report/log pattern scan found no fixture Credential value, private-key marker, Bearer token, cloud
access key or provider-key pattern. This scan is post-run operator evidence rather than a field embedded in the
generated JSON.

## Evidence boundary

This acceptance uses the deterministic Provider Host fixture, an isolated Personal SQLite Control Plane and the
local Artifact store. It proves the real Kubernetes Target, Control Plane, reconciler, agentd, Worker Protocol,
Runtime Event, Interaction, Artifact and recovery paths. It does not prove:

- real Codex App Server or Claude Agent SDK release behavior;
- PostgreSQL or S3/MinIO behavior;
- Kubernetes Drain, Eviction, Image Rollout or Network Interruption;
- real Provider Cursor expiry and authoritative-history fallback;
- the complete Provider × Capability × Local/SSH/Docker/Kubernetes release matrix.

Those remain explicit Stage 3 gates.

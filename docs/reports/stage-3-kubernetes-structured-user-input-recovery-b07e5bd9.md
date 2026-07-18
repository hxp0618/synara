# Stage 3 Kubernetes Structured User Input Recovery — `b07e5bd9`

- Date: 2026-07-18
- Branch: `codex/saas-tenancy-user`
- Commit: `b07e5bd9b7953b951529a6aebf35e8cc489cf5e3`
- Source state: clean worktree; generated report recorded `worktreeDirty=false`
- Target: runner-owned disposable Kind Kubernetes cluster
- Provider descriptor: `codex`, Experimental
- Host runtime: deterministic Provider Host Protocol 2.1 fixture
- Result: PASS, 17/17 cases
- Duration: `387820 ms`

This report closes the deterministic execution-pinned Structured User Input Pod-loss recovery slice. It does not
promote Codex to a remote release tier and does not substitute for real Codex App Server, Claude Agent SDK, SaaS
multi-browser, production SLA, or production KMS evidence.

## Command

```bash
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target kubernetes \
  --provider codex \
  --timeout 900
```

The runner created an owned Kind cluster and kubeconfig, rebuilt the Worker image from the clean commit, loaded the
image with `imagePullPolicy=Never`, and used execution-pinned Pods. It did not reuse or mutate an operator Kubernetes
context.

## Source and build evidence

```text
Git SHA:             b07e5bd9b7953b951529a6aebf35e8cc489cf5e3
Docker Engine:       29.4.0
Kubernetes server:   v1.33.1
Worker image:        synara-stage3-provider-acceptance:b07e5bd9b7953b951529a6aebf35e8cc489cf5e3-kubernetes-7bcbe0798855
Worker image ID:     sha256:0c15f60d5d5a5d30eee2d4ed7f7b7d07d74cf767eaa8bcd197564675b19552dd
Worker Protocol:     2
Runtime Event:       2
Capability catalog:  742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb
```

The Control Plane and Worker used the same source SHA. The compatible Codex CLI runtime remained Experimental and
required explicit enablement.

## Case results

| Case                                       | Result |
| ------------------------------------------ | ------ |
| `environment.target-prepare`               | pass   |
| `environment.control-plane-start`          | pass   |
| `identity.dev-login`                       | pass   |
| `runtime.target-provision`                 | pass   |
| `resources.credential-project-session`     | pass   |
| `runtime.worker-discovery`                 | pass   |
| `recovery.pending-approval-runtime-loss`   | pass   |
| `fixture.approval-resolution`              | pass   |
| `fixture.text-tool-usage-artifact`         | pass   |
| `fixture.terminal-large-log`               | pass   |
| `recovery.pending-user-input-runtime-loss` | pass   |
| `fixture.user-input-resolution`            | pass   |
| `fixture.provider-error`                   | pass   |
| `recovery.control-plane-restart`           | pass   |
| `fixture.second-turn-continuity`           | pass   |
| `environment.cleanup`                      | pass   |
| `security.output-secret-scan`              | pass   |

## Product-shaped fixture Credential

The deterministic fixture now uses the supported Provider Credential shape:

```text
credentialType = api_key
payload key     = apiKey
```

The Worker received the Credential through the anonymous FD path. Runtime evidence retained only
`credentialPayloadKeys=["apiKey"]` and `credentialVerified=true`; the Sentinel value was registered with the
redactor and was absent from the report and logs. This keeps deterministic fixture delivery aligned with third-party
API-key product behavior without introducing another Credential type.

## Pending Structured User Input Pod-loss recovery

The fixture opened one deterministic question on an execution-pinned Pod. The runner deleted the exact Generation 1
Pod while the interaction was pending and observed `execution.recovering` at Session Event Sequence 55.

Recovery preserved the logical Turn and Execution while replacing every Generation-owned identity:

| Evidence       | Obsolete Generation                    | Replacement Generation                 |
| -------------- | -------------------------------------- | -------------------------------------- |
| Generation     | `1`                                    | `2`                                    |
| Request ID     | `fixture-user-input-generation-1-1`    | `fixture-user-input-generation-2-1`    |
| Interaction ID | `9e1cbe55-b071-4860-bf12-841fb7bece90` | `61adb33a-c4de-4076-9945-17596686bea0` |
| Worker ID      | `1860fedb-022c-41c1-bf64-a3994272e840` | `2affd74a-6547-4bab-b0c8-dffa95e0b001` |
| Pod UID        | `1f0a3a8d-a00b-423b-b863-131915d01794` | `0d649c13-5120-4f15-8eb1-02ff5d4843ac` |

The persisted obsolete interaction became `status=expired` and `deliveryStatus=superseded`. The replacement remained
on the same Execution, advanced exactly one Generation, and used the `user-input.requested` Runtime Event rather than
the Approval-only `request.opened` event.

Before resolving, the runner required the replacement payload to retain the exact deterministic question ID, header,
question text, single-select flag, and `Continue`/`Stop` option order. Missing or changed questions fail closed. The
fixture also rejects an incorrect `Stop` resolution and accepts only the exact `fixture-choice=Continue` answer.

Only the Generation 2 Request was resolved. The same Turn completed over one terminal path with Sequence 50 through
65, `singleTerminal=true`, and the terminal execution-pinned Pod was removed.

## Approval compatibility and continuity

The existing Approval recovery path also passed without acquiring the new execution-interaction-history dependency.
It preserved the same Execution, advanced Generation `1 -> 2`, replaced the Request and Pod identity, and resolved
only the replacement Request.

The remaining deterministic flow passed text, Tool, usage, generated Artifact, lossless large terminal-log segments,
Provider error classification, Control Plane restart, native Session continuity, and a second Turn.

## Cleanup and Secret evidence

The runner used exact ownership and reported:

- `ownedClusterRemoved=true`;
- `ownedWorkerImageRemoved=true`;
- `stateRemoved=true`;
- `broadCleanupUsed=false`;
- no remaining runner-owned Kind cluster or Worker image tag.

The output scan covered `11` report/log files and `349738` bytes. It found zero private-key, cloud-key,
GitHub-token, or OpenAI-style key signatures. Ignored `.tmp` output was not staged or committed.

Generated report hashes:

```text
JSON:     d0d0d9dd1243f581af1dfcd9253b27793d45d2c14731f8276c1bca2abe227bfe
Markdown: ec579d3acdb2f37003f040c41c1fee4d4479b55cef51fb356f6c81a6fd1c9ce7
```

## Verification

- Acceptance Runner: `170/170`
- Stage 3 Python: `304/304`
- Provider Host fixture: `17/17`
- Focused Web unit tests: `116/116`
- Structured User Input Browser tests: `4/4`
- `bun fmt`: pass
- `bun lint`: pass, repository warnings only
- `bun typecheck`: pass, `9/9` packages

## Evidence boundary

This clean-SHA run proves the deterministic Kubernetes Target, execution-pinned Pod replacement, Interaction
persistence, Generation fencing, exact Structured User Input replacement, resolution, Artifact, terminal, restart,
cleanup, and Secret paths. It does not prove:

- real Codex or Claude remote interaction behavior;
- two-page SaaS Structured User Input convergence or concurrent resolve;
- reusable-cluster, cloud CNI, multi-node scheduling, Drain, or rollout behavior;
- real SSH, Docker, or Kubernetes Provider release aggregates;
- numeric production latency/error/duration SLA or production-duration soak;
- a concrete production KMS reference, signer identity, tlog policy, or admission policy.

No SQL or migration file changed. The forward DDL boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

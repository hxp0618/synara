# Stage 5 real agentd + Provider tenant-isolation local acceptance

Date: 2026-07-28 (Asia/Shanghai)

Status: **PASS for the local real-agentd + deterministic Provider Host A → B combination boundary**.

This report is immutable acceptance evidence. Later runs must create a new report rather than editing this file.

## Frozen source and runtime identity

- Branch: `codex/saas-tenancy-user`
- Embedded repository HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Worktree: dirty; the test binary and Provider fixture were built from the current Stage 5 worktree, not from HEAD alone.
- Worker base image: `synara-worker:stage5-local-20260728`
- Worker image ID: `sha256:772e33a4e43053d2fb2fa5d76583d61b6a849fb19839f2a10cfa84fdb7655d00`
- Architecture/runtime: Linux arm64 in OrbStack Kubernetes.
- Test namespace: `synara-stage5-agentd-provider-20260728`
- Test Pod: `stage5-agentd-provider`

Source SHA-256 values used by the final run:

| Source | SHA-256 |
| --- | --- |
| `services/control-plane/internal/agentd/stage5_tenant_isolation_integration_test.go` | `b817e24bd7527c70872381fb06037066abd9370b1e2b32f7c212acdeae5c6d04` |
| `scripts/stage3-provider-acceptance/provider-host-fixture.ts` | `77c493d7f4de339b6fff218bcdacd31e4efcadab20eb2d6a2fb6e4fd26a2c3cb` |
| `scripts/stage3-provider-acceptance/provider-host-fixture.test.ts` | `8c9f35eaf28f1de34da9a5e6bd1b0e5c721a36a34f78450db63aa21c3b123276` |

## Acceptance path

The same `agentd.NewDaemon` instance registered one `general-pool` Worker and processed two Kubernetes Execution
generations in order:

1. Tenant A's Provider Host Protocol v2 process wrote the same unpredictable marker into six independently scrubbed
   locations: the active workspace v2 checkout, a workspace v3 Tenant subtree, the legacy Tenant workspace, the
   Target-scoped Tenant git cache, workspace quarantine, and the Worker-private temporary root.
2. Completion created a pending generation-fenced storage scrub. The daemon claimed it before requesting another
   Execution.
3. The real agentd `WorkspaceMaterializer.ScrubTenantStorage` and private-temp scrub removed those roots and only then
   acknowledged the receipt.
4. Tenant B was claimed on the same Worker ID. Its new Provider process recursively scanned the shared acceptance
   storage root instead of relying on a guessed Tenant A path.
5. Tenant B reported zero marker-bearing paths and no readable marker content.

The deterministic HTTP control-plane fixture exists only to drive the daemon/Provider combination. The complementary
OrbStack + PostgreSQL acceptance in
[`stage-4-worker-pool-tenant-isolation-orbstack-pg-20260727-final1.md`](stage-4-worker-pool-tenant-isolation-orbstack-pg-20260727-final1.md)
exercises the real control-plane row locks, Worker identity, claim fence, scrub receipt, and A → B state transition.

## Final observed result

```text
=== RUN   TestStage5SharedWorkerAgentdProviderTenantIsolation
Stage 5 real agentd + Provider A-to-B isolation PASS
worker=e89c3eae-8485-470d-8751-d765ef988203
marker=stage5-5ed2af3b52834dea96366d6ed1c06979
seeded=6
scanned=27
residualPathCount=0
residualMarkerReadable=false
--- PASS: TestStage5SharedWorkerAgentdProviderTenantIsolation (3.01s)
PASS
```

The focused Provider fixture suite also passed `21/21`, including a default-run regression that proves the fixture
detects residue before scrub and reports zero after the same six storage classes are removed.

## Fail-closed discovery

The first live attempt deliberately used a minimal synthetic Kubernetes Claim and failed before Provider startup:

```text
runner_failed: Kubernetes executions require an immutable Recovery Bundle before Provider startup.
```

The acceptance control plane was corrected to freeze a production-shaped Recovery Bundle and calculate its canonical
SHA-256 with the shared recovery-bundle encoder. The final pass therefore exercised the normal Kubernetes fail-closed
precondition instead of weakening or bypassing it.

## Cleanup

- The exact Kubernetes namespace `synara-stage5-agentd-provider-20260728` was deleted and confirmed absent.
- The local temporary cross-compiled test binary and bundled fixture directory was moved to macOS Trash and is
  recoverable until Trash is emptied.
- The existing Stage 5 Worker image was retained for reproducibility.
- No pre-existing namespace, database, report, user instance, Git staging area, commit, or remote branch was changed.

## Scope limits

- The daemon ran from a Linux `go test -c` binary containing the same production `agentd` package and `NewDaemon`
  path; this does not claim that the image's `/usr/local/bin/synara-agentd` entrypoint bytes were independently hashed.
- The Provider process was the deterministic Provider Host Protocol v2 acceptance fixture, not an external Codex or
  Claude vendor runtime. It is sufficient to prove process-to-storage visibility and real agentd scrub ordering without
  spending or exposing a Provider credential.
- This is local OrbStack evidence. It does not replace managed-cluster node, CNI, cloud metadata, IAM, or production
  canary acceptance.

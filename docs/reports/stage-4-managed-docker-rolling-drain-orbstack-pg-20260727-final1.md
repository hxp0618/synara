# Stage 4 Managed Docker durable rolling Drain — OrbStack/PostgreSQL final1

Date: 2026-07-27
Repository branch: `codex/saas-tenancy-user`
HEAD observed at final verification: `79a9067a2b92d91e70c76023c55fa8761d128064`
Evidence level: **E3 local real-Docker/PostgreSQL mechanics evidence**

This report covers only the Managed Docker reconciliation-Drain increment. The worktree was intentionally dirty and
contained concurrent Stage 4 and fast-cloud-agent work. No commit, stage, push, production deployment, or cloud mutation
is claimed here.

## Result

Managed Docker replacement and scale-down no longer depend on an ephemeral busy-Worker snapshot. Migration `000086`
adds a server-authored, durable Drain that freezes the exact Worker incarnation and instance UID before the external
Docker DELETE boundary.

The verified invariants are:

- a Worker heartbeat cannot clear a reconciliation Drain;
- new Execution and Workspace-cleanup Claim fail with `worker_reconciliation_draining` after the fence is written;
- already-held Execution and cleanup leases may finish, renew, or release, and both classes are rechecked before DELETE;
- PostgreSQL permits at most one nonterminal reconciliation Drain per Docker Target;
- the Reconciler advances at most one stale logical Worker per cycle and waits for fresh, compatible,
  release-active replacement registration before advancing again;
- overhanging scale-down indices drain before config-mismatched survivors, while an old promoted Worker cannot reserve a
  canary slot;
- a Control Plane restart after DELETE recovers the persisted Drain from an authoritative Engine list without creating
  a second Drain; and
- a same-name replacement must register with a different instance UID before the old fence is completed.

The Worker management response and Settings UI expose `reconciliationDrainIncarnation`,
`reconciliationDrainInstanceUid`, `reconciliationDrainRequestedAt`, and `reconciliationDrainReason`.

## Source identity

| Source | SHA-256 |
| --- | --- |
| `migrations/000086_worker_reconciliation_drains.sql` | `5a64b0ab6a921dfd8773cbd53f1b343a75cb1f6def372547ff19367979ebc411` |
| `internal/persistence/execution_models.go` | `59e3b95ef5e9ca9044b551285ddbefd625ba0ecba0166101c012f6dcb663cc07` |
| `internal/database/worker_revocation_sqlite.go` | `186f695a730134e4f65e41986efc867bf4f6930362726a76200d71f5dcdd818e` |
| `internal/executions/managed_docker_worker_lifecycle.go` | `3d7ef4777faa10eac56a992db77bde395fc079cc1573e6d020bafcf116bd3a4e` |
| `internal/executions/managed_docker_worker_lifecycle_test.go` | `16373e9f6d2d594077f0250a7c761ba837e233daf959b6a3a66d5e27f61af7be` |
| `internal/executions/managed_docker_worker_lifecycle_postgres_integration_test.go` | `4017b0c9d898b0a64762f214a82364f8d342e3de6b0d9f6d3368543348ae4fb3` |
| `internal/executiontargets/docker_reconciler.go` | `97f9342b7ba63c922f8472156ceb63bb528110a3c4fc2466960f5497e82bef94` |
| `internal/executiontargets/docker_reconciler_test.go` | `c9096596ca8bdc9224f8cd430400c0d885c75ce4adbf40ea08d2b1f13b5ae10e` |
| `internal/executiontargets/docker_reconciler_orbstack_integration_test.go` | `78c77770d45c2bdef1726f280a655c87d7762e486473390b0e3694f70b1ef2b3` |
| `cmd/api/main.go` | `a051ec2d789bc7d19cbd6557b0b45ff96df6237bb85e93c299aa2a7833211ff5` |
| `apps/web/src/lib/controlPlaneClient.ts` | `78d3fdc5fe7b4882ff331ba41e953f5387cc4df16517f6d712c024c994a863df` |
| `apps/web/src/components/settings/ExecutionTargetWorkerManagement.tsx` | `d5b8fdf96330d54be5292035cd1321426ab48f9d1af2f520bcf9a5fef61fa0bd` |
| `apps/web/src/components/settings/ExecutionTargetWorkerManagement.test.tsx` | `8593577e3cc329f6081c98dcda5a02d88dc8f36075d51498af3f91bec12bb922` |

## Go and UI verification

The complete Control Plane module passed after the concurrent fast-cloud merge and the preserved pre-merge Stage 4
changes were combined:

```text
go test ./... -count=1
```

All packages passed. The focused Settings projection also passed:

```text
bun run test src/components/settings/ExecutionTargetWorkerManagement.test.tsx
Test Files 1 passed (1)
Tests      4 passed (4)
```

`git diff --check` produced no errors. The heavyweight `bun fmt`, `bun lint`, and `bun typecheck` commands were not run
because they were not explicitly requested.

## Real PostgreSQL concurrency gate

Owned container: `synara-stage4-docker-drain-pg-final1`
Image: `postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20`
Loopback port: `55490`
Final applied schema tail: `87 worker_pool_min_idle`, `86 worker_reconciliation_drains`,
`85 agent_executions_claim_indexes`

The database was recreated after the final Migration 86 checksum was fixed, then the exact gate ran:

```text
SYNARA_TEST_DATABASE_URL=<owned loopback PostgreSQL URL> \
  go test ./internal/executions \
  -run '^(TestManagedDockerDrainSerializesWithPostgresExecutionClaim|TestManagedDockerDrainPostgresAllowsOneInflightWorkerPerTarget)$' \
  -count=1 -v
```

Results:

- `TestManagedDockerDrainSerializesWithPostgresExecutionClaim`: PASS, 1.54 s;
- `TestManagedDockerDrainPostgresAllowsOneInflightWorkerPerTarget`: PASS, 0.16 s.

The losing concurrent Drain produced the expected unique-index rejection (`duplicated key not allowed`); it did not
create a second active fence. The Claim/Drain race ended in one linearized authority, and a post-fence Claim observed
`worker_reconciliation_draining`.

## Current-source Worker image

The final live run rebuilt the official `worker-acceptance` target from the current dirty worktree:

```text
image tag: synara-agentd:stage4-docker-drain-final2-20260727
image ID:  sha256:3ec88ca552e5cde0c44f46e273dd175ef122d069fabca3c7aec10a7e26fa8bb4
version:   0.6.1+stage4.docker-drain.final2
revision:  79a9067a2b92d91e70c76023c55fa8761d128064
labels:    synara.io/acceptance=stage4-docker-drain-final2
           synara.io/source-worktree=dirty-verification
metadata SHA-256: b46dfa7ee3964a4269a9a7a2b93c7e52a7f8058b1f339a22cb7d920de6c73e1b
```

The revision label identifies the base HEAD only. Because the image was explicitly built with `--allow-dirty`, it is
mechanics evidence and is not a clean-SHA, signed Registry, SBOM-policy, or production release claim.

## Real OrbStack Docker result

Docker Engine: `29.4.0 linux/arm64`

```text
SYNARA_ORBSTACK_DOCKER_TEST=1 \
SYNARA_ORBSTACK_AGENTD_IMAGE=synara-agentd:stage4-docker-drain-final2-20260727 \
  go test ./internal/executiontargets \
  -run '^TestManagedDockerRollingDrainOrbStackIntegration$' \
  -count=1 -v
```

Result: PASS in 53.21 s.

```text
initial:
  index 0 = a8a1a67e574e
  index 1 = 3d8a0c2a0d53

first pass:
  index 0 = a8a1a67e574e  unchanged
  index 1 = 31e95513317c  replaced

second pass:
  index 0 = 13e68290a90e  replaced
  index 1 = 31e95513317c  unchanged

final:
  activeDrains    = 0
  currentWorkers  = 2
  terminatedFacts = 2
```

Both replacement containers ran the current acceptance agentd, registered through the real HTTP Worker endpoints, sent
fresh heartbeats, and converged on a stable third pass. The test accepted only the expected terminal
`worker_reconciliation_draining` response from the old agentd loop.

## Negative controls and cleanup

Unit/SQLite coverage also proved partial Drain shapes are rejected, a Worker heartbeat cannot clear the server fence,
Execution and cleanup Claim are blocked, both lease classes defer DELETE, a missing-container restart replay is
idempotent, and a Reconciler without the lifecycle coordinator fails closed with
`docker_worker_lifecycle_unavailable`.

The live test removed its exact containers, network, and volume. After evidence capture, the owned PostgreSQL container,
both final1/final2 local acceptance image tags, and their image IDs were removed; both metadata files were moved to the
macOS Trash. Container/image inspection returned not-found, the acceptance-label container/volume/network queries were
empty, and no process listened on loopback port `55490`. Existing OrbStack Kubernetes namespaces and unrelated Docker
resources were not modified.

## Evidence boundary

This is E3 local mechanics evidence. It does not prove a clean production image, Registry signature/tlog/admission,
multi-host Docker scheduling, managed-cloud IAM, multi-AZ behavior, production-duration soak, or a production rollout.
Those remain separate release and Stage 4 cloud gates.

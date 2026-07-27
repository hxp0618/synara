# Stage 4 Personal, SSH, and Docker regression — 2026-07-27 final1

Status: **PASS**
Evidence level: current-source unit/integration plus local real-Docker mechanics evidence
Branch: `codex/saas-tenancy-user`
Observed HEAD: `b0182cf6436683a32a25b3905d57c1936d20564e`
Worktree: dirty verification; no commit, stage, push, or deployment claimed

## Personal profile

The current source passed deterministic Personal bootstrap, metadata export/import, and runtime configuration
regression:

```text
go test ./internal/bootstrap ./internal/metadatamigration ./internal/config -count=1
```

All three packages passed. This covers deterministic owner/Tenant/Organization/local Target bootstrap, Personal
metadata round-trip, embedded Local agentd configuration, and the unchanged SQLite deployment path.

## SSH target

The current source passed all SSH-named execution-target and authority regressions:

```text
go test ./internal/executiontargets -run '^(TestSSH|TestDocker)' -count=1
go test ./internal/executions -run '(SSH|ManagedDocker)' -count=1
```

These tests cover SSH install/upgrade/revoke, exact bootstrap generation, post-registration readiness, host-key
pinning, protected-cgroup configuration, root/layout safety, atomic authority revocation, heartbeat/operation fences,
and cross-target claim isolation. The existing owned disposable OrbStack Ubuntu VM product-path acceptance remains
[`SSH final4`](stage-4-ssh-atomic-ready-acceptance-20260726-final4/acceptance-report.md): 16/16 cases passed through
real Control Plane, SSH provisioning, systemd agentd, Worker Protocol, restart/recovery/revoke, cleanup, and secret
scan. This run did not recreate that VM; it pairs the current-source focused regression with the already frozen live
Stage 4 evidence.

## Docker target

A new `worker-acceptance` image was built from the current dirty worktree and used against the real OrbStack Docker
Engine `29.4.0 linux/arm64`:

```text
SYNARA_ORBSTACK_DOCKER_TEST=1 \
SYNARA_ORBSTACK_AGENTD_IMAGE=synara-worker:stage4-regression-20260727-final2 \
  go test ./internal/executiontargets \
  -run '^TestManagedDockerRollingDrainOrbStackIntegration$' -count=1 -v
```

Result: PASS in `52.82s`. Image ID:
`sha256:de0287317bd3db38b112e85adee3ebb7afdab3642254a58bbb84d574f01fff0b`.

Two physical containers registered and became fresh/compatible. The first cycle replaced only index 1, the second
cycle replaced only index 0, and the stable cycle made no further replacement. Final durable facts were:

```text
activeDrains=0
currentWorkers=2
terminatedFacts=2
```

The run-owned containers, network, volume, build metadata directory, and image tag were all absent after exact
ownership cleanup.

## Conclusion and boundary

The K8s queue, placement, capacity, and autoscaling additions did not fork the Worker Protocol or break Personal,
SSH, or Docker single-machine behavior. This is local regression evidence, not a production release, real Provider
quality gate, multi-host Docker scheduler, or new SSH live-host run.

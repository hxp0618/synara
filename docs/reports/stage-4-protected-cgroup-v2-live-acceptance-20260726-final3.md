# Stage 4 protected cgroup-v2 live acceptance — 2026-07-26 final3

## Result

- Overall gate status: **passed**
- Live containment scenarios: **passed 5/5**
- Evidence window: `2026-07-26T15:43:29.827240Z` to `2026-07-26T15:44:22.453461Z`
- Machine-readable evidence:
  [stage-4-protected-cgroup-v2-live-acceptance-20260726-final3.json](stage-4-protected-cgroup-v2-live-acceptance-20260726-final3.json)
- Evidence SHA-256: `dea2487e23545048eb5789b7c663e49e16e542d41b6bb0de80d82006d2375217`
- Tested binary SHA-256: `f895ac58f99a99e6ee53e8767670bea63ecc2d351e2996d080b70af9f0626ecf`
- Captured source-set SHA-256: `9f601ef8684a79d4099fafce8d515dc64b48747ba8abcef2e272af574da915d7`
- Captured runner SHA-256: `28b8d4616c84a4934b7d15d74802945c8bce88e5faf96fb0df35c179d702c201`
- Focused runner-test SHA-256: `2c7ea44251350b73f9a280f9a009b0e91f1c15c3c15e871a6cf5b02b77725771`
- Captured Git HEAD: `6dc25578234472d870673d485b0556a4b61cc760` (dirty worktree)

The run created the owned disposable Ubuntu 24.04 arm64 VM
`synara-cgroup-v2-live-2026072615432913966`, opaque ID `01KYFHGZ32PMQZQ76G48638W24`. The runner observed a stable exact
name/ID/image/architecture binding around a root-owned mode `0600` run-random cloud-init marker before claiming
ownership.

## Live containment proof

The cross-built Go test ran as the only MainPID of a real systemd 255 transient service with `Delegate=yes`,
`KillMode=process`, and a unified cgroup-v2 hierarchy. All five scenarios passed:

- standalone preflight proved `UseCgroupFD`, Provider UID/GID drop, `setsid` descendant cleanup, overlap exclusion,
  and same-fence rejection;
- an extra parent PID rejected recovery with zero mutation;
- any legacy-v1 record rejected the complete recovery set with zero mutation;
- an unknown child failed closed, then exact repair allowed a fresh holder to recover the orphan;
- SIGKILL released the parent flock, preserved the non-`Pdeathsig` descendant, and a fresh holder used real
  `cgroup.kill` plus `cgroup.events` to recover it.

The retained `debian` preservation oracle stayed identical before and after the run:
`01KXWKJDF238XYZ6HFPYGQ09VF`, Debian trixie arm64, Running.

## Exact-identity cleanup proof

The runner first used OrbStack's documented exact opaque-ID delete. OrbStack 2.2.1 build 2020100 returned the known
nil-pointer panic at `scon/cmd/scli/cmd/delete.go:141`; its bounded output SHA-256 is
`d00c811e65af0649dfcd6d367d32933824db8781d6abd3f1fa0a032b2fe84c56`.

The compatibility path was enabled only after all of the following matched:

- the exact CLI return code and two panic markers;
- version `2.2.1`, build `2020100`, and commit `0e182b501fcd9e05b99ffb363fce03610390c400`;
- an owner-controlled, non-group/other-writable `sconrpc.sock` and parent directory;
- the same socket device, inode, owner, and file type before and after the request.

It then issued one local HTTP JSON-RPC request with method `ContainerDelete` and a single positional parameter equal
to the already captured opaque ID. No name field or name-delete command exists in this path. OrbStack returned the
matching request ID with `result: null`; final inventory independently confirmed that both the exact ID and owned name
were absent. There was no retry and no `manualCleanupRequired` result. Unknown versions, changed panic sites, insecure
or replaced sockets, malformed responses, a remaining ID, or a same-name replacement still fail closed and never
fall back to name deletion.

Focused Python tests passed `22/22`, including exact request shape, version gating, ambiguous-response reconciliation,
replacement refusal, marker ownership, late-create handling, lock safety, and report fallback. The `internal/agentd`
Go package also passed before this live run.

## Retained failed attempt

[final2](stage-4-protected-cgroup-v2-live-acceptance-20260726-final2.json) is retained as a separate failed attempt
with SHA-256 `9aea151cba072a1872f1344cc1a6b95620146d98161e9032ef9f727d4b19beca`. Its Ubuntu image-index fetch made no progress
for 180 seconds and left a `creating` record with no rootfs/subvolume. The root-only marker could not be observed, so
the runner correctly refused to claim ownership or delete the candidate after 15 bounded reconciliation samples.
An operator later reacquired the same name lock, bound two stable observations and the exact creation log to opaque
ID `01KYFH3YJ9EWTQBB3YP2T3HB2A`, confirmed the no-data ghost state, deleted that exact ID through the same local RPC,
and confirmed the preservation oracle was unchanged. That manual recovery is not counted as a passing gate.

## Evidence boundary

This result proves the current captured runner bytes and protected-cgroup Go source set against a local OrbStack
Ubuntu VM's real systemd and cgroup-v2 kernel behavior. It does not prove a real Codex or Claude Provider workload,
signed Control Plane projection, a production SSH host, managed-cloud escape resistance, deployment, or release. The
worktree was dirty, and these changes remain uncommitted, unpushed, and undeployed.

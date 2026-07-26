# Stage 4 protected cgroup-v2 live acceptance — 2026-07-26 final1

## Result

- Overall gate status: **failed during VM cleanup**
- Live containment scenarios: **passed 5/5**
- Evidence window: `2026-07-25T22:47:48Z` to `2026-07-25T22:48:17Z`
- Machine-readable evidence:
  [stage-4-protected-cgroup-v2-live-acceptance-20260726-final1.json](stage-4-protected-cgroup-v2-live-acceptance-20260726-final1.json)
- Evidence SHA-256: `8cb4bbb48354ca840648dc712e92687da54b0be4dddb597b095ca41f2ecacc78`
- Tested binary SHA-256: `40460904aa3731311df6a971b726624169c95abce9d768ba8d36da00e705ea7b`
- Captured source-set SHA-256: `220995a750f9c951ebd0c9b81a082dda5bff4600e2bd0e42e8d8ce634afc532c`
- Captured runner SHA-256: `14763170bb17b0822c7cc857ff7c6b6c459a69124a82a467715c2e8866219f02`

The run used the owned disposable Ubuntu 24.04 arm64 VM
`synara-cgroup-v2-live-20260726f6b9d3e7`, opaque ID `01KYDQD5RXNH9FKDMSTEEXNDEX`. Its cloud-init ownership marker was
root-owned mode `0600` and matched the run-random marker digest across a stable opaque ID.

## Live containment proof

The Go test ran as the only MainPID of a real systemd 255 transient service with `Delegate=yes`, `KillMode=process`,
and a unified cgroup-v2 hierarchy. All five scenarios passed:

- standalone preflight proved `UseCgroupFD`, Provider UID/GID drop, `setsid` descendant cleanup, overlap exclusion,
  and same-fence rejection;
- an extra parent PID rejected recovery with zero mutation;
- any legacy-v1 record rejected the complete recovery set with zero mutation;
- an unknown child failed closed, then exact repair allowed a fresh holder to recover the orphan;
- SIGKILL released the parent flock, preserved the non-`Pdeathsig` descendant, and a fresh holder used real
  `cgroup.kill` plus `cgroup.events` to recover it.

The retained `debian` VM stayed byte-for-byte identical in the recorded identity oracle:
`01KXWKJDF238XYZ6HFPYGQ09VF`, Debian trixie arm64, Running.

## Cleanup failure and recovery

OrbStack 2.2.1 documents `orb delete` as accepting an ID or name, but both the runner and an explicit retry of
`orb delete --force -- 01KYDQD5RXNH9FKDMSTEEXNDEX` crashed inside OrbStack's `delete.go` with a nil-pointer panic.
The VM therefore remained after the runner and correctly made the JSON status fail.

The operator then reacquired the same per-name lock, checked the exact opaque ID and Ubuntu noble arm64 shape twice,
observed the root-only marker, deleted the unique name, and confirmed that both the captured ID and name were absent.
Only the original `debian` VM remained. This operator recovery is not counted as a passing runner result because a
name-based delete cannot close the final identity-replacement race.

The current runner therefore deliberately has no automatic name fallback. Its SHA-256 is
`c8dfb4c62c54748ff4f2cd36d5f79d3ea93a66ba75c999b842fb3d29d147e01a`; 19 focused tests cover marker ownership,
late-create reconciliation, lock safety, fallback evidence, opaque-ID failure, and rejection of name deletion. The
known OrbStack CLI bug blocks an all-green live runner until OrbStack provides a functioning conditional ID delete.

## Evidence boundary

The real kernel and systemd observations prove the unchanged Go containment core and live test binary. They do not
prove the current runner bytes, a real Codex or Claude Provider, signed Control Plane projection, a cloud host, or a
release. The worktree is dirty, uncommitted, unpushed, and not deployed to production.

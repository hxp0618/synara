# Stage 4 protected cgroup Linux acceptance — 2026-07-25

## Result

- Status: **pass**
- Completed at: `2026-07-25T05:06:26Z`
- Runtime: privileged `golang:1.26` Linux container on OrbStack Docker, cgroup v2 with a private cgroup namespace
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`

This is a local kernel-level acceptance result from the current dirty Stage 4 worktree. It is not a clean-commit
release gate, a pushed artifact, or evidence from a production SSH host.

## Passed behavior

- The same-identity compatibility test killed a `setsid` descendant with `cgroup.kill`, waited for an empty cgroup,
  removed the fenced child, and did not advertise strict containment.
- The real `synara-agentd protected-cgroup-preflight` command ran as supervisor `uid:0 gid:0` and launched its probe
  as provider `uid:65534 gid:65534`.
- The live probe reported `useCgroupFD=true` and `setsidDescendantKilled=true`.
- The live probe generated the frozen digest
  `d0f2cfa2418c5d8c9d63b57750c21363a653bd07455e2186a90776b7c7db4a70`.
- A temporary root-owned Ed25519 key signed the full process-containment statement; the report included a canonical
  public key, its SHA-256, key ID, and a schema-v1 signature envelope.
- The Provider never received the temporary registration token or attestation private key as runner input.

## Frozen implementation inputs

| Asset | SHA-256 |
| --- | --- |
| `internal/agentd/protected_cgroup_attestation_linux.go` | `7bacb1ac7da4953670bbe5d3e537b03154297f002997902171a3bf53e942afa8` |
| `internal/agentd/protected_cgroup_supervisor_linux.go` | `7fbb5bad2b43d8c3d55c6c7f62d9e50eeb6b9e6904b0e0a8ae39f8e2fef60b14` |
| `ssh_protected_cgroup_gate.py` | `7d4977f72578bd4c695e82691ae282488a56e111cee161492dd4d689fb4b8cc8` |

## Cleanup

- The test container used `--rm` and exited successfully.
- The exact temporary cgroup root was removed by the test trap after all descendant processes exited.
- The temporary Ed25519 private key existed only inside the disposable container.

## Evidence boundary

This pass proves the Linux kernel primitives, different-UID execution, live escape probe, and signed attestation
path. A production non-Kubernetes release still has to run `ssh_protected_cgroup_gate.py` against an explicitly
approved host to prove the real systemd unit runs as root with `Delegate=yes`, the private key is root-only, the
checked env allowlist matches the live service, and the Control Plane projects `processContainment.trustState` as
`verified` for the registered Worker Manifest.

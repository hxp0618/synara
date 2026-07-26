# Stage 4 OrbStack SSH protected-cgroup acceptance — 2026-07-25

## Result

- Status: **pass**
- Gate observation: `2026-07-25T07:52:16Z`
- Disposable host: `synara-stage4-cgroup-final-20260725`
- Host runtime: OrbStack Ubuntu 24.04 (`noble`), Linux `arm64`
- Init and cgroup runtime: systemd `255`, unified `cgroup2fs`
- Branch: `codex/saas-tenancy-user`
- Base Git HEAD: `5dbeacd1c9542e76430966643d7b6fd5a2ee42f7`
- Source worktree dirty: `true`
- Agent binary SHA-256: `6843cb36e115d59bb1ccfdb81d1464160d3b3d55bf3c7ef6f624efeb0db527db`

This is local evidence from a dirty, uncommitted worktree. It is not a pushed release artifact, a production host
deployment, or a Stage 4 release-completion claim.

## Bound identities

| Identity            | Value                                                        |
| ------------------- | ------------------------------------------------------------ |
| Tenant              | `34fef581-69f9-5b83-bcc4-54e19ba7ece4`                       |
| Organization        | `11daa2d0-6f97-5dae-a2ad-36f16ff014ff`                       |
| Execution Target    | `789d5070-97e6-4832-908e-2f962eecc61d`                       |
| Worker              | `7f93c1a5-95dd-434c-bfa8-d44b87f6abc3`                       |
| Worker instance UID | `a9fbda21-9b2c-4268-9380-fcd80a6f3563`                       |
| Worker Manifest     | `27537d5e-9f15-4127-9050-5a6768258fc7`                       |
| Service             | `synara-agentd-789d5070-97e6-4832-908e-2f962eecc61d.service` |

The Execution Target was created through an isolated local Control Plane API and installed through the real SSH
provisioner. The Worker subsequently registered as `online` and `compatible`; the Control Plane's current Worker
Manifest projection reported `processContainment.mode=cgroup-v2` and `trustState=verified`.

## Passed behavior

- The systemd unit was `active/running` with a non-zero `MainPID` and `NRestarts=0` at the evidence observation.
- The new three-second post-start stability predicate was also executed against the live unit and kept
  `ActiveState=active`, `SubState=running`, a non-zero `MainPID`, and an unchanged restart count for all three samples.
- The running process executable and its systemd `EnvironmentFiles` were bound to the requested agent binary and env
  file; the live process environment matched the Target, cluster, namespace, Pod identity, and stable instance UID used
  for registration.
- `Delegate=yes` was active and `SYNARA_AGENTD_CGROUP_V2_ROOT` exactly matched the unit's delegated ControlGroup:
  `/sys/fs/cgroup/system.slice/synara-agentd-789d5070-97e6-4832-908e-2f962eecc61d.service`.
- The supervisor ran as `uid:0 gid:0`; the provider probe ran as the independently configured
  `uid:10001 gid:10002` identity and used a cgroup file descriptor.
- The preflight proved that a descendant escaping through `setsid` was still killed with its delegated subtree.
- The signed containment statement verified against the Target's allowlisted Ed25519 public key. The public-key
  SHA-256 was `f35f6577b0070053b2c1779f75fa3b72be90f4db549affa32498aa0d75e565b8`; the probe SHA-256 was
  `d0f2cfa2418c5d8c9d63b57750c21363a653bd07455e2186a90776b7c7db4a70`.
- The hardened gate's 12 focused regression tests passed before the live invocation, and the live gate returned
  `schemaVersion=synara.ssh-protected-cgroup-gate.v1`, `manifestTrusted=true`, and `status=pass`.

No private attestation key, SSH private key, bootstrap token, registration token, or Control Plane bearer credential is
recorded in this report.

## Findings fixed during the proof

- The SSH provisioner originally required the service cgroup to exist before the service could be installed. The
  pre-install check now validates the cgroup-v2 mount and policy inputs, while the exact service ControlGroup is checked
  after systemd starts the unit.
- The preflight originally sourced the env file without exporting its values to the child process. It now uses shell
  auto-export while sourcing and has a regression assertion.
- The gate now rejects a passing standalone binary when the systemd service is inactive, runs another executable,
  references another env file, owns another cgroup, or differs from the registered identity.

## Evidence boundary

The provider host used for this proof is a descriptor-only fixture that advertises the protocol surface needed to let
the Worker register. This result proves the protected non-Kubernetes process-host boundary; it does not prove a real
Codex or Claude Provider execution aggregate. The install also observed one initial registration `404` while the Target
was still in its provisioning/offline phase, followed by successful retry after activation. Production installation
still needs a durable Worker-readiness boundary rather than treating an immediate `systemctl is-active` result as full
registration readiness.

The existing OrbStack Kubernetes acceptance remains the real-kubelet lane. This separate disposable Linux-host lane
exists because Kubernetes Pod isolation cannot prove the systemd delegated-cgroup contract for SSH execution targets.

## Cleanup

- The isolated Control Plane was stopped and port `58125` was verified not listening.
- The exact disposable VM `synara-stage4-cgroup-final-20260725` was permanently deleted; its filesystem is not
  recoverable.
- The exact temporary directory containing the gate's SSH key, SQLite state, and ephemeral Control Plane credentials
  was deleted after the non-secret report was persisted; those temporary credentials are not recoverable.
- The pre-existing `debian` VM remained running and untouched.
- OrbStack Kubernetes was preserved; its `orbstack` control-plane node remained `Ready`.

# gVisor runtime tier

These optional assets install the Synara-owned `synara-gvisor` RuntimeClass and
the short-lived Node attestation publisher required by
`gvisor-sandboxed-v1`. They do not install gVisor itself.

Use the upstream [gVisor installation guide](https://gvisor.dev/docs/user_guide/install/)
and [containerd configuration guide](https://gvisor.dev/docs/user_guide/containerd/configuration/).
Current releases are multi-file archives: keep `runsc`,
`containerd-shim-runsc-v1`, and the adjacent `gvisor-bin/` directory together.

Before applying them on every selected Linux node:

1. Install a pinned gVisor release including `runsc`, its adjacent
   `gvisor-bin` assets, and `containerd-shim-runsc-v1`.
2. Configure containerd handler `runsc` and restart containerd/kubelet.
3. Verify a disposable Pod with `runtimeClassName: synara-gvisor` reaches
   `Succeeded`.
4. Label only the verified nodes with `synara.io/gvisor-ready=true`.
5. Build the root `Dockerfile` target `gvisor-node-attestor`, publish it by
   immutable digest, and replace the image placeholder in
   `node-attestor.yaml`.

The example assumes host paths `/usr/local/bin/runsc` and `/etc/containerd/config.toml`. Change
both the host mounts and matching environment paths when the distribution puts
runsc or containerd configuration elsewhere. Do not mount the container runtime
socket into the attestor.

The attestor is a node trust anchor. Its main process uses uid 0 because production
containerd configuration is commonly mode `0600`, but it drops every capability,
cannot escalate privileges, has a read-only root filesystem, and receives only the
exact read-only `runsc` binary and exact containerd configuration file. The manifest
does not mount the containerd socket or any writable host path. Restrict who may
update this DaemonSet, its image digest, ServiceAccount, or Node annotations.

Apply the assets:

```bash
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -k deploy/kubernetes/gvisor
kubectl -n synara-system rollout status daemonset/synara-gvisor-node-attestor
```

The attestor computes a bounded SHA-256 of the exact host `runsc` file, executes
`runsc --version`, verifies the configured
containerd file maps an approved CRI `runsc` section to exact runtime type
`io.containerd.runsc.v1`, and refreshes bounded
Node annotations containing the runtime version, binary digest, and configuration
digest every 15 seconds. The Control Plane accepts an observation for
at most 45 seconds and independently verifies RuntimeClass handler, every
eligible Node, a trusted-image canary Pod, and each live Worker Pod. Static
labels or annotations alone do not satisfy this contract.

Configure a new Kubernetes Target with:

```json
{
  "runtimeIsolation": {
    "mode": "explicit",
    "runtime": "gvisor",
    "minimumProfile": "gvisor-sandboxed-v1",
    "fallbackPolicy": "fail-closed",
    "runtimeClassName": "synara-gvisor",
    "gvisorCompatibleProviders": ["codex", "claudeAgent"]
  }
}
```

For `mode: auto`, keep `gvisor` before `runc` in `preferred`. A minimum of
`gvisor-sandboxed-v1` fails closed when attestation or the canary is unavailable;
it never silently creates a native-runtime Pod.

`gvisorCompatibleProviders` is an explicit Target acceptance list. If managed
Worker Releases are enabled, every active promoted/canary revision must also
carry the same immutable compatibility declaration; node detection alone never
authorizes a Provider to enter gVisor.

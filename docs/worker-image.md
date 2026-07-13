# Synara Worker image

Build the root Dockerfile `worker` target:

```bash
docker build --target worker -t synara-worker:latest .
```

The image contains Node.js 24, `synara-agentd`, `provider-host`, pinned Codex and Claude
CLIs, and a writable Workspace root. It runs as non-root UID/GID 10001 with no embedded
registration, Lease, Provider, or cloud credentials. Docker and Kubernetes Execution
Target configuration should use `synara-worker`, not the older `synara-agentd` example.

Managed Workers explicitly select Provider Host Protocol v2. Agentd performs Describe/compatibility
gating before registration and again before the actual Provider Turn; v1 requires the documented
operator-only compatibility switch and is never an automatic execution fallback.

Production deployments should publish an immutable digest and configure CPU, memory,
ephemeral storage, read-only root filesystem, disabled ServiceAccount token automount,
and a dedicated Workspace volume or `emptyDir` according to recovery requirements.
Configure separate writable Workspace and Git cache roots. Docker Workers for one Target may share both roots on
the target-scoped volume because agentd uses cross-process locks and private Workspace repositories. Kubernetes
keeps the cache Pod-local by default; an optional dedicated cache PVC must provide RWX-equivalent access and
reliable POSIX locking before it is used across Pods.

## Provider process environment

Agentd and Provider Host build child-process environments from an explicit runtime allowlist. Ambient Worker
credentials, Control Plane/Lease tokens, cloud credentials, database/object-store settings, GitHub tokens,
`NODE_OPTIONS`, SSH Agent sockets, and standard proxy variables are not inherited by Codex or Claude. Provider
credentials continue to use the Provider Host credential file descriptor and provider-specific field allowlists.

Directly operated and Local Workers that require an outbound proxy must configure the explicit Provider-only
inputs below instead of ambient `HTTP_PROXY` variables:

```text
SYNARA_PROVIDER_HTTP_PROXY
SYNARA_PROVIDER_HTTPS_PROXY
SYNARA_PROVIDER_ALL_PROXY
SYNARA_PROVIDER_NO_PROXY
```

Provider Host validates these values, maps them to the standard proxy names only in the Provider child
environment, and redacts authenticated proxy URLs and credentials from Provider diagnostics. Do not use this
channel for Control Plane, Git Workspace, database, or object-store proxy configuration; those processes retain
their own separately scoped network settings. Managed SSH, Docker, or Kubernetes Targets must expose these values
through their target-specific encrypted configuration/Secret plumbing before use; host-level ambient proxy values
are intentionally not treated as that plumbing.

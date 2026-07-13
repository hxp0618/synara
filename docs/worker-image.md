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

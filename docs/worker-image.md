# Synara Worker image

Build the root Dockerfile `worker` target:

```bash
docker build --target worker -t synara-worker:latest .
```

The image contains Node.js 24, `synara-agentd`, `provider-host`, pinned Codex and Claude
CLIs, and a writable Workspace root. It runs as non-root UID/GID 10001 with no embedded
registration, Lease, Provider, or cloud credentials. Docker and Kubernetes Execution
Target configuration should use `synara-worker`, not the older `synara-agentd` example.

Production deployments should publish an immutable digest and configure CPU, memory,
ephemeral storage, read-only root filesystem, disabled ServiceAccount token automount,
and a dedicated Workspace volume or `emptyDir` according to recovery requirements.

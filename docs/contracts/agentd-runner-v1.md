# agentd runner v1 contract

> Legacy compatibility boundary: managed Workers use Provider Host Protocol v2 by default. This v1
> one-shot contract is available only when `SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v1` is configured
> explicitly. Agentd never retries a failed v2 Turn through v1 because that could repeat Provider or
> tool side effects.

`synara-agentd` is the outbound Worker Protocol client shared by Local, SSH, Docker, and Kubernetes
Execution Targets. It registers one target-bound Worker, sends heartbeats, claims one Execution at a
time, renews its Lease while the runner is active, forwards Runtime Events and Artifacts, and reports
the terminal result.

The provider-specific runner command is configured as a JSON argument array in
`SYNARA_AGENTD_RUNNER_COMMAND_JSON`. It receives one JSON object on stdin:

```json
{
  "execution": { "id": "...", "generation": 1 },
  "workload": {
    "tenantId": "...",
    "organizationId": "...",
    "projectId": "...",
    "sessionId": "...",
    "turnId": "...",
    "provider": "codex",
    "model": "gpt-5.6-sol",
    "inputText": "...",
    "repositoryUrl": "https://example.com/repository.git",
    "defaultBranch": "main"
  },
  "providerResumeCursor": null,
  "workspaceDirectory": "/data/workspaces/..."
}
```

Lease tokens and Worker credentials are intentionally excluded from runner input.

The runner writes newline-delimited JSON to stdout. Supported messages:

```json
{"type":"event","eventType":"runtime.output.delta","payload":{"text":"working"}}
{"type":"artifact","artifact":{"path":"result.txt","kind":"generated_file","contentType":"text/plain"}}
{"type":"result","output":{"summary":"done"},"providerResumeCursor":"opaque-provider-state"}
```

Artifact paths must resolve to regular files inside the Execution workspace; symlink or traversal
escapes are rejected. The daemon uploads and confirms the Artifact using the current Lease rather than
passing storage credentials or upload grants to the provider runner.

Exactly one result message is required. A runner error, malformed/oversized message, Lease renewal
failure, or escaped Artifact path terminates the process and fails or releases the Execution through
the Worker Protocol.

## Supervised Local mode

The control plane can run the same daemon automatically for its bootstrapped Local Execution Target.
Set `SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON` on the control plane instead of starting a separate
`synara-agentd` process. The supervisor uses the loopback listener, keeps Worker credentials internal,
restarts the daemon after unexpected exits, and cancels/releases active work during control-plane
shutdown. `SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT` and `SYNARA_LOCAL_AGENTD_RESTART_BACKOFF` control the
workspace and restart delay. This mode preserves the Worker Protocol boundary; the runner still never
receives Worker or Lease tokens.

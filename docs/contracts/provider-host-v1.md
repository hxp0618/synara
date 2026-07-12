# Provider Host contract v1

`apps/provider-host` is the Worker-side boundary between `synara-agentd` and concrete
coding-agent CLIs. It reads one JSON request per line and emits normalized Runtime
Events plus a terminal result as JSON lines.

Supported adapters in v1:

- Codex through `codex exec --json` and native resume cursors;
- Claude through `claude --print --output-format stream-json` and session IDs.

The process receives Provider credentials only through anonymous file descriptor 3.
Credential fields use strict Provider-specific allowlists. Worker registration tokens,
Lease tokens, control-plane URLs, and Agentd internal environment variables are removed
before starting the Provider process. Output and errors are redacted before emission.

Later Turns reconstruct a bounded authoritative transcript from ordered Session Events
when the Worker or Execution Target changes. Native Provider resume is allowed only for
retry/recovery before durable prior Turn history exists; Worker-local Provider files are
never authoritative Session state.

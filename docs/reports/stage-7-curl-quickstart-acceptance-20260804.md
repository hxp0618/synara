# Stage 7 curl + API Key Quickstart Acceptance

- Date: 2026-08-04
- Branch: `codex/stage-7-external-sdk-platform`
- Run: `stage3-provider-acceptance-86695996-d0e5-4949-ab4b-da094d61fb50`
- Result: **PASS** (17/17 cases, 202353 ms total)
- Quickstart: **PASS** (11143 ms; required `< 300000 ms`)

## Directly proven path

The acceptance runner created an ephemeral Tenant-scoped `owner` Service Account with `api.access`, then ran
the checked-in [`examples/polaris/curl/quickstart.sh`](../../examples/polaris/curl/quickstart.sh) as a separate
process against the real local Control Plane. The script used only the public-beta HTTP surface to:

1. create a Project-bound Session on the provisioned SSH Execution Target;
2. bind the Organization-scoped fixture Provider Credential;
3. create a Turn with a stable Idempotency-Key;
4. consume the durable SSE stream;
5. find and resolve the Approval with a stable Idempotency-Key; and
6. observe `execution.completed` for the same Execution.

The five-minute measurement starts before Service Account/API Key issuance and stops only after the terminal
Event. The measured 11143 ms therefore satisfies both the M1 curl exit and M2 quickstart timing line in
[`external-sdk-developer-platform.md`](../plans/external-sdk-developer-platform.md).

## Runtime and recovery evidence

The same run used an owned disposable OrbStack Ubuntu 24.04 arm64 VM with real SSH, systemd, agentd, Worker
Protocol and Control Plane. It also passed deterministic text/tool/usage/Artifact, Approval, large Terminal,
structured user input, Provider failure, Worker replacement, post-replacement Workspace continuity, Control
Plane restart, second-Turn continuity, cleanup and output secret scan cases.

The final OrbStack inventory was empty. The incorrect Host Key negative case was rejected, the durable SSH
provisioning operation reached `succeeded`, and the public result omitted SSH configuration and the internal
systemd service name.

## Evidence minimization

The quickstart subprocess produced 6903 bytes of SSE/output data with SHA-256
`3355ba9b1b8396c56d3ea4e15d892fbd321dd17a01bb4ad72579ff116415f0d9`. The acceptance report retains only
that byte count and digest plus completion metadata. It does not retain the request prompt, raw SSE, Provider
Credential payload or one-time `syna_sa_` token. Repository search confirmed the final JSON report contains no
`inputText`, deterministic prompt marker or Service Account token prefix.

## Boundary

The Provider behavior is deterministic fixture behavior. This proves the public HTTP/API-Key contract and the
real SSH execution plumbing, but it is not a real Codex App Server or Claude Agent SDK release gate, an external
production Control Plane, an external Registry publication, or a GA approval.

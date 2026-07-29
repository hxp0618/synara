# Stage 5 Provider Proxy Credential Containment — Local Acceptance (2026-07-29)

## Scope and evidence boundary

This report covers the controlled outbound proxy environment passed from agentd through Provider Host to Codex or
Claude and arbitrary tool subprocesses. It records local source, package-test, build, and real Provider Host process
evidence. It is **not** managed-cloud CNI/IAM evidence and does not replace the Provider × Ready Worker production
matrix.

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Evidence tree: dirty, with 190 porcelain entries when captured; this report describes the uncommitted working tree.
- Host: macOS 26.5.2, Darwin 25.5.0 arm64.
- Runtime: Bun 1.3.14, Node.js 26.5.0, Go 1.26.5.

No commit, push, release artifact, managed cluster, or production change was made.

## Gap found

Provider Host previously accepted an authenticated controlled proxy URL such as
`http://user:password@proxy.example:8080`, copied it verbatim to `HTTP_PROXY`, and treated the URL/user/password as
terminal-redaction secrets. Redaction protected diagnostics but not the Provider runtime: a model-authored command can
read its own environment and recover the upstream proxy Credential directly.

The managed Kubernetes target already rejected proxy userinfo. Agentd and Provider Host still retained the older
authenticated-URL behavior, so Local/SSH/direct environment paths did not enforce the same boundary and the trusted
agentd process forwarded the value into Provider Host before child-environment construction.

## Implemented fail-closed boundary

- A shared Go `providerproxy` policy now defines the HTTP(S)/SOCKS5 credential-free authority contract used by the
  Kubernetes target reconciler and agentd.
- Agentd validates and normalizes controlled aliases before starting Provider Host. Errors contain the alias name but
  never the rejected URL or password.
- Provider Host independently revalidates every alias before constructing Codex/Claude environments. This protects
  direct/non-Kubernetes callers and future agentd regressions.
- `HTTP_PROXY` and `HTTPS_PROXY` accept only HTTP(S); `ALL_PROXY` additionally accepts SOCKS5 with an explicit valid
  port. URL userinfo (including empty userinfo), paths, query/fragment delimiters, unsupported schemes, malformed or
  invalid hosts/ports, and control characters fail closed.
- `NO_PROXY=*`, empty entries, more than 64 entries, entries longer than 253 characters/bytes, and control characters fail closed. Adding
  loopback exclusions for the task-scoped Provider API broker must also remain within the 64-entry limit.
- Credential-free endpoints remain visible in diagnostics because they are not secrets. Provider/API credentials
  continue to use task-scoped credential redaction and the execution-lifetime loopback broker.
- An authenticated enterprise proxy must terminate behind an Execution-local credential-hiding gateway; the Provider
  receives only that gateway's credential-free authority.

## Real built-process negative probe

The production Provider Host bundle was launched as a separate Node.js process with the attested outer-sandbox
profile, a temporary workspace, and a fake authenticated `SYNARA_PROVIDER_HTTP_PROXY`. A minimal one-shot Codex input
was sent on stdin. The process exited before Provider startup with:

```text
provider-host: SYNARA_PROVIDER_HTTP_PROXY must be a credential-free proxy authority
```

The fake password sentinel was absent from stdout/stderr. The temporary workspace and output file were removed after
the probe.

## Verification

- Provider Host focused proxy/runtime regression: 3 files, 99 passed / 0 failed.
- Provider Host full package: 11 files, 169 passed / 0 failed.
- Shared Go proxy policy package: PASS.
- Agentd full package: PASS (29.214s).
- Execution-targets full package: PASS (9.745s).
- Provider Host production bundle build: PASS.
- `git diff --check`: PASS.

Removing authenticated proxy values from the redactor correctly changed no-secret terminal output from one buffered
segment to its two original deltas. The existing terminal test was tightened to reconstruct the ordered byte-offset
stream (`0 + 6`, lengths `6 + 7`) instead of relying on secret-redaction buffering to coalesce those deltas.

Per repository instructions, workspace-level `bun fmt`, `bun lint`, and `bun typecheck` were not run because the
current conversation has not explicitly authorized them.

## Remaining Stage 5 gates

- Execute and retain the complete managed Codex/Claude × Ready Worker matrix, including real proxy/gateway topology.
- Close or explicitly exclude the remaining Provider-native result-provenance gaps.
- Implement the actual Stage 9 Issue webhook → automation provenance → unattended request → decline path.
- Run the final workspace format, lint, and typecheck pass after explicit authorization.

Stage 5 remains **IN PROGRESS**.

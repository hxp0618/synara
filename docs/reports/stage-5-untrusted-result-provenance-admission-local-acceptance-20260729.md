# Stage 5 Untrusted Result-Provenance Admission — Local Acceptance (2026-07-29)

## Scope and evidence boundary

This report covers Provider admission for server-authored `automation`, `external-mcp`, and `synara-mcp` task
sources. It does not change ordinary current-user turns. It records source review, official Provider-contract review,
focused/full Server tests, and a production Server build from the local dirty tree.

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Evidence tree: dirty and uncommitted; no commit, push, release artifact, or production change was made.
- Evidence class: local code/build only. This is not managed Provider × Worker or CNI/IAM acceptance.

## Gap found

The untrusted-task admission registry previously required a Host-observed fresh Approval callback and repository
startup-config isolation. Local OpenCode/Kilo satisfied those two checks after pure-mode hardening, so Agent/Synara
MCP, External MCP, and Automation could select them even though their result-provenance registry truthfully remained
`policy-only` for both successful and failed native tool results.

That left the capability matrix informational rather than authoritative. Initial task input was wrapped, and sensitive
actions still used server policy, but no attested Host context was guaranteed next to a malicious native tool result
before model consumption.

## Provider contract audit

- ACP agents execute tools and consume their results before the Client receives `session/update`; rewriting the Client
  projection cannot change model input.
- OpenCode/Kilo expose model-preconsumption `tool.execute.after` only through external same-process plugins. Mandatory
  pure mode intentionally removes those un-attested plugins.
- Google Antigravity 2.0 documents `PostToolUse` stdout as exactly `{}`. It cannot rewrite or append to the result.
  `PreInvocation` supports transient injection, but Synara's current local external plugin/script shares the Provider
  OS identity and is not an attested Host boundary. Source:
  [Google Antigravity Hooks](https://www.antigravity.google/docs/hooks).
- Pi does have adjacent Host context for both outcomes, but it still lacks the independent Host-observed fresh
  Approval callback required for server-authored untrusted tasks.
- Codex has adjacent Host context for supported success/failure outcomes under an attested tool surface; Claude has a
  structural success envelope plus adjacent failure context. Both cover local and managed runtimes.

## Implemented fail-closed admission

- The exhaustive native result-provenance registry moved to a dedicated security module and remains re-exported for
  harness policy/reporting consumers.
- A single predicate now requires non-`policy-only` provenance for both successful and failed outcomes at the selected
  runtime boundary.
- Untrusted Provider admission is the conjunction of: fresh Host-observed Approval, repository startup isolation,
  successful/failed model-preconsumption Host provenance, and a Host-started/attested runtime.
- Agent/Synara MCP input schemas and capability responses, plus External MCP capability projection, now advertise
  only Codex and Claude Agent for untrusted task creation.
- Automation create/update/run rejects policy-only Providers. The run check also covers old or directly persisted
  definitions that predate the current create/update validation.
- The durable orchestration decider rejects OpenCode/Kilo before Thread creation, worktree materialization, Provider
  startup, or subagent steer. The Reactor retains the same last-responsible-moment check.
- Ordinary human-authored Provider turns remain available; this is a source-specific security admission, not removal
  of OpenCode/Kilo/ACP/Antigravity/Pi from Synara.

## Verification

The first focused run passed 346 tests and failed seven stale product expectations: two OpenCode/Kilo capability
advertisements, two former late-Reactor failures, and three assertions that Host-started OpenCode/Kilo remained
admitted. The tests were updated to assert early schema/decider failure and zero Provider/worktree creation. No runtime
implementation defect was hidden or skipped.

- Final focused regression: 8 files, 351 passed / 0 failed.
- Full Server package: 285 files passed / 2 skipped; 3111 passed / 7 skipped / 0 failed.
- Server production build: PASS.
- `git diff --check`: PASS on the final 192-entry evidence tree.

Per repository instructions, workspace-level `bun fmt`, `bun lint`, and `bun typecheck` were not run because the
current conversation has not explicitly authorized them.

## Remaining Stage 5 gates

- Execute and retain the complete managed Codex/Claude × Ready Worker matrix.
- Run real Provider/node metadata-egress, credential-scope, and malicious-issue-denial cases on every selected managed
  Worker.
- Run the final workspace format, lint, and typecheck pass after explicit authorization.

Stage 5 remains **IN PROGRESS**.

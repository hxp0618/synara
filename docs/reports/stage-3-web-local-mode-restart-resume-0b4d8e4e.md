# Stage 3 Web no-Control-Plane local-mode restart and resume acceptance (`0b4d8e4e`)

Date: 2026-07-18

## Conclusion

Clean commit `0b4d8e4e` passes the no-Control-Plane Web local-mode slice for a real Codex first
Turn, page refresh, complete Synara Server/dev restart, transcript restore and a second native-resume
Turn.

The run used the existing local Backend Adapter only. The Control Plane profile endpoint returned the
expected `503 control_plane_unavailable`, the local SQLite projection remained authoritative, and no
Control Plane-named table or SaaS authority write existed in the isolated state database.

This closes the Workflow K local main-chat regression gate. It does not claim local Project folder
selection/file operations, another Provider, SaaS Artifact/Interaction projection, compatible-Worker
recovery or multi-browser concurrency.

## Environment

- Source: clean Git commit `0b4d8e4e` on `codex/saas-tenancy-user`.
- Runtime: bundled Node.js `v24.14.0`, Bun workspace dependencies and Codex CLI `0.144.5`.
- Web: isolated Vite/Synara development instance on Web port `8993` and Server port `58180`.
- State: isolated home `.synara-stage3-local-k`; no existing user Synara state was reused.
- Routing: `SYNARA_CONTROL_PLANE_URL` and `SYNARA_AUTH_TOKEN` unset.
- Provider: the locally authenticated Codex App Server; no SSH or third-party acceptance Credential
  was sourced, printed or persisted by this run.

The required dry-run resolved the same isolated home and ports before startup. The machine default
Node.js 22 could not execute the repository TypeScript runner, so the run used the workspace-provided
Node.js 24 without changing repository scripts or dependencies.

## Rendered flow

The Browser flow was:

```text
Control Plane profile -> 503 control_plane_unavailable
  -> local new Chat
  -> real Codex Turn 1: LOCAL_MODE_OK
  -> page refresh: same route and transcript
  -> complete Synara Server/dev restart
  -> same route and transcript restored from local SQLite
  -> real Codex Turn 2: LOCAL_MODE_RESUMED
  -> Codex thread/resume with the same Provider thread identity
```

Both exact-output probes produced one user message and one assistant message. After the full restart,
the second Provider open used `thread/resume`; its requested and resolved Provider thread identities
matched. The Browser stayed on the same local thread route throughout.

## Browser evidence

- Page identity: `Synara (Dev)` on the isolated `localhost:8993` route.
- First meaningful screen and composer rendered; there was no Vite/framework overlay.
- Turn 1 returned exact visible assistant text `LOCAL_MODE_OK`.
- Page refresh restored one matching user paragraph and one matching assistant paragraph.
- Full Server/dev restart restored the same route and the same first Turn.
- Turn 2 after restart returned exact visible assistant text `LOCAL_MODE_RESUMED`.
- Browser Console error/warning collection remained empty after the first Turn, refresh, restart and
  native-resume Turn.
- The Control Plane reconnect banner remained in its closed disclosure region: computed height `0`,
  opacity `0` and overflow hidden; it was not visible in the rendered local-mode screenshot.

## Local authority evidence

The isolated SQLite projection reported:

```text
thread env_mode=local runtime_mode=full-access interaction_mode=default
messages=4: user,assistant,user,assistant
streaming_messages=0
message_source=native
provider=codex adapter=codex resume_cursor_present=1
effect_sql_migrations=54
control_plane_named_tables=0
```

The exact persisted transcript was:

```text
user       Reply with exactly LOCAL_MODE_OK
assistant  LOCAL_MODE_OK
user       Reply with exactly LOCAL_MODE_RESUMED
assistant  LOCAL_MODE_RESUMED
```

The second startup ran no new local migration. No repository DDL or Control Plane migration changed;
the Stage 3 Control Plane migration boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Verification

```text
isolated dev dry-run                              -> passed
Control Plane profile                            -> 503 control_plane_unavailable
Browser first Turn / refresh / restart / resume  -> passed
Browser Console error/warning scan               -> 0 relevant entries
SQLite transcript/runtime/authority assertions   -> passed
bun fmt                                           -> passed
bun lint                                          -> 0 errors; existing workspace warnings only
bun typecheck                                     -> 9/9 packages passed
```

## Cleanup

The Browser test tab, isolated Synara/Vite processes, ports `58180`/`8993`, isolated home and temporary
Codex workspace were removed. No ignored acceptance state or untracked file remained in the Git
worktree. The QA screenshot was kept only under `/tmp` for the current task response and was not added
to the repository.

## Remaining Workflow K gates

1. Artifact Ready plus download/refresh/reconnect behavior through the SaaS Web.
2. Compatible-Worker live output, Approval/Input and Worker loss/recovery projection.
3. Multi-browser concurrency and strict-CAS convergence.
4. Local Project-scoped folder/file operations if they are included in the release-candidate UI
   regression matrix.

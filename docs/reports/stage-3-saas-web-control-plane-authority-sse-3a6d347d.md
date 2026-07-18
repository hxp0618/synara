# Stage 3 SaaS Web / Control Plane authority and SSE disconnect acceptance (`3a6d347d`)

Date: 2026-07-18

## Conclusion

Clean commit `3a6d347d` passes the isolated SaaS Web authority slice for Project, Session and Turn
creation, PostgreSQL persistence, page refresh, Browser reconnect, Synara Server restart and SSE
disconnect cleanup.

The run found and fixed a real proxy leak: a disconnected downstream EventSource could leave the
upstream Control Plane stream alive, so repeated refreshes accumulated active
`sse_connection_leases` and could eventually exhaust the per-user connection limit. The proxy now
binds the upstream fetch lifetime to Bun's `IncomingMessage` abort/incomplete-close signals, the
standard Node `ServerResponse` incomplete-close signal and the response-stream finalizer.

This evidence closes only the basic Web authority/reconnect slice. No compatible Worker was started,
so remote output, Artifact Ready, Approval/Input and Worker recovery remain separate acceptance gates.

## Environment

- Source: clean Git commit `3a6d347d` on `codex/saas-tenancy-user`.
- Web: isolated Vite/Synara development instance on non-default ports with `SYNARA_AUTH_TOKEN`
  unset.
- Control Plane: repository `deploy/saas` PostgreSQL + MinIO + Go Control Plane profile.
- Metadata: PostgreSQL 17, `postgres-outbox`, one Control Plane replica.
- Artifact store: private MinIO test bucket.
- Identity: local dev bootstrap with a synthetic acceptance user.
- Secrets: independent random per-run values; no Provider Credential, SSH password or production
  signing material was used or persisted.

## Authoritative flow

The rendered Browser flow was:

```text
local SaaS login
  -> create Control Plane Project
  -> create Control Plane Session
  -> submit Turn
  -> queue Execution while no compatible Worker is registered
  -> refresh/reconnect/restart
  -> recover the same Project, Session, Turn and Event Sequence
```

Observed authoritative rows:

| Resource  | Evidence                                                               |
| --------- | ---------------------------------------------------------------------- |
| Project   | `Stage 3 Authority QA`, default branch `main`, organization visibility |
| Session   | `Stage 3 authority connectivity probe`, active, Codex `gpt-5.5`        |
| Execution | one queued Local/Codex Execution                                       |
| Events    | continuous `1:session.created,2:turn.created`                          |

The Browser remained on the same Session route after refresh and Server restart, rendered the same
Project/thread/message state and reported no relevant Console error or warning. The visible
`Waiting for a compatible Worker` banner was expected because this run intentionally did not start a
Worker.

## SSE leak reproduction and fix

Before the fix, refreshing the page created another active `sse_connection_leases` row while the old
row continued renewing every heartbeat. Closing a short direct proxy client likewise left its lease
active.

The fix in `apps/server/src/controlPlaneProxy.ts`:

1. uses the Web `ReadableStream` adapter so stream finalization cancels the reader;
2. owns one `AbortController` for the complete upstream fetch lifetime;
3. aborts on Bun `IncomingMessage.aborted` or incomplete request close;
4. aborts on an incomplete standard Node response close;
5. removes all downstream listeners and aborts on normal response finalization.

Focused tests cover response-body cancellation/finalization and downstream abort propagation.

## Clean-SHA runtime evidence

- Browser refresh 1: exactly one active lease for the Session.
- Browser refresh 2: exactly one active lease for the Session; no accumulation.
- Browser close/finalize: active lease count returned to zero.
- Independent three-second SSE client through the Synara proxy:

```text
clean_sha=3a6d347d baseline=0 during=1 after=0
```

- Restarting the Synara Server recovered the existing authoritative Project/Session/Turn from the
  Control Plane and re-established one live stream.
- `/ready` reported PostgreSQL, database write, `postgres-outbox`, MinIO and schema ready.

## Verification

```text
apps/server: bun run test src/controlPlaneProxy.test.ts -> 9/9 passed
bun fmt                                           -> passed
bun lint                                          -> 0 errors; existing workspace warnings only
bun typecheck                                     -> 9/9 packages passed
```

## DDL boundary

No migration or DDL changed in this slice. PostgreSQL reported:

```text
control_plane_schema_migrations count|max(version) = 41|41
```

The Stage 3 migration boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Cleanup

The isolated Synara development processes, Control Plane, PostgreSQL, MinIO, Compose network,
PostgreSQL/MinIO volumes, temporary workspace and the run-built Control Plane image were removed.
The non-default Web, Synara, Control Plane and MinIO ports were no longer listening.

## Remaining release gates

1. Artifact Ready plus download/refresh/reconnect behavior through the SaaS Web.
2. Compatible Worker output, Approval/Input and Worker loss/recovery projected live into the Browser.
3. No-Control-Plane local-mode regression evidence on the same clean release candidate.
4. Multi-browser concurrency and the broader Provider × Target acceptance matrix.
5. Production SLA, Registry/KMS identity, transparency-log and admission evidence.

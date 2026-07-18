# Stage 3 SaaS Web Structured User Input multi-browser acceptance (`807ffa8c`)

Date: 2026-07-18

## Conclusion

Clean commit `807ffa8c` passes the isolated SaaS Web two-page Structured User Input convergence slice.

Two independent Browser contexts watched the same authoritative Session and rendered the same pending multi-select
request. They submitted different answers concurrently; the Control Plane returned exactly one `200` and one
`409 idempotency_conflict`. Both pages removed the resolved request without refresh and converged on the single
authoritative answer.

The same run then proved two stale-client boundaries. First, one page selected an answer that would normally use the
single-select auto-submit timer while the other page resolved the request directly. The stale page unmounted the
resolved request and emitted zero obsolete resolve requests after the timer window. Second, a replacement request
mounted with no inherited selection draft and resolved normally.

All three Interaction resolutions were delivered and acknowledged by the fixture Worker. The Execution emitted one
`execution.completed` and no competing terminal path. Both Browser pages rendered the final authoritative text,
kept the reconnect/unavailable Session Event banners collapsed, and produced terminal screenshots.

The run also closes the observed false reconnect presentation. After a failed catch-up succeeds and a replacement
SSE source is attached, the projection runtime now leaves `reconnecting` for `connecting`; `onOpen` or the next Event
still promotes it to `live`. This preserves the distinction between backoff and an attached-but-not-yet-confirmed
transport without hiding real errors.

This closes deterministic SaaS two-page Structured User Input convergence, competing resolve, stale timer, draft
isolation, terminal uniqueness, and false reconnect evidence. It does not prove a real Codex/Claude remote Adapter,
Provider-native unsupported-resume behavior, SSH/Docker/Kubernetes interaction release acceptance, production load
SLA, or production KMS identity/tlog/admission.

## Environment

- Source: clean Git commit `807ffa8ceb77c3bc9c4fb433864869c4b9f3523a` on `codex/saas-tenancy-user`.
- Web: isolated Vite surface at `http://localhost:57733`.
- Synara Server: isolated development Server on `127.0.0.1:60333`, with inherited Web auth disabled.
- Control Plane: repository single-node profile on `127.0.0.1:60331`.
- PostgreSQL: run-owned Compose service; `/ready` reported schema `41/41`.
- Artifact store: run-owned MinIO on `127.0.0.1:60332`.
- Compose project: `stage3-user-input-20260718155346`.
- Browser: Playwright Chromium, two isolated contexts at `1440 x 960`.
- Identity: generated `example.invalid` Dev Bootstrap identity.
- Worker: generated fixture registration, manifest, token and lease kept in process memory.
- Secrets: independent run-owned values stayed outside Git and report output. No real Provider, SSH, Kubernetes or
  production KMS credential was used.

## Acceptance flow

```text
isolated SaaS dev login
  -> create Tenant-scoped Project and Session
  -> register compatible fixture Worker from providerCapabilityCatalog.json
  -> create and claim one execution-pinned Turn
  -> open two authenticated Browser contexts on the same Session
  -> append one multi-select user-input.requested
  -> submit different answers concurrently from both pages
  -> observe one 200 and one 409 idempotency_conflict
  -> observe both pages remove the request without refresh
  -> deliver and acknowledge the authoritative resolution
  -> append one single-select stale-timer request
  -> select on page B, resolve authoritatively from page A
  -> observe zero obsolete page-B requests after unmount
  -> append a replacement single-select request
  -> verify no inherited draft, resolve and acknowledge
  -> emit final text and complete the Execution
  -> verify one terminal Event and collapsed Session Event banners on both pages
  -> capture terminal screenshots
```

## Authoritative identities

```text
tenant_id=c4109025-8ff0-4e8f-9131-b460d112be63
project_id=faa4574c-4809-4388-80c9-041f605f25e0
session_id=e14411c4-9251-435d-aad4-38510f480e7c
turn_id=953b5615-cbc2-4f73-875a-f196207f86e5
execution_id=263ddcac-0112-486b-9fb1-980a0c2770a1
worker_id=e0829df5-d563-4333-9561-8ef94c84ecf4
generation=1
last_sequence=13
```

## Multi-browser results

| Check                    | Result | Evidence                                                         |
| ------------------------ | ------ | ---------------------------------------------------------------- |
| Shared pending request   | pass   | Both pages rendered the same structured question and options     |
| Competing resolve        | pass   | HTTP statuses `200 + 409`; conflict code `idempotency_conflict`  |
| Authoritative answer     | pass   | `race-choice=["Continue"]`                                       |
| Cross-page convergence   | pass   | The request detached from both pages without refresh             |
| Stale timer cancellation | pass   | Obsolete page requests after authoritative resolve: `0`          |
| Replacement isolation    | pass   | `inheritedDraft=false`, replacement answer `Continue`            |
| Delivery                 | pass   | All three resolutions reached `acknowledged`                     |
| Terminal uniqueness      | pass   | Exactly one `execution.completed`                                |
| Session Event banner     | pass   | `pageA=hidden`, `pageB=hidden` after convergence                 |
| Framework health         | pass   | Meaningful page, valid title/route, zero Vite/Next overlay nodes |

Chromium emits one expected generic console error for the deliberately exercised `409 Conflict`. The harness
classifies only the exact browser message or explicit Synara conflict codes and stores the SHA-256 prefix
`3e46000c4ccaf61a`; raw console text is not retained in result evidence. Any other warning/error fails the run.

## Screenshot evidence

| File                                                         |           Size | SHA-256                                                            |
| ------------------------------------------------------------ | -------------: | ------------------------------------------------------------------ |
| `/tmp/synara-stage3-user-input-807ffa8c/page-a-terminal.png` | `86,962` bytes | `84fe4e0144d481a4ee9914259b7d6730c8cc29920af0b2bfbc58c23e2f370974` |
| `/tmp/synara-stage3-user-input-807ffa8c/page-b-terminal.png` | `87,062` bytes | `489c56dd77e7f223ff00febf881c865f8189d363942fceafe9623a589f340df1` |

Both images are `1440 x 960`. They show the same completed Session timeline, three User Input required/resolved pairs,
the final text `Structured User Input converged across both live pages.`, one completed Execution row, and no visible
Session Event reconnect/unavailable banner.

## Harness and runtime hardening

- The reusable entry is `bun run --cwd apps/web stage3:user-input:multibrowser`.
- Required inputs are named only: `SYNARA_STAGE3_WEB_ORIGIN`, `SYNARA_STAGE3_CONTROL_PLANE_URL`, and
  `SYNARA_WORKER_REGISTRATION_TOKEN`.
- Product API requests go through the Web origin; Worker APIs go directly to the isolated Control Plane.
- Login supports multiple Cookies and keeps them in process memory.
- HTTP failures expose only a bounded normalized problem code, never a raw response body.
- Browser console evidence stores category plus digest, never raw text.
- `SYNARA_STAGE3_MANUAL_TIMEOUT_MS` rejects non-positive or unsafe values.
- The stale-timer probe uses its own acceptance idempotency key and does not depend on the Web's internal key format.
- The Worker token, lease token, registration token and Cookie are never included in output.

## Focused verification

```text
ControlPlaneProjectionRuntime
ControlPlaneSessionStreamBanner
controlPlaneClient
controlPlaneInteractions
  -> 4 files, 54 tests passed

node --check apps/web/scripts/stage3-user-input-multibrowser.mjs
  -> passed

clean-SHA live multi-browser harness
  -> passed
```

The required single repository-wide verification pass completed:

```text
bun fmt       -> passed; 1,932 files checked/formatted
bun lint      -> passed with the existing repository warning baseline and no errors
bun typecheck -> 9/9 packages passed
```

## DDL boundary

No migration or DDL changed. The Control Plane `/ready` response reported:

```text
database=ready
databaseWrite=ready
queue=postgres-outbox ready
schema expectedVersion=41 appliedVersion=41
```

The checked-in forward migration boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Security and cleanup boundary

- No Provider Credential value, password, Worker token, Cookie, private key, presigned URL or production KMS
  identity is present in the implementation, screenshots or report.
- No repository-external SSH authentication value was used, echoed, copied or saved.
- The harness is state-writing and must run only against an isolated stack.
- Browser contexts are closed by the harness even on failure.
- The isolated dev process, Compose services/volumes and temporary screenshot directories are removed after the
  final verification pass.

## Remaining release gates

1. Passing real Codex and Claude product profiles with controlled third-party API keys across remote Targets.
2. Real Provider unsupported-resume and replacement-Worker Interaction behavior across SSH/Docker/Kubernetes.
3. External SSH acceptance using a repository-external private key and pinned Host Key; password authentication is
   intentionally unsupported.
4. Approved production resource profile plus duration, P95/P99, error-rate and recovery thresholds.
5. Concrete production KMS reference, signer identity, transparency-log and admission policy evidence.
6. Remaining advanced SaaS operations and remote-Target multi-browser release acceptance.

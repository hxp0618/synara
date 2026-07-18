# Stage 3 SaaS Web Artifact Ready and download acceptance (`88f922ed`)

Date: 2026-07-18

## Conclusion

Clean commit `88f922ed` passes the isolated SaaS Web Artifact slice. A real Control Plane Session received a
server-verified Ready `generated_file`; the Web reconciled the durable `artifact.ready` event into one visible
download row, issued a fresh download grant, fetched the exact MinIO payload, and restored the same row after page
refresh, SSE reconnect and a complete Synara Server/dev restart.

This closes the user-facing Artifact Ready/list/download/recovery delta left by the earlier Web authority report. It
does not prove compatible-Worker live output, Worker loss/recovery, Provider-native Artifact production,
Approval/Input recovery or multi-browser concurrency.

## Implementation boundary

- The Web lists Artifacts under the active Tenant and Session through the existing authenticated Control Plane API.
- A newly observed durable `artifact.ready` Sequence revalidates the authoritative Artifact list without duplicating
  the initial Session request.
- Only Ready, non-deleted `attachment`, `generated_file`, `terminal_log` and `diff` entries are user-visible;
  `checkpoint` and `workspace_snapshot` stay internal.
- Visible entries are newest first, show a leaf-only name plus kind/size metadata, and serialize download attempts so
  one panel cannot start overlapping payload fetches.
- Download clicks issue a fresh grant, fetch the payload as a Blob and surface a retryable toast on failure. The
  disclosure motion reuses the shared `DisclosureRegion` implementation.

## Environment

- Source: clean Git commit `88f922ed` on `codex/saas-tenancy-user`.
- Browser surface: Codex in-app browser, `1280 x 720`, `http://localhost:55733`.
- Synara: isolated Vite/Server development instance on Web `55733` and Server `58090`, with auth token unset and an
  ignored run-owned home directory.
- Control Plane: repository `single-node` profile on `127.0.0.1:59880`.
- Metadata and queue: PostgreSQL 17 plus `postgres-outbox`.
- Artifact store: private MinIO on `127.0.0.1:59080` with the exact browser Origin allowed by CORS.
- Identity and Secrets: synthetic Dev Bootstrap identity and independent random per-run values. No Provider
  Credential, SSH authentication, production KMS material or signing identity was used.

The first download probe correctly failed in the browser because the harness allowed
`http://127.0.0.1:55733` while the actual page Origin was `http://localhost:55733`. MinIO was recreated with the exact
Origin while preserving the run-owned volume and credentials; no source or database change was made. All passing
download evidence below is from the corrected environment.

## Browser flow and checks

The flow under test was:

```text
local SaaS login
  -> create Project
  -> submit first Turn and create authoritative Session
  -> complete one generated_file through the user Artifact API
  -> receive artifact.ready and render the Artifact panel
  -> click Download
  -> refresh/reconnect
  -> stop and restart the complete Synara dev stack
  -> recover the same Session and download again
```

| Check                 | Result | Evidence                                                                                          |
| --------------------- | ------ | ------------------------------------------------------------------------------------------------- |
| Page identity         | pass   | `Synara (Dev)` on the same Session route before/after refresh and Server restart                  |
| Meaningful page       | pass   | Project, Session transcript, Worker-wait banner, composer and Artifact panel rendered             |
| Framework overlay     | pass   | zero Vite/Next/Webpack overlay nodes                                                              |
| Console health        | pass   | zero Browser `error`/`warn` entries throughout the passing flow                                   |
| Artifact presentation | pass   | `1 ready file`, leaf name `stage3-artifact-88f922ed.txt`, `Generated file · 64 B`                 |
| Interaction proof     | pass   | download authorization `POST=200`, MinIO payload `GET=200 text/plain`, button returned to enabled |
| Refresh/reconnect     | pass   | same route and Artifact row restored from the authoritative list                                  |
| Server restart        | pass   | complete dev stop/start recovered the same route, Session message, metadata and download action   |

The app uses a fetched Blob plus a temporary object-URL anchor, which the in-app automation backend does not expose
as a native download event. The click was therefore verified through sanitized browser Network events: no presigned
query string was recorded, the same-origin grant returned `200`, MinIO returned `200 text/plain`, the request reached
`loadingFinished`, the UI reported no error toast, and the button returned to enabled.

## Artifact and persistence evidence

The one non-deleted Artifact was:

```text
kind=generated_file
status=ready
originalName=stage3-artifact-88f922ed.txt
sizeBytes=64
sha256=bc564002795a2f0e1a30a00a3c9f6dcf8b6fe41623fec2c555ec0f1ca1445fd3
```

An independent authenticated download read the same `64` bytes from MinIO and reproduced the exact SHA-256. The
single failed setup upload was explicitly deleted before the passing probe; PostgreSQL ended with one non-deleted
Ready Artifact and one soft-deleted audit row, with no pending upload.

The authoritative Session Event chain was continuous:

```text
1|session.created
2|turn.created
3|artifact.ready
last|3
```

Exactly one active SSE lease existed while the Browser Session was open. After final tab cleanup it returned to
zero. Closing the Browser canceled five concurrent read requests at the Control Plane and produced five bounded
`context canceled` error lines; there were no warnings, failed durable operations or leaked values, and the SSE lease
still returned to zero.

## Verification

```text
apps/web focused Vitest -> 2 files, 6 tests passed
apps/web typecheck      -> passed
```

The final repository-wide `bun fmt`, `bun lint` and `bun typecheck` results are recorded in the documentation commit
that publishes this report.

## DDL boundary

No migration or DDL changed. PostgreSQL and `/ready` both reported:

```text
control_plane_schema_migrations count|max(version) = 41|41
expectedVersion=41
appliedVersion=41
```

The Stage 3 migration boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Security and cleanup

- The staged implementation diff contained no Credential, password, token, private-key or API-key material.
- Control Plane log comparison against the run-owned database, MinIO, Worker, Cursor and KMS values found zero
  matches; Synara logs contained zero presigned URL or authorization markers.
- No Cookie, presigned URL query, Secret value or operator environment variable name is included in this report.
- The isolated Web/Server processes, PostgreSQL, MinIO, Control Plane, Compose network, volumes, run-built image and
  temporary state were removed. Ports `55733`, `58090`, `59880` and `59080` were no longer listening.

## Remaining release gates

1. Live compatible-Worker output, Approval/Input and Worker loss/recovery projected into the same Browser Session.
2. Multi-browser concurrency and cross-browser model/Interaction/Artifact reconciliation.
3. Real SSH aggregate plus passing real Codex/Claude Docker and Kubernetes product profiles.
4. Production-duration load/soak with approved P95/P99/error/recovery thresholds.
5. Approved production KMS reference, signer identity, transparency-log and admission policy evidence.

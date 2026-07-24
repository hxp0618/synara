# Stage 3 Worker Release Automatic Rollback

Date: 2026-07-24
Implementation base: `a7e1ca5b2f987cec8e5969a242828a3391f00a91`

## Delivered boundary

- Canary starts a durable, Policy-Version-scoped observation window by default. The default is 15 minutes,
  minimum four relevant terminal Executions, minimum two attributable failures and a 50% attributable failure rate.
- Promoting the canary starts a fresh promoted-revision window with the same thresholds and the previous promoted
  Revision as fallback.
- The multi-replica controller uses a PostgreSQL advisory lock, persists `rollback-pending` evidence before acting,
  and reuses release CAS, active/recovering Execution fencing, Worker synchronization and immutable transitions.
- A blocked decision remains pending and is retried. A successful action writes system Audit and dedicated Outbox
  evidence; operator policy changes supersede the window safely.
- Migration `000042_worker_release_auto_rollback.sql` and the SQLite safety mirror make window identity/configuration
  immutable, constrain state transitions and prohibit evidence deletion.

## Decision boundary

Threshold signals are limited to `provider_not_installed`, `provider_version_incompatible`, `protocol_violation`
and a candidate Worker Manifest becoming incompatible during the bounded observation window. Third-party API Key or
OAuth failures, Provider availability, rate limits, quota/credit issues, network failures and Base URL failures are
ignored by the release decision and are not included in the relevant-execution denominator. No Provider error message,
credential or Base URL is persisted in decision evidence.

## Verification

The focused control-plane packages passed:

```text
go test ./internal/workerreleases ./internal/config ./internal/database \
  ./internal/observability ./internal/httpapi ./cmd/api -count=1
go vet ./...
```

Automatic rollback tests cover threshold trigger, ignored external Provider failures, durable pending/retry after an
active Execution drains, immediate candidate incompatibility, and the promoted-revision observation window.

The broader `go test ./... -count=1` was also attempted. All packages except `internal/executions` passed;
`internal/executions` failed existing environment-sensitive Provider catalog assertions because the locally observed
Codex runtime was classified outside the frozen compatible range. The failing package is outside this change and the
focused HTTP/database/release packages remain green. No real Provider, Kubernetes acceptance matrix or model-heavy
load was rerun.

## Deployment note

This change is a post-closure extension and is not part of the sealed runtime SHA `8415efa1` production evidence.
Deploying it requires the normal PostgreSQL backup/change window, applying migration `000042`, and a targeted
release-policy smoke. It does not require repeating the already-passing Provider/Kubernetes model matrices.

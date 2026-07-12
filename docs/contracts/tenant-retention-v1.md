# Tenant retention contract v1

Tenant Owners, Admins, and Security Admins may configure:

- inactive Agent Session archival after 1–36500 days;
- ready Artifact payload deletion after 1–36500 days.

`null` disables a policy dimension. Session archival skips any Session with a queued,
leased, running, or recovering Execution. Artifact deletion removes the object and
access tokens before marking metadata deleted; object-store failures remain retryable
in `deleting`.

One sweeper runs at a time through a PostgreSQL advisory lock. It records per-resource
and summary Audit entries only when material work occurs, so stable reruns do not create
redundant Audit rows. The same sweep also removes bounded batches of expired ephemeral
records such as login attempts, old login sessions, Worker receipts, access tokens,
Service Account tokens, and old invitations. Append-only Audit Logs are never deleted.

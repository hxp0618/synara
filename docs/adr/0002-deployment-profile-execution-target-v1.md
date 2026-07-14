# ADR 0002: Deployment Profile and Execution Target v1

- Status: Accepted
- Date: 2026-07-12
- Scope: Revised Phase 0 foundations used by Phases 4 and 5

## Context

Synara must support a small personal installation and a replicated enterprise control plane without
forking the Tenant, Organization, Session, Execution, Lease, or Worker domain. The place where the
control plane runs is a different concern from the place where an Agent executes. Docker can appear
in either dimension, so one combined deployment/execution enum would be ambiguous and unsafe.

## Decision

`DeploymentProfile` and `ExecutionTarget` are independent v1 contracts.

The supported deployment profiles are:

| Profile       | Metadata   | Artifacts        | Queue                                              | Control-plane replicas |
| ------------- | ---------- | ---------------- | -------------------------------------------------- | ---------------------- |
| `personal`    | SQLite     | local filesystem | in-process                                         | exactly one            |
| `single-node` | PostgreSQL | MinIO or S3      | PostgreSQL outbox                                  | exactly one            |
| `enterprise`  | PostgreSQL | S3               | PostgreSQL outbox, with an optional external queue | two or more            |

The supported execution target kinds are `local`, `ssh`, `docker`, and `kubernetes`. Every kind uses
the same Worker Protocol, Execution lease, generation fencing, runtime event, and future artifact
interfaces. Workspace mode (`local` or `worktree`) remains a separate Session/workspace setting.

Additional v1 decisions:

1. Personal-to-enterprise migration is explicit metadata export/import. In-place profile mutation is
   rejected. Artifact payload transfer uses the verified, reentrant Artifact v1 migration contract.
2. SSH targets may later install, start, upgrade, and revoke `synara-agentd` automatically. Long-lived
   execution still uses the Worker Protocol rather than raw `ssh "codex ..."` commands.
3. Docker execution v1 uses a registered worker pool. It does not create one container per Agent
   Session.
4. SQLite is a single-control-plane adapter and does not emulate PostgreSQL `SKIP LOCKED` behavior.
5. Remote workers (`ssh`, `docker`, and `kubernetes`) must advertise lease and generation-fencing
   support at registration and cannot claim work for another target.

## Invariants

- Profile validation occurs before database startup; invalid enum values or unsafe combinations never
  silently fall back.
- SQLite, local artifacts, and the in-process queue are rejected with multiple control-plane replicas.
- An Agent Session always references one active execution target. Every Agent Execution persists the
  inherited `execution_target_id` and `target_kind`.
- A tenant-owned target cannot reference an organization in another tenant. Platform targets have both
  `tenant_id` and `organization_id` null.
- A Personal installation's default target is tenant- and organization-owned; no Personal domain
  resource uses null ownership.
- Target connection configuration is encrypted at rest and is never returned by the HTTP API.

## Consequences

The revised foundations support Personal SQLite and server PostgreSQL through one GORM domain while
retaining PostgreSQL's migration and concurrent-claim semantics. Artifact payload migration is now
implemented; concrete execution drivers, Kubernetes reconciliation, and enterprise identity/security
remain Phase 5-6 work.

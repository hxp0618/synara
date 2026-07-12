# ADR 0001: SaaS control plane tenancy baseline

- Status: Accepted
- Date: 2026-07-11
- Scope: Phase 0 and Phase 1 of the SaaS tenancy plan

## Context

Synara's existing TypeScript server is a local-first Provider Runtime backed by SQLite. Its
`owner` and `client` roles protect a server connection; they are not SaaS identities or tenant
roles. Adding tenant columns directly to the local runtime would couple deployment, billing,
identity, and worker scheduling concerns to process-local provider state.

## Decision

1. Add a modular Go control plane under `services/control-plane`.
2. Keep the TypeScript Provider Runtime and its SQLite database intact during the migration.
3. Use PostgreSQL as the authority for SaaS identity, tenants, organizations, membership,
   login sessions, audit logs, and the transactional outbox.
4. Serve control-plane APIs below `/v1`. The existing Synara server proxies that prefix so the
   web app keeps a same-origin connection and does not learn internal service addresses.
5. Generate UUID v4 identifiers in the application. Store all timestamps as UTC
   `TIMESTAMPTZ`. IDs are opaque and clients must not infer ordering from them.
6. A Tenant is the customer, billing, security, and data-isolation boundary. A User may join
   multiple Tenants.
7. Organizations reserve a parent relationship, but v1 only grants permissions directly; it
   does not inherit permissions through the organization tree.
8. Every Tenant is created with one Root Organization and one active Owner membership in the
   same database transaction.
9. Business code checks permissions, not role strings. Roles map to a fixed permission set in
   `internal/authorization`.
10. PostgreSQL Outbox is the v1 delivery mechanism. Runtime Event payloads use versioned JSON.

## Defaults frozen for v1

- Standard tenants use shared worker namespaces; dedicated namespaces are an enterprise policy.
- Agent Session visibility defaults to `private`.
- Provider credentials may eventually be tenant-, organization-, or user-owned, but Phase 1
  does not persist credential material.
- Remote workspaces begin with Git clone/fetch/push and snapshots; live local-file mirroring is
  out of scope.
- Desktop and SaaS deployments share schema contracts, while their persistence implementations
  remain separate.

## Invariants

- Tenant-scoped repositories always receive `tenantID` explicitly.
- Resource identifiers alone never authorize access.
- Organization membership requires an active Tenant membership.
- Every active Tenant has at least one active Owner.
- Tenant IDs are immutable after resource creation.
- Audit rows are append-only.
- The frontend never connects directly to a Worker Pod.

## Consequences

The control plane can evolve independently and later schedule remote Provider workers without a
rewrite of existing adapters. During the transition, local runtime state remains usable while
SaaS control-plane features are enabled only when `SYNARA_CONTROL_PLANE_URL` is configured.

# Stage 4 Platform routing authority — OrbStack E3 final1

Date: 2026-07-26  
Repository branch: `codex/saas-tenancy-user`  
Base commit observed during the run: `260dd2d4465e`  
Evidence level: **E3 local real-Kubernetes/runtime evidence**  
Kubernetes context: explicit `--context orbstack`  
Kubernetes server: `v1.34.8+orb1`

The working tree was intentionally dirty and no commit or push was performed. This report covers only the signed
Platform/integration routing-authority increment and does not reclassify unrelated Stage 4 evidence.

## Scope

The increment adds:

- strict `SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON` public-key configuration;
- exact publisher, key-rotation, Target ownership, health, and source/destination DR-domain scopes;
- Ed25519 schema-v1 signing with canonical UUID/timestamp/JSON rules and a bounded request window;
- `PUT /v1/platform/routing-authority/execution-targets/{executionTargetID}/observations` without user/session authority;
- one-transaction health + multiple DR-readiness update and immutable nonce receipt;
- exact replay, nonce conflict, stale-row whole-bundle rollback, and receipt response-integrity validation;
- tenant API exclusion for authority assigned to a Platform publisher and managed-Kubernetes health dual-writer rejection;
- SQLite safety triggers, PostgreSQL Migration `000083`, bounded metrics, and the `routing-authority-sign` reference tool.

The Control Plane configuration contains only public keys. The signer private key remains in the external publisher.

## Source identity

| Source | SHA-256 |
| --- | --- |
| `000083_platform_routing_authority_publications.sql` | `8ecd54b14837bbb4f42662735e3dcb569e6931f12160b5aea976bba5edfd0c51` |
| `internal/routing/platform_authority.go` | `5bf04a33df7d168dae8a983da9a8b178bd8a860d58272785e7a0d619be84decb` |
| `internal/config/platform_routing_publishers.go` | `3e7a106eb62055385db9614dc9a8bc45172caabc316b493b5a7b6e5af2b5745f` |
| `internal/persistence/platform_routing_authority_models.go` | `afcbd40952f084f2fa6d0a70834d53668f8da49fde797b5e2f8a98bb67d98256` |
| `internal/database/platform_routing_authority_sqlite.go` | `29e8d902e91cf34ad04e5729f2f4217b50ee2cc666a3a223fe3870b8721c3ebe` |
| `internal/routing/platform_authority_test.go` | `19f9b39d62ee9d5adc26ee09bda9feede00fb9d4549430c386a72598bbe597b1` |
| `internal/routing/platform_authority_postgres_integration_test.go` | `9aecd9ae26f21cdd61835c8baae0800638ddd1e69e80cc57b7efc3bfaa9e895b` |
| `internal/routing/platform_authority_http_integration_test.go` | `bd83d9a2bed980248d4447ae017d194cc702dec6b77b34b87126ddc67e2b7b19` |
| `cmd/routing-authority-sign/main.go` | `b48cf17651b7c1bb9daade3f8889c293416b74f8b66a824c1e910f464c0308fc` |
| `internal/observability/distributed_routing_billing_metrics.go` | `61b61cdbc3750686f880de49179578beb8d3afb844463f84582e6762ac8cf865` |

## SQLite and package-level results

Focused tests proved valid signing/scope, bad-signature rejection, runtime owner mismatch, overlapping-config rejection,
exact replay after the request window, immutable receipt UPDATE/DELETE rejection, and whole-bundle rollback when health
would advance but one readiness row is stale. The HTTP test proved that no tenant Login Session is required or accepted as
publisher authority and that a tenant write cannot overwrite configured Platform health authority.

The reference signer test covered PKCS#8 Ed25519 input, exact signature parity with the server encoder, public-key output,
strict unknown-field and existing-signature rejection, protected key permissions, default symlink rejection, and explicit
read-only projected-Secret symlink opt-in.

## OrbStack PostgreSQL transaction run

Owned namespace: `synara-stage4-routing-auth-20260726-final1` (deleted after the run).  
Database: PostgreSQL `17.10` in an OrbStack Pod.

Command under test:

```text
SYNARA_TEST_DATABASE_URL=<ephemeral PostgreSQL URL> \
  go test ./internal/routing \
  -run '^TestPlatformAuthorityPostgresSerializesReplicaReplayAndProtectsReceipt$' \
  -count=1 -v
```

Result: PASS. Two independent service instances and database connections submitted the exact signed bundle concurrently.
The assertion required exactly one first acceptance and one replay. PostgreSQL held:

- Migration `83`, name `platform_routing_authority_publications`, checksum prefix `8ecd54b14837`;
- one receipt for one Target and one request digest;
- health `healthy / available`, ceiling `12`, allocated `4`, publisher source, version `1`;
- exact source/destination readiness with Artifact/Checkpoint/Memory all ready, publisher identity, version `1`;
- trigger rejection of direct receipt UPDATE and DELETE.

The namespace and port-forward were removed and absence was verified.

## OrbStack two-replica HTTP run

Owned namespace: `synara-stage4-routing-http-20260726-final1` (deleted after the run). The final topology contained:

- two current Control Plane Pods from image digest
  `sha256:0c8a335d9f74021f118f9eabfb580ba97dd9d75ac7a424fef6d61f528e5fbf00`;
- one PostgreSQL `17.10` Pod;
- one pinned MinIO Pod with an ephemeral test bucket;
- two Ready EndpointSlice addresses for the Control Plane Service;
- the RFC 8032 test public key and exact acceptance Target/DR scope; no production key material.

The generic Service port-forward was first observed to pin all eight TCP connections to one backend, so it was not used
as evidence of cross-replica execution. The successful evidence used one direct port-forward per Pod and one fixed signed
body:

1. Pod A: exactly `1` first acceptance and `7` replays.
2. Pod B: the same nonce/body produced `8/8` replays.
3. Pod A was deleted. The first post-delete request did not reach the application because the independent Pod B
   `kubectl port-forward` also exited with `lost connection to pod`; this harness transport interruption is recorded as a
   failed attempt and is **not** counted as API availability evidence.
4. After recreating the direct tunnel, surviving Pod B produced `8/8` replays.
5. The Deployment-created replacement Pod produced `8/8` replays of the same receipt.

The final database state contained two receipts total from the random and fixed test publications, exactly one receipt for
the fixed nonce, health version `2`, readiness version `2`, and no duplicate receipt. Both survivor and replacement
reported identical metrics:

```text
synara_platform_routing_publications_total 2
synara_platform_routing_publication_latest_timestamp_seconds 1785067267.191784
```

Both final Control Plane Pods were `Ready=true`, `Running`, with `0` restarts. The namespace, all port-forwards, and the
temporary local image tag were deleted; their absence was verified.

An initial MinIO bootstrap call raced server readiness and received connection refused; the bounded retry created the
bucket before the successful two-replica run. Earlier Control Plane starts without the local S3 endpoint failed closed
while checking the Artifact bucket and are not counted as successful replicas.

## Regression checks

- `go test ./... -count=1`: PASS across the Control Plane module.
- `go test ./cmd/routing-authority-sign -count=1`: PASS after the signer follow-up.
- `git diff --check`: PASS.
- `bun fmt`, `bun lint`, and `bun typecheck`: not run; the user did not request the heavyweight Bun checks.

## Evidence boundary

This is E3. It proves real OrbStack Kubernetes scheduling, two Control Plane processes, shared PostgreSQL authority,
signed HTTP ingestion on both replicas, replica deletion/recreation, immutable receipts, and exact replay. OrbStack is a
single-node local cluster; PostgreSQL and MinIO shared the same local failure domain. The publisher observation was an
acceptance driver, not a production cloud probe.

It does **not** prove independent Region/AZ failure domains, real external Target measurement, Artifact/Checkpoint/Memory
replication, successor consumption, cloud Workload Identity, cloud IAM/RBAC, provider-account billing export, measured
production RPO/RTO, or a production-duration soak. Those remain E4 gates.

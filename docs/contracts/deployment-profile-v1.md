# Deployment Profile v1 contract

Deployment Profile describes how the control plane and its infrastructure are deployed. It never
describes where an Agent executes; that is the independent Execution Target contract.

## Profiles and capabilities

| Capability                 | `personal`                      | `single-node`                  | `enterprise`                   |
| -------------------------- | ------------------------------- | ------------------------------ | ------------------------------ |
| Metadata store             | SQLite                          | PostgreSQL                     | PostgreSQL                     |
| Artifact store declaration | local                           | MinIO or S3                    | S3                             |
| Queue declaration          | in-process                      | PostgreSQL outbox              | PostgreSQL outbox or external  |
| Replicas                   | 1                               | 1                              | 2+                             |
| High availability          | no                              | no                             | yes                            |
| Identity baseline          | deterministic local owner       | local/dev or future OIDC       | future OIDC/SAML/SCIM          |
| Execution target kinds     | local, SSH, Docker, Kubernetes  | local, SSH, Docker, Kubernetes | local, SSH, Docker, Kubernetes |
| Metadata export/import     | export                          | import                         | import                         |
| Artifact payload migration | export Local payload references | import to MinIO/S3             | import to S3                   |

Execution target support is intentionally the same across profiles. Availability of a driver or
worker pool is operational capability, not a reason to merge the two dimensions.

## Configuration

Safe/public fields exposed by `GET /v1/platform/profile`:

- `profile`
- `metadataStore`
- `artifactStore`
- `queueDriver`
- `controlPlaneReplicas` and `highAvailability`
- `leaseEnabled` and `fencingEnabled`
- supported `executionTargetKinds`
- boolean metadata/artifact migration capabilities

The endpoint never exposes database URLs, SQLite paths, local artifact paths, object-store
credentials, queue credentials, encryption keys, registration tokens, or installation secrets.

Primary environment variables:

```text
SYNARA_DEPLOYMENT_PROFILE
SYNARA_METADATA_STORE
SYNARA_ARTIFACT_STORE
SYNARA_QUEUE_DRIVER
SYNARA_CONTROL_PLANE_REPLICAS
SYNARA_WORKER_LEASES_ENABLED
SYNARA_WORKER_FENCING_ENABLED
SYNARA_DATABASE_URL
SYNARA_SQLITE_PATH
SYNARA_ARTIFACT_LOCAL_PATH
SYNARA_ARTIFACT_BUCKET
SYNARA_ARTIFACT_REGION
SYNARA_ARTIFACT_ENDPOINT
SYNARA_ARTIFACT_PUBLIC_ENDPOINT
SYNARA_ARTIFACT_ACCESS_KEY_ID
SYNARA_ARTIFACT_SECRET_ACCESS_KEY
SYNARA_ARTIFACT_SESSION_TOKEN
SYNARA_ARTIFACT_USE_PATH_STYLE
SYNARA_ARTIFACT_PRESIGN_TTL
SYNARA_ARTIFACT_MAX_UPLOAD_BYTES
SYNARA_INSTALLATION_ID
```

Invalid booleans, durations, integers, enum values, and profile combinations are startup errors.

## Illegal combinations

- SQLite with more than one control-plane replica.
- Local artifacts with more than one control-plane replica/node.
- In-process queue with more than one control-plane replica.
- `personal` without SQLite, local artifacts, in-process queue, or exactly one replica.
- `single-node` without PostgreSQL, MinIO/S3, PostgreSQL outbox, or exactly one replica.
- `enterprise` without PostgreSQL, S3, PostgreSQL outbox/external queue, or multiple replicas.
- Any v1 configuration that disables execution leases or generation fencing.
- Persisted profile changes. Personal upgrades use explicit export/import.

Artifact metadata, Local/MinIO/S3 payload lifecycle, verified presigned uploads/downloads, and
reentrant Local-to-object-storage migration are implemented by the Artifact v1 contract.

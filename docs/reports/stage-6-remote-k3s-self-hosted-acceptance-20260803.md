# Stage 6 remote K3s self-hosted acceptance — 2026-08-03

## Scope and assessment

This report records a real deployment on `103.217.189.80`, not a local-only Compose
exercise. The deployment is an isolated, internal validation environment in the
`synara-stage6-remote` namespace. It is not a production rollout or GA approval.

Assessment: `remote-k3s-internal-self-hosted-usage-cost-runtime-validated`.

## Remote subject

- Host: `7ES-1005` / `103.217.189.80`
- OS/architecture: Debian GNU/Linux 12 (bookworm), `x86_64`
- K3s: `v1.36.1+k3s1`; containerd `2.2.3-k3s1`
- Capacity observed before deployment: 40 vCPU, about 60 GiB available memory,
  26 GiB available on `/`
- Existing namespaces/resources were left untouched: `sandbox-system`,
  `synara-stage-c-runtime`, and `synara-system`
- No Docker daemon was installed; images were imported through the existing K3s
  containerd/private registry path

## Deployed resources

Namespace `synara-stage6-remote` contains one replica each of:

- PostgreSQL 17 with a `local-path` 2 GiB PVC
- MinIO `RELEASE.2025-04-22T22-12-26Z` with a `local-path` 2 GiB PVC
- Control Plane, migration schema `162`
- Tenant Web
- Platform Admin

All five services are ClusterIP-only. Access was performed through SSH tunnels;
no NodePort, LoadBalancer or public ingress was created.

The deployment is reproducible with
[`deploy/kubernetes/remote-stage6-acceptance.sh`](../../deploy/kubernetes/remote-stage6-acceptance.sh).
The runner refuses an existing unlabelled namespace, requires immutable
application image digests by default, generates authorities without printing
them, and adds a 60-attempt/5-second Control Plane `startupProbe` so schema
migrations are not mistaken for a dead process. It keeps all Services ClusterIP
and deletes only a namespace created by that invocation unless `--keep` is set.

Images observed in the remote containerd store:

```text
10.77.0.2:5001/synara/stage6-control-plane:20260803
  sha256:fc567fab1b8788393da69e54b9de87a3e2adacd0258d137976f3f102b28e76de
10.77.0.2:5001/synara/stage6-node:20260803
  sha256:7cfe291015e2335a1f60c23b5a7dd647bb81d31afc25e364ac232700a57471c8
10.77.0.2:5001/synara/postgres:17-alpine
  sha256:f7b22dedcd41ec51e5a1abd50e81616ca0a1b317bdddde2722721aae53bf614e
10.77.0.2:5001/synara/minio:RELEASE.2025-04-22T22-12-26Z
  sha256:e4cfd69993796b860f18a72eee502c1f8a12f2e0ed00ee1b78ddc53dffa5146e
```

The remote registry is reached through the host's existing `10.77.0.2:5001`
endpoint. The Synara Web/Admin image is an amd64 Node runtime assembled from the
current worktree; Admin dev-login is enabled only for this disposable acceptance.

## Remote checks

Control Plane `/ready` returned `200` with:

```text
database=ready (postgresql)
databaseWrite=ready
artifactStore=ready (minio)
queue=ready (postgres-outbox)
schema=ready expectedVersion=162 appliedVersion=162
```

Web `/ready` and `/` returned `200`, and the response contained the Synara shell.
Admin `/ready` returned `200`; `/admin-config.json` returned the internal Web handoff
origin. The served Admin bundle contained the intended Platform Admin and Internal
status labels plus the acceptance-only Development bootstrap form.

Through the real remote Control Plane API and Admin same-origin proxy:

- Dev login succeeded and produced an authenticated Web session.
- A self-service Tenant was created and selected through the API.
- Tenant usage returned `200` with token counters, execution time, provider/platform
  cost maps and no external invoice data.
- Tenant internal cost accounting report returned `200` with the same internal-only
  cost boundary and no payment fields.
- A dedicated disposable Platform Operator Tenant was selected; Platform Tenant
  inventory returned `200` with `operatorRole=owner`, and
  `/v1/platform/internal-cost-reviews` returned `200` with an empty list.
- `GET /v1/billing/checkout` returned `404 page not found` both through Control
  Plane and the Admin proxy; no active checkout route was exposed.

The environment initially exposed the expected fail-closed Platform Operator
response (`403`) before switching to the configured disposable operator Tenant.
This confirms the active-Tenant boundary rather than bypassing it.

## Reproducibility pass

On the same host, the runner was executed against the separate
`synara-stage6-repro-20260803` namespace with the Control Plane and Node image
digests listed above. The first run intentionally exposed two image/runtime
assumptions (the Node image has no default command and PostgreSQL must retain its
official entrypoint); both were corrected in the checked-in runner. The final run
reported five Ready Pods, verified all five Services as `ClusterIP`, and returned
`200` from Control Plane `/ready`, Web `/ready`, and Admin `/ready`. The temporary
namespace was then deleted and `synara-stage6-remote` was rechecked healthy, so the
reproducibility exercise did not mutate the retained evidence namespace.

## Validation profile and limits

The deployment uses `single-node` metadata/profile semantics inside a private,
loopback-tunnelled K3s namespace. The checked-in enterprise Kustomize base expects
operator-managed HTTPS ingress, external S3/workload identity and a real IdP, so
those production authorities were not fabricated for this acceptance. HTTP is
explicitly limited to the SSH-tunnel validation path; `SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP=true`
is not a production identity configuration.

Not established by this run: enterprise IdP/MFA, TLS termination and certificate
rotation, production KMS/workload identity, public DNS/ingress, real Status Board and
on-call delivery, backup restore, long-duration SLO/capacity evidence, independent
penetration testing, compliance sign-off, image signing/attestation, or formal GA
approval. The namespace and PVC-backed validation stack are intentionally left
running for follow-up remote testing and can be removed with:

```bash
kubectl delete namespace synara-stage6-remote
```

That command is deliberately not run by this acceptance so the remote environment is
available for the next verification pass.

# Stage 7 Developer Docs Container Acceptance — 2026-08-05

## Scope and verdict

The immutable Polaris Developer Docs container and its non-publishing Kubernetes base passed local acceptance.
This proves the repository can build and run a bounded deployment artifact; it does not prove a public origin,
certificate, DNS route, external availability or production approval.

## Direct evidence

- Docker built the root `Dockerfile` target `developer-docs-runtime` with Bun `1.3.14` and Node 24 images pinned
  by digest.
- The image build generated 41 public-beta OpenAPI operations, 12 Astro pages and the Redocly API Reference.
- The runtime started as the image's `node` user with a read-only root filesystem, all Linux capabilities dropped
  and `no-new-privileges` enabled.
- `/`, `/getting-started/`, `/api-reference/` and `/ready` returned HTTP 200 from the running container.
- `/ready` returned `{"status":"ready","revision":"local-uncommitted"}` and the OCI revision label contained
  the same candidate identifier.
- The local acceptance image was `sha256:5341bbf1088ad5b4f04ba8bff1ab3abaef6e7237658f94758e4b6dfb93e8a8d1`.
- `kubectl kustomize deploy/kubernetes/developer-docs` rendered a two-replica Deployment, ClusterIP Service and
  PodDisruptionBudget in `synara-system` without mutating a cluster.

The temporary acceptance container and local image tag were removed after verification. CI repeats the image build,
runtime route probes, revision binding, non-root user and read-only-root checks for each candidate.

## Publication boundary

The Kustomize base deliberately excludes `ingress.example.yaml`. The example uses the reserved `.invalid` domain and
placeholder ingress class/TLS Secret. Publication remains blocked until the deployment authority supplies an approved
hostname, ingress path, certificate, immutable Registry digest and Console HTTPS origin, then records externally
observed HTTPS and availability evidence.

# Polaris developer documentation

This Astro site is the server-side Developer API entry point. Its guide pages are authored in
`src/content.ts`; the API Reference is generated at build time from a filtered view of
`docs/api/openapi.yaml`.

Only operations marked `x-synara-contract-status: codegen-ready` are included in the public reference.
The complete OpenAPI source also contains the internal route-surface inventory used by Control Plane
conformance tests, so serving that source directly would expose an internal classification artifact. The
current public-beta source has no `route-only` operations, but the filter remains fail-closed for future work.

```sh
bun run --cwd apps/developer-docs dev
bun run stage7:docs:build
```

Set `PUBLIC_POLARIS_CONSOLE_URL` at build time to point the **Open Console** action at the deployed Tenant
Console. The default `/console` value is intended only for same-origin previews.

## Immutable container

Build the static site and its non-root Node server from the repository root:

```sh
docker build \
  --target developer-docs-runtime \
  --build-arg PUBLIC_POLARIS_CONSOLE_URL=https://console.example.invalid \
  --build-arg POLARIS_DEVELOPER_DOCS_REVISION="$(git rev-parse HEAD)" \
  -t polaris-developer-docs:local .
```

The runtime listens on port `8080`, serves only `GET`/`HEAD`, exposes a non-cached `/ready` response, redirects
directory paths to their trailing-slash canonical URL, sends immutable caching only for Astro fingerprinted assets,
and rejects dot, traversal and malformed paths. `POLARIS_DEVELOPER_DOCS_REVISION` appears in `/ready`; it must bind
the immutable source revision or image digest used by the deployment evidence.

The Kubernetes base is in [`deploy/kubernetes/developer-docs`](../../deploy/kubernetes/developer-docs/README.md).
It deliberately creates no Ingress, DNS record or certificate.

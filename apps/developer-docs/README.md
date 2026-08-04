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

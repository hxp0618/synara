# Synara Platform Admin

`@synara/admin` is the independently built authority surface for cross-Tenant Platform operations.
It is not a customer Settings route and must never be bundled into Desktop.

## Local development

Start an isolated Control Plane, then run:

```bash
VITE_CONTROL_PLANE_PROXY_TARGET=http://127.0.0.1:3780 \
VITE_TENANT_APP_URL=http://127.0.0.1:5733 \
VITE_ENABLE_DEV_LOGIN=true \
bun run --cwd apps/admin dev
```

`VITE_ENABLE_DEV_LOGIN` only exposes the local bootstrap form. Keep it absent or `false` in shipped
artifacts. Production identity discovery uses the dedicated Operator Tenant's OIDC/SAML connection.
The Control Plane maps the fixed Platform Admin SSO return marker only to
`SYNARA_PUBLIC_ADMIN_URL`; it does not accept an arbitrary absolute return URL.

## Runtime boundary

`server.mjs` serves the production build, returns the customer-Web handoff URL through a no-store
runtime config response, and proxies same-origin `/v1` requests to `SYNARA_CONTROL_PLANE_URL`.
Configure:

- `SYNARA_ADMIN_HOST` / `SYNARA_ADMIN_PORT`;
- `SYNARA_CONTROL_PLANE_URL` for the internal Control Plane address;
- `SYNARA_TENANT_APP_URL` for the browser-reachable customer Web origin.

The Admin app first authenticates the cookie session, then requires the configured Operator Tenant to
be active and rechecks the Platform role through `GET /v1/platform/tenants`. Owner/Admin roles receive
provisioning, entitlement and Support decision workflows. `security_admin` receives aggregate triage
and Support request only. When `support_readonly` is active, global Platform workflows are not mounted.

## Verification

```bash
bun run --cwd apps/admin test
bun run --cwd apps/admin test:browser
bun run --cwd apps/admin build
```

The source matrix proves registration and route wiring only. GA still requires all 27 operations to be
exercised on the candidate deployment with role-separated accounts, denied-role results and matching
Audit/request evidence.

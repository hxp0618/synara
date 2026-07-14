# Production Authentication Policy

This contract defines the deployment-profile authentication boundary for the Go Control Plane.

## Profile policy

| Deployment profile | Authentication policy                                                                                                          |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------ |
| `personal`         | The deterministic local owner may use explicit local login. The installation remains single-replica.                           |
| `single-node`      | OIDC or SAML is the production path. Dev Bootstrap may be enabled only for a controlled development or acceptance environment. |
| `enterprise`       | OIDC/SAML login and SCIM provisioning are supported. Dev Bootstrap is rejected during startup.                                 |

Enterprise startup also requires `SYNARA_PUBLIC_CONTROL_PLANE_URL`. A non-loopback public URL must use
HTTPS and requires a Secure login cookie. `SameSite=None` also requires a Secure cookie.

## Public URL and proxy trust

- OIDC/SAML callback URLs are derived from `SYNARA_PUBLIC_CONTROL_PLANE_URL`.
- A configured public URL cannot contain user info, query parameters, or a fragment.
- Without a configured public URL, callback derivation is allowed only when both the direct peer and the
  request Host are loopback. `X-Forwarded-Proto` is not used for this fallback.
- `X-Forwarded-For` is accepted only when the direct peer belongs to
  `SYNARA_TRUSTED_PROXY_CIDRS`. The chain is evaluated from right to left while each hop remains trusted.
- An empty trusted-proxy list records the direct peer address and ignores forwarded client addresses.

## Login cookie

The login cookie is always `HttpOnly`. The following settings are explicit and validated at startup:

```text
SYNARA_LOGIN_COOKIE_NAME
SYNARA_LOGIN_COOKIE_SECURE
SYNARA_LOGIN_COOKIE_DOMAIN
SYNARA_LOGIN_COOKIE_PATH
SYNARA_LOGIN_COOKIE_SAME_SITE
```

Login, logout, session lookup, SSO callbacks, and authenticated API responses use
`Cache-Control: no-store`. Logout clears the cookie with the same Domain, Path, SameSite, and Secure
attributes used when it was issued.

## Session lifecycle

- Every successful login creates a new random token and Login Session ID; an existing browser token is
  never adopted as the authenticated session.
- Only the SHA-256 token hash is persisted.
- `SYNARA_LOGIN_SESSION_TTL` is the absolute lifetime.
- `SYNARA_LOGIN_SESSION_IDLE_TTL` is the maximum idle lifetime and cannot exceed the absolute lifetime.
- `last_seen_at` is refreshed at a bounded interval of `min(5m, idleTTL/2)` rather than on every request.
- Logout and administrator revocation update the authoritative PostgreSQL Login Session row. Other
  Control Plane replicas reject that token on their next authentication lookup.

Tenant owners, administrators, and security administrators with `identity.sessions.revoke` may call:

```text
POST /v1/tenants/{tenantID}/members/{userID}/revoke-sessions
```

The operation revokes only sessions whose active Tenant is the requested Tenant, returns
`revokedCount`, and writes the `identity.sessions_revoked` audit action. It does not revoke the target
user's sessions in unrelated Tenants.

## Verification evidence

- Unit tests cover profile fail-fast validation, cookie attributes, hostile Host/proxy headers, idle and
  absolute expiry, token rotation, and administrator revocation audit behavior.
- PostgreSQL integration tests use separate connection pools to cover idle expiry, cross-replica
  administrator revocation, and concurrent Authenticate/Revoke without session resurrection.
- `deploy/saas/multi-replica-acceptance.sh` validates cross-replica logout/rejection in the same runtime
  topology used for SSE, Turn, Execution Claim, and replica-failure acceptance.

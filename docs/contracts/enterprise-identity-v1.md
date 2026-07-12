# Enterprise identity contract v1

## OIDC

Tenant-scoped OIDC connections use discovery, authorization-code flow, PKCE S256,
state and nonce validation, signed ID-token verification, verified email claims,
allowed-domain checks, and explicit Group-to-role mappings. Client secrets and login
attempt payloads use the same KMS envelope encryption boundary as Provider Credentials.

Existing stronger Tenant roles are never downgraded by SSO login. Group mappings can
grant a Tenant fixed role and/or an Organization fixed role. Metrics and logs never use
identity claims or subject IDs as labels.

## SAML

Tenant-scoped SAML connections fetch and validate IdP metadata, derive or verify the IdP
issuer, and generate a dedicated RSA service-provider keypair. The private key and
certificate bundle is KMS envelope encrypted; only the certificate is published through
the SP metadata endpoint.

SP-initiated login uses a signed HTTP-Redirect AuthnRequest with RSA-SHA256 and an
HTTP-POST assertion consumer callback. Login attempts store a hashed one-time RelayState
and an encrypted AuthnRequest ID. The callback requires a signed response or assertion
and validates `InResponseTo`, Destination, Audience, Issuer, time conditions, subject,
email, allowed domains, and exact Group-to-role mappings before creating a Synara session.
Replayed RelayState values are rejected.

The public SP metadata endpoint is:

```text
GET /v1/auth/sso/{connectionId}/metadata
```

IdP metadata URLs must use HTTPS. Loopback HTTP is accepted only for local development
and end-to-end testing, and redirects cannot downgrade to an unsafe URL. SAML private
keys, raw assertions, RelayState values, and subject identifiers are not written to Audit
Log metadata or metric labels.

## Service Accounts and SCIM

Service Accounts are Tenant-scoped machine identities with explicit scopes. Tokens are
shown once, stored only as SHA-256 hashes, throttled when updating `last_used_at`, and
invalidated atomically on rotation or revocation.

SCIM v2 supports User and Group list/get/create/replace/patch/delete operations through
`/scim/v2`. User deactivation suspends the Tenant membership and Organization access,
but cannot suspend the final active Tenant Owner. Group members must be active members
of the same Tenant. Every mutation writes an Audit Log with actor type
`service_account`.

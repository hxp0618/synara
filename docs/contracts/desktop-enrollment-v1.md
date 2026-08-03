# Desktop Enrollment v1

- Status: frozen for Stage 6 implementation
- Authority: Synara Control Plane
- Surfaces: `apps/admin`, `apps/desktop`, customer Web settings
- Secret-bearing values: never logged, audited, persisted in browser storage, or exposed to the renderer

## Purpose

Desktop Enrollment connects one installed Synara Desktop to one named Synara user at one Control
Plane. It does not copy a browser session, create Tenant Membership, grant Platform authority, or
silently choose a runtime on the user's behalf.

The first launch with no persisted mode blocks backend startup until the user explicitly chooses `local` or `cloud`.
Neither a protected Cloud credential, a local SQLite file, nor an OS-dispatched Enrollment link may infer that choice.
Later launches restore only the persisted mode. An Enrollment link opened while `local` is active requires an explicit
confirmation before switching to `cloud`; choosing or retaining `local` performs no Cloud request. Switching to `local`
removes Cloud injection and proxy authorization without deleting the protected Cloud credential. Switching back to
`cloud` revalidates and rotates the saved connection before use. Explicit disconnect remains the only UI action that
revokes and deletes the Cloud device credential. Local projects and the local runtime remain available in either mode.
The Desktop commits a newly selected mode only after its startup initialization succeeds. If the saved Cloud connection
cannot be unlocked because the OS credential store is unavailable or damaged, startup offers `local`, retry, or quit and
never overwrites the saved `cloud` choice before the user decides. A normal Control Plane outage keeps the saved mode and
surfaces a disconnected/error state instead of silently converting the install into first-use setup.

## Authority and safe issuance

An authenticated Platform Owner or Admin may request an Enrollment from an explicitly selected
customer Tenant and subject user. Issuance revalidates all of the following in one server-side
transaction:

- the issuer has a Web-audience session in the configured Platform Operator Tenant;
- the session is not a Support Access session;
- the Tenant is internally persisted as evaluation-compatible or active and is not the Operator Tenant; public API/UI lifecycle
  vocabulary is `evaluation | active`;
- the subject user is active and has an active Membership in the customer Tenant;
- an optional Organization belongs to that Tenant, is active, and has an active Membership for the
  subject;
- the requested authority is safe for automatic redemption.

V1 automatically issues only self-enrollment, where `issuedByUserId == subjectUserId`. An unverified
cross-user request returns `desktop_enrollment_subject_authentication_required` and never returns or
persists a redemption handle. A later managed-device authority may add `managed_device` only after a
separate MDM, device-certificate, or equivalent binding contract is established. Platform role alone
is not such an authority.

Issuance never creates or repairs Membership. A separately authorized provisioning transaction must
complete before issuance.

## Enrollment state

The authoritative record contains at least:

```text
DesktopEnrollment
  id
  secret_hash
  status = pending | redeemed | expired | revoked
  version
  mode = connect_existing | provisioned_then_connect
  authority = self
  control_plane_origin
  issued_by_user_id
  issued_by_session_id
  subject_user_id
  tenant_id
  organization_id?
  membership_version_snapshot
  expires_at
  redeemed_at?
  redeemed_device_id?
  revoked_at?
  terminal_reason?
  created_at
  updated_at
```

`membership_version_snapshot` is the canonical UTC RFC 3339 nanosecond representation of the
Membership `updated_at`, prefixed by the role and status. Redemption requires an exact match and
therefore fails closed after role, status, or Membership mutation.

The raw 256-bit redemption handle is returned once. Only its SHA-256 digest is stored. The configured
lifetime must be between one and five minutes; the Stage 6 default is three minutes. `expired`,
`revoked`, and `redeemed` are terminal. One conditional update inside the redemption transaction is
the single consumption authority.

## Desktop link

The packaged fallback link is:

```text
synara://connect?v=1&control_plane=<percent-encoded-public-base-url>&enrollment=<opaque-handle>
```

Canary uses `synara-canary://connect`. The parser accepts only:

- the active application scheme;
- host `connect` and path empty or `/`;
- exactly one `v=1`, `control_plane`, and `enrollment` parameter;
- a normalized HTTPS Control Plane public base URL, optionally with one fixed deployment path,
  without credentials, query, or fragment;
- loopback HTTP only in an explicitly enabled development build;
- an origin present in the packaged/deployment allowlist;
- a base64url handle of the frozen length.

Unknown or duplicate parameters fail closed. Parsing must occur before changing the current
connection or local state. The preferred claimed HTTPS/universal-link association can translate to
the same parsed value after platform association is shipped and verified.

The Admin page keeps the raw link only in current page memory. It may open or copy that value while
the page remains alive, but a reload cannot recover it from the server. A new Enrollment is required.

## Device identity and redemption proof

Desktop generates an Ed25519 key pair. The private key is protected by macOS Keychain, Windows DPAPI
through Credential Manager-compatible OS protection, or a supported Linux Secret Service backend.
Linux `basic_text` storage is not accepted for Cloud Panel credentials.

Redemption uses:

```http
POST /v1/desktop-enrollments/redeem
Content-Type: application/json

{
  "version": 1,
  "controlPlaneOrigin": "https://control.example.com",
  "enrollment": "<opaque-handle>",
  "devicePublicKey": "<base64url raw 32-byte Ed25519 key>",
  "nonce": "<base64url 32 random bytes>",
  "proof": "<base64url 64-byte Ed25519 signature>",
  "platform": "darwin | win32 | linux",
  "appVersion": "<Synara semantic version>",
  "deviceLabel": "<bounded user-safe device label>"
}
```

The signature covers the UTF-8 bytes of these five lines with `\n` separators and no trailing
newline:

```text
synara.desktop-enrollment.redeem.v1
<normalized control-plane public base URL>
<opaque enrollment handle>
<base64url device public key exactly as sent>
<base64url nonce exactly as sent>
```

The endpoint is narrowly unauthenticated: the one-time handle is its credential. It is rate-limited,
returns non-enumerating failures, and cannot authorize any other route. Proof validation happens
before consumption, while subject, Membership, Organization, Tenant status, expiry, revocation, and
deployment origin are revalidated inside the consuming transaction.

Success returns once:

```json
{
  "audience": "desktop",
  "credential": "<opaque refresh credential>",
  "credentialExpiresAt": "<RFC3339>",
  "sessionId": "<uuid>",
  "credentialFamilyId": "<uuid>",
  "device": { "id": "<uuid>", "status": "active" },
  "defaultTenantId": "<uuid>",
  "defaultOrganizationId": "<uuid-or-null>"
}
```

The credential is a Desktop-audience Bearer credential and is never set as the Admin or customer Web
cookie. Desktop stores it with the same OS protection as the private key. Renderer requests reach the
local Synara proxy; the Electron main process injects the credential at the network boundary. The
renderer and local server process never receive the plaintext credential through IPC, localStorage,
SQLite, command arguments, or environment variables.

Desktop fetches the authenticated Session, Tenant, Organization, and entitlement/feature snapshot
before committing the new connection. Failure preserves the previous connection and all
local state. A consumed Enrollment is never retried as a new session.

## Desktop session and rotation

Desktop sessions have:

- `audience = desktop`;
- `auth_method = desktop`;
- one `desktop_device_id`;
- one `credential_family_id`;
- an ordinary active Tenant selected from the subject's current Memberships;
- no Platform or Support authority, even if the same user has a Platform Membership in another
  browser context.

Cookie authentication accepts only Web-audience sessions. Bearer authentication accepts only
Desktop-audience sessions. Supplying both is rejected as ambiguous.

Rotation is atomic. A successful rotation revokes the presented session and creates one successor in
the same credential family. If a rotated credential is presented again, the Control Plane marks
replay and revokes every active session in that credential family before returning an authentication
failure. Device revocation does the same family-wide revocation before returning success.

The rotation proof uses the device key and a fresh nonce; its canonical payload is:

```text
synara.desktop-session.rotate.v1
<normalized control-plane public base URL>
<device id>
<current session id>
<base64url nonce exactly as sent>
```

## API surface

Platform Web-audience routes:

- `GET /v1/platform/tenants/{tenantID}/desktop-access`
- `POST /v1/platform/tenants/{tenantID}/desktop-enrollments`
- `POST /v1/platform/desktop-enrollments/{enrollmentID}/opened`
- `POST /v1/platform/desktop-enrollments/{enrollmentID}/revoke`
- `POST /v1/platform/desktop-devices/{deviceID}/revoke`

Narrow public exchange:

- `POST /v1/desktop-enrollments/redeem`

Desktop-audience routes:

- existing authenticated reads, including `/v1/auth/session` and Tenant hydration reads;
- `POST /v1/desktop-sessions/rotate`;
- `POST /v1/desktop/disconnect`.

Every mutation uses the existing request ID and stable problem response contract. Platform
revocation inputs carry `expectedVersion` and a bounded reason. Redemption failures deliberately do
not distinguish unknown, expired, revoked, consumed, Membership-drifted, or suspended-Tenant handles
to an unauthenticated caller.

## Audit and redaction

Stable actions are:

- `desktop.enrollment_created`
- `desktop.enrollment_opened`
- `desktop.enrollment_redeemed`
- `desktop.enrollment_expired`
- `desktop.enrollment_failed`
- `desktop.enrollment_revoked`
- `desktop.device_connected`
- `desktop.device_session_issued`
- `desktop.device_session_rotated`
- `desktop.device_credential_replay_detected`
- `desktop.device_revoked`
- `desktop.device_disconnected`

Audit metadata may contain Enrollment ID, device ID, credential-family ID, subject, Tenant,
Organization, status, platform, app version, failure class, actor, request ID, IP address, and user
agent. It never contains the raw link, handle, credential, signature, nonce, private key, public-key
bytes, Authorization header, Cookie header, or customer content. Logs and user-facing errors follow
the same rule.

## Failure and rollback requirements

- Malformed or disallowed links do not perform network I/O.
- Unavailable OS credential protection prevents redemption before the handle is consumed.
- Network or proof failure leaves the previous connection unchanged.
- Initial hydration failure deletes the newly returned local credential and preserves the previous
  connection; the server-side new device session is revoked on the best-effort rollback path.
- Membership drift, Organization removal, Tenant suspension, expiry, revocation, or replay fails
  closed.
- Control Plane unavailability never authorizes a cached mutation.
- Disconnect or revocation never removes or hides local projects or stops the local runtime.
- First launch performs no local-backend start or Cloud request before the user chooses a mode.
- Local mode performs no Cloud proxy injection; changing modes is persisted atomically and never implies disconnect.

## Release evidence boundary

Unit and local integration tests prove parser, transaction, authority, redaction, storage-adapter, and
rollback behavior. Stage 6 GA additionally requires signed packaged link-open acceptance on macOS,
Windows, and supported Linux targets, a production-like role-separated browser exercise, real
PostgreSQL concurrent redemption/rotation evidence, and verification of the selected OS credential
store on each target. Source or local evidence alone is not GA approval.

# ADR 0004: Stage 6 enterprise frontend and desktop boundaries

- Status: Accepted
- Date: 2026-07-30
- Scope: Stage 6 Tenant UI, Platform administration, one-click Desktop Enrollment, desktop integration,
  and upstream maintenance

## Context

Synara ships the same React application through a browser deployment and an Electron desktop shell.
The desktop application owns native window behavior, updates, OS integration, and supervision of the
bundled local server; it loads the Web build rather than maintaining a second renderer.

Stage 6 adds internal Tenant administration for lifecycle, membership, identity, credentials,
usage, quota, retention, support policy, and data governance. It also adds Platform-level operations
for enterprise provisioning, entitlements, Tenant triage, Support Access, Workers, queues, Outbox,
and release operations. These two surfaces have different actors and security boundaries:

- Tenant administration is part of the product used by internal Owners, Admins, Security Admins,
  Cost Admins, Auditors, and Members.
- Platform administration is an operator and support surface with cross-Tenant visibility and
  four-eyes workflows. It is not an extension of a customer's ordinary settings page.

The repository is a monorepo fork that regularly integrates `upstream/main`. Putting enterprise UI
inside Electron main/preload code, duplicating browser and desktop renderers, or scattering Stage 6
changes across upstream-owned screens would increase merge conflicts and create inconsistent product
behavior. Splitting the enterprise implementation into a separate repository now would instead add
versioning, release, and local-development coordination before the contracts are stable.

## Decision

Synara will keep one monorepo fork and adopt the following frontend boundaries.

### 1. Keep `apps/desktop` as an upstream-friendly thin shell

`apps/desktop` owns only native desktop responsibilities:

- Electron process and window lifecycle;
- secure preload bridges and narrowly scoped native capabilities;
- application updates, packaging, signing, and platform integration;
- bundled local-server startup, supervision, recovery, and shutdown;
- desktop-only SSO callback or external-browser handoff when an OS integration is required.

It must not own Tenant lifecycle, RBAC, SSO/SCIM policy, Plan, quota, cost accounting, compliance, Support
Access, or Platform administration UI. It must not contain a second implementation of a Web setting.
An enterprise feature that needs a native capability receives that capability through a small typed
adapter; the enterprise business logic remains outside Electron main and preload processes.

### 2. Deliver Tenant administration through a shared enterprise feature

Stage 6 Tenant UI remains part of the React product so browser and desktop users receive the same
behavior. During the first migration step, the feature may live under
`apps/web/src/features/enterprise`. Once its API and composition boundary are stable, it will move to
the workspace package `packages/enterprise-ui` with package name `@synara/enterprise-ui`.

The shared Tenant feature owns:

- SaaS sign-in state and Tenant/Organization context presentation;
- Tenant lifecycle and deletion recovery;
- member lifecycle and fixed-RBAC administration;
- Domain Verification, SSO enforcement, identity connections, and SCIM governance;
- Provider Credential metadata/governance and Service Accounts;
- usage, quota, retention, residency, Legal Hold, privacy/export, and Tenant support policy;
- capability-aware rendering and safe destructive-action workflows;
- feature-level queries, mutations, error presentation, and focused tests.

The package must not depend on Electron. It may depend on shared React/design-system contracts exposed
by the Web host, but it must not reach into Web route internals or import application singletons through
private paths. The host supplies navigation, authenticated client access, runtime capabilities, and
shared visual primitives through explicit public interfaces.

### 3. Extract the typed Control Plane client from the Web application

The browser-capable Control Plane client will move to `packages/control-plane-client` with package name
`@synara/control-plane-client`. It owns:

- typed request and response models for the Control Plane HTTP surface;
- authentication/session transport behavior;
- stable error decoding and request identifiers;
- API methods, cache-key helpers where transport-specific, and event/subscription transport;
- transport-focused tests that do not require the complete Web application.

It must not depend on React, Electron, Web routes, or UI components. `packages/contracts` remains
schema-only, and `packages/shared` remains limited to runtime utilities that are genuinely shared by
server and Web consumers. UI permissions may improve presentation, but authenticated server routes,
database constraints, and domain state machines remain authoritative.

### 4. Create a separate `apps/admin` for Platform operations

`apps/admin` will be an independently built Web application for Platform Operators and Support staff.
It owns cross-Tenant workflows including:

- enterprise Tenant provisioning and versioned Plan/Entitlement management;
- Platform Tenant search, health, and operational-state triage;
- Support Access request, separate approval, expiry, revocation, and read-only Tenant context;
- Worker, execution, queue, Outbox, identity, credential-metadata, and release operations;
- Platform audit navigation and role-separated operational evidence.

`apps/admin` may reuse `@synara/control-plane-client`, shared design primitives, and deliberately public
enterprise components. It must not import `apps/web` route internals. Tenant settings must not become
the Platform console, and the Platform console must not grant Tenant Membership as a side effect of
operator access.

The admin application is independently deployable but remains in the same repository and release
compatibility matrix. Its server authorization is deny-first. Hiding a route, menu, or button is never
an authorization control.

### 5. Keep host integration narrow and explicit

`apps/web` will expose a small static feature-registration seam for enterprise navigation and routes.
The normal local-first product remains usable when no SaaS Control Plane is configured. Enterprise
entries are selected from server-reported availability, authenticated identity, Tenant status,
entitlements, and capabilities; they are not inferred from Electron presence.

The initial dependency direction is:

```text
apps/desktop
  -> loads the apps/web build

apps/web
  -> @synara/enterprise-ui
       -> @synara/control-plane-client

apps/admin
  -> @synara/control-plane-client
  -> selected public enterprise UI and shared design primitives

services/control-plane
  -> authoritative authentication, authorization, state machines, and audit
```

No enterprise package may import from `apps/desktop`. `apps/web` and `apps/admin` must not connect
directly to Worker Pods or bypass the authenticated Control Plane routes.

## Desktop SaaS connection and one-click enrollment

Desktop remains local-first, but an authenticated Platform user may connect an installed Synara
Desktop to SaaS with one action from `apps/admin`. This is a one-time Desktop Enrollment flow, not a
transfer of the Admin browser session and not a permanent replacement of local mode.

After connection, the product model is additive:

```text
Synara Desktop
  -> local projects and local runtime
  -> optional connected SaaS account
       -> active Tenant and Organization
       -> additional authorized Tenants
```

Disconnecting SaaS, revoking a device, or losing Control Plane availability must not delete or hide
local projects. A connected Desktop must not use stale SaaS state to authorize a mutation while the
Control Plane is unavailable.

### Admin experience

`apps/admin` exposes the following actions from an explicitly selected user/Tenant context:

- `Generate and open Synara Desktop`;
- `Open Synara Desktop` for an already-created pending Enrollment;
- `Copy one-time Desktop link` when the link must be transferred to a managed device;
- `Revoke` for a pending Enrollment or connected device.

The primary action asks the Control Plane to create an Enrollment and then invokes the registered
Desktop link. If Synara Desktop is installed, the OS opens it and the application redeems the
Enrollment without another password prompt or confirmation click. If it is not installed, the Web
fallback explains how to install Synara and offers a retry while the server-side Enrollment remains
pending. The raw handle remains only in the current authenticated page memory and is not persisted to
browser storage.

Before creating an Enrollment, Admin must show the exact subject user, Tenant, optional Organization,
effective Membership, expiry, and target Control Plane origin. The issuer confirms this information as
part of the single primary action. A Platform Admin role by itself does not create customer Membership;
the subject must already have an authoritative active Membership, or a separately authorized and
audited provisioning operation must create it before Enrollment issuance.

### Enrollment authority boundary

An Enrollment authorizes connection for one named subject and one Control Plane. It is not a Platform
token and does not contain reusable login credentials.

The safe automatic cases are:

1. the issuing Platform user is connecting their own user identity and has an active Membership in the
   selected Tenant; or
2. an enterprise-managed device/user binding has already been established through an approved MDM,
   device certificate, or equivalent enrollment authority.

Generating a link for another unverified user's unmanaged device is a passwordless-login flow and
must require that user to authenticate with the configured IdP before the first Desktop session is
issued. Platform Admin authority must not silently mint an arbitrary customer's full user session.
Support Access and `support_readonly` context can never be converted into a Desktop Enrollment.

The Desktop receives only:

- the named Synara user identity;
- a device-bound session with `audience = desktop`;
- the selected default Tenant/Organization when still authorized;
- the ordinary capabilities of that user's current Membership and entitlements.

It never receives the Admin cookie, Platform role, Platform session, cross-Tenant visibility, Support
Access grant, or the issuer's browser credentials.

### Enrollment record and lifecycle

The Control Plane persists the Enrollment as authoritative state. The exact persistence model may be
split into enrollment, device, and credential tables, but it must retain at least:

```text
DesktopEnrollment
  id
  secret_hash
  status = pending | redeemed | expired | revoked
  mode = connect_existing | provisioned_then_connect
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
  created_at
```

The raw redemption secret is returned once and never stored. The default lifetime is short—between
one and five minutes—and every Enrollment has exactly one successful redemption. Expiry and revocation
are terminal. A redemption attempt races through one authoritative compare-and-swap or database
transaction so two Desktop processes cannot both consume the same Enrollment.

`connect_existing` revalidates the referenced active Membership during redemption.
`provisioned_then_connect` records that an independent provisioning transaction already established
the Membership; Enrollment redemption still revalidates it and never performs a hidden role grant.

### Desktop link and redemption

The preferred link is a claimed HTTPS/universal link when the platform supports secure app
association, with a registered `synara://connect` protocol as the packaged-desktop fallback. The link
contains only an opaque, unpredictable, single-use redemption handle—not an Admin session, access
token, refresh token, or reusable JWT.

The Desktop handler must allow only the documented connect route and Control Plane origins permitted
by product/deployment policy. It must redact the handle from application logs, crash reports,
telemetry, process diagnostics, recent-item storage, and UI error messages.

On receipt, Desktop:

1. parses and validates the link without changing current local or SaaS state;
2. creates or loads a device key pair protected by the OS credential store;
3. sends the one-time handle, device public key, nonce, platform, and application version to the
   Control Plane over TLS;
4. proves possession of the device key when required by the redemption contract;
5. waits for the Control Plane to atomically consume the Enrollment, revalidate issuer/subject,
   Membership, Tenant status, expiry, revocation, and deployment policy, and issue a Desktop-audience
   device session;
6. stores the refresh credential in macOS Keychain, Windows Credential Manager, or the supported Linux
   secret store—never localStorage, SQLite, logs, or renderer-readable plaintext;
7. fetches the authenticated Session, Tenant, Organization, entitlement, and capability snapshot;
8. marks the UI connected only after the complete snapshot succeeds.

If redemption or initial hydration fails, Desktop preserves its previous connection and local state,
shows a bounded error, and never partially switches the active Tenant. Retrying a consumed Enrollment
does not issue another session; the Admin must create a new Enrollment.

### Session, device, revocation, and audit

The issued Desktop session is separate from both the Admin browser session and normal customer Web
session. It has its own audience, device identifier, refresh-credential family, last-seen time, expiry,
and revocation boundary. Rotation of a Desktop refresh credential must detect replay and revoke the
affected device credential family.

Both the subject user and an authorized Platform operator can list and revoke connected Desktop
devices. Revocation takes effect at the Control Plane before the next privileged request; the Desktop
then returns to disconnected/local behavior without removing local projects.

Enrollment creation, open/reopen, redemption, expiry, failed redemption, device-session issuance,
rotation, and revocation emit stable Audit actions with actor, subject, Tenant, device, request ID,
reason where applicable, IP/user agent, and before/after status. Audit and notification payloads never
contain the raw handle, refresh credential, device private key, or customer content. Successful
cross-user or managed-device enrollment notifies the subject through the configured security channel.

Creation routes require an authenticated Platform principal, CSRF protection, reason where policy
requires it, and rate limits. Redemption is unauthenticated only in the narrow sense that the one-time
handle is the credential; it is rate-limited, returns non-enumerating failures, and cannot call any
other API before successful atomic exchange.

### UI placement

The Admin action uses the existing flat Settings/detail language, not a new connection wizard shell:

- Tenant or user detail contains a `Desktop access` `SettingsSection`;
- pending Enrollments and connected devices render as `SettingsListRow` entries;
- generation/open/revoke actions use existing Button and Dialog variants;
- expiry, redeemed, revoked, and connected states use text/icon status in addition to color.

Desktop exposes `Connect to Synara SaaS`, connected account/Tenant, `Switch Tenant`, connected devices
where appropriate, and `Disconnect SaaS` through the existing Organization settings taxonomy and
settings primitives. Connection is a state, not a mutually exclusive Local/SaaS toggle.

## UI information architecture and visual consistency

The package boundary does not create a second visual system. Tenant administration and Platform
administration must look and behave like the existing Synara settings and route surfaces. The
separation is an information-architecture and authority boundary, not a new skin.

### Tenant settings navigation

Tenant administration extends the existing Settings taxonomy and sidebar. It must reuse the current
settings search, stable deep links, active-row treatment, section labels, icon slots, keyboard focus,
and `Back to app` behavior.

Enterprise features register ordinary settings navigation items under the existing `Organization`
group. They must not add a nested enterprise sidebar, a second settings switcher, or a tab strip inside
one oversized Organization page. The target taxonomy is:

| Settings item         | Customer scope                                                                               |
| --------------------- | -------------------------------------------------------------------------------------------- |
| Organization overview | Active Tenant and Organization summary, lifecycle state, Plan, and region                    |
| Members & roles       | Invitations, membership, fixed RBAC, offboarding, and Organization assignment                |
| Identity & access     | Domain Verification, SSO enforcement, identity connections, SCIM, and Service Accounts       |
| Credentials           | Provider Credential metadata, bindings, automatic-selection policy, rotation, and revocation |
| Usage & limits        | Usage explanations, quota, entitlement visibility, and reporting-period status               |
| Data & compliance     | Retention, residency, Legal Hold, privacy requests, and export receipts                      |
| Support               | Tenant Support Access policy, active grants, expiry, revocation, and Tenant-visible audit    |

Navigation items are capability-filtered only after the server has returned authenticated context. A
user must not see an empty destination that exists only because a package was compiled into the app.
Conversely, hiding a destination does not replace a server denial.

The global Control Plane context switcher remains the primary Tenant/Organization context control.
Enterprise pages show the active context in their page heading or first summary row and may provide a
context-change affordance by invoking the same host action. They must not create an independent context
store or persist a different active Tenant.

### Tenant settings content

Each Tenant destination uses the existing Settings route shell:

- `RouteInsetSurface` and the standard opaque settings page surface;
- the current content-card seam and collapsed-sidebar behavior;
- the existing settings content width (`max-w-2xl` by default; `max-w-3xl` only for genuinely wider
  lists or evidence views);
- the existing page heading: title, short description, and an optional right-aligned action;
- internal scrolling inside the content surface rather than document scrolling.

Content hierarchy is fixed as:

```text
Settings route
  -> page heading
  -> SettingsSectionShell / SettingsSection
       -> SettingsCard
            -> SettingsRow / SettingsListRow
                 -> optional outlined inset list or DisclosureRegion
```

The existing settings primitives remain the visual source of truth:

- `SettingsSection` groups one business concern and supplies its section label and card.
- `SettingsCard` owns separators between stacked rows. Individual rows do not draw another border.
- `SettingsRow` is for one stable policy or setting; `SettingsListRow` is for dynamic members,
  domains, credentials, grants, holds, requests, Workers, or other collections.
- `SettingsEmptyState` is used for empty, unavailable, or failed sections instead of an ad hoc panel.
- Nested content uses the shared outlined inset surface. It must not add another filled card inside a
  filled card.
- Destructive actions stay inside the section that owns the resource lifecycle. They use the existing
  destructive button/dialog variants and retain reason, version, and dependency confirmation.

This keeps visual separation consistent with the current system: a section label separates business
groups, a single card outline contains related rows, and one parent-owned hairline separates adjacent
rows. Enterprise screens must not wrap every field in an independent card or introduce dashboard tiles
for settings-shaped content.

### Platform Admin navigation

`apps/admin` is a separate authority surface but uses the same Synara shell grammar:

- the existing `SidebarProvider` plus inset content card;
- a 46 px chrome header and the same sidebar row height, radius, icon slot, active state, and focus
  treatment;
- the same theme, typography, density, surface, border, radius, and status tokens;
- list routes at the existing `max-w-3xl` width and settings/detail routes built from the shared
  settings primitives;
- the same dialogs, popovers, toasts, empty states, skeletons, and reduced-motion behavior.

Its initial navigation groups are:

| Admin group | Routes                                                |
| ----------- | ----------------------------------------------------- |
| Platform    | Overview, Tenants, Entitlements                       |
| Operations  | Workers & executions, Queue & Outbox, Releases        |
| Support     | Support Access requests, active support contexts      |
| Governance  | Platform audit, security evidence, operational policy |

The Platform shell must always identify Platform authority. Entering `support_readonly` context adds a
persistent, non-color-only context indicator containing Tenant, reason, expiry, and exit action. It
does not restyle the customer surface as an admin dashboard and never makes a write control available.

Overview pages may summarize operational state, but they must use the existing flat list/status
language. They must not introduce a generic analytics-dashboard aesthetic with shadows, gradients,
large metric tiles, or a separate navigation system.

### Shared visual contract

Enterprise code consumes existing semantic primitives and tokens rather than copying Tailwind class
strings. In particular:

- colors use semantic utilities or the current `--color-*` and `--app-*` variables;
- font sizes use `--app-font-size-*` with fallbacks;
- row spacing uses `--app-density-*`;
- settings cards, controls, and inset surfaces use `settingsPanelStyles.ts`;
- sidebar navigation uses `sidebarRowStyles.ts` and the existing settings-navigation styles;
- header controls use `ChatHeaderButton`, `ChatHeaderIconButton`, or `SurfaceTabChip`;
- pure icon actions use `IconButton` with a required accessible label;
- open/close UI uses `DisclosureRegion`, `CollapsiblePanel`, or
  `disclosureMotion.ts`—never bespoke height/opacity animation;
- hidden inactive or collapsed content uses both `aria-hidden` and `inert`;
- status is communicated by text or icon as well as color.

There is only one content seam. The sidebar, rail, enterprise feature, and Admin app must not add a
second vertical border beside the content card's inset ring. Header hairlines use the existing
`chat-surface-divider` and retreat from the seam by the existing one-pixel rule. Internal settings
row separators remain intentionally lighter and are owned by the containing card.

When two hosts need a visual primitive that currently lives privately in `apps/web`, extract the
smallest stable primitive and its style contract into a shared workspace boundary. Do not copy it into
`apps/admin`, and do not move the whole upstream-facing Web design system preemptively. The extraction
must retain the current public appearance and keep `apps/web` as the reference implementation until
both hosts have focused visual tests.

### Responsive and accessibility behavior

Enterprise pages follow the current desktop-density-first responsive model:

- rows stack their control below the description in narrow containers and return to the standard
  side-by-side layout at the existing breakpoint;
- touch/coarse-pointer hit targets use the shared control behavior without inflating desktop layout;
- long Tenant, domain, Credential, and Worker names truncate or clamp through existing utilities;
- loading regions use `role="status"` and polite announcements, while errors use `role="alert"`;
- keyboard users can reach every navigation item and action with the existing `focus-visible`
  treatment;
- dialog and disclosure motion honors reduced-motion preferences.

Package extraction is not accepted if browser, desktop, and Admin render different spacing, borders,
focus treatment, action order, or destructive confirmation for the same shared enterprise component.

### Current reference implementation

The following current files define the appearance and behavior to preserve during extraction. Their
paths may change as public workspace boundaries are introduced, but their responsibilities must not be
reimplemented independently.

| Concern                                                     | Current source of truth                                                                                    |
| ----------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| Settings taxonomy and stable targets                        | `apps/web/src/settingsNavigation.ts`                                                                       |
| Settings sidebar and search                                 | `apps/web/src/components/SettingsSidebarNav.tsx`                                                           |
| Sidebar rows and settings navigation styling                | `apps/web/src/sidebarRowStyles.ts`, `apps/web/src/settingsSidebarNavStyles.ts`                             |
| Settings page surface and card/row styling                  | `apps/web/src/settingsPanelStyles.ts`                                                                      |
| Settings sections, cards, rows, lists, and empty states     | `apps/web/src/components/settings/SettingsPanelPrimitives.tsx`                                             |
| Shared settings controls                                    | `apps/web/src/components/settings/SettingControls.tsx`                                                     |
| Route inset surface and content card                        | `apps/web/src/components/RouteInsetSurface.tsx`, `apps/web/src/components/chat/composerPickerStyles.ts`    |
| Header dimensions and controls                              | `packages/shared/src/desktopChrome.ts`, `apps/web/src/components/chat/chatHeaderControls.tsx`              |
| Disclosure and collapsible motion                           | `apps/web/src/lib/disclosureMotion.ts`, `apps/web/src/components/ui/DisclosureRegion.tsx`                  |
| Theme, typography, and density projection                   | `apps/web/src/theme/theme.logic.ts`, `apps/web/src/lib/appTypography.ts`, `apps/web/src/lib/appDensity.ts` |
| General buttons, icon actions, dialogs, and status surfaces | `apps/web/src/components/ui/`                                                                              |

If these sources and this ADR disagree during implementation, first determine whether the source was
intentionally evolved upstream. Update the shared design contract and this ADR together; do not preserve
stale class strings locally inside an enterprise component.

## Upstream maintenance policy

The monorepo fork remains the unit of development and release. Enterprise extraction is used to reduce
the conflict surface, not to replace upstream Git integration.

1. Keep the upstream-owned desktop shell, chat transcript, composer, and general settings code as close
   to `upstream/main` as practical.
2. Concentrate Stage 6 code in new feature directories, workspace packages, `apps/admin`, and narrow
   host registration points.
3. Integrate `upstream/main` regularly through an explicit integration branch or the active Stage
   branch. Resolve behavior deliberately and validate both upstream and enterprise paths.
4. Keep enterprise refactors, behavior changes, generated artifacts, and upstream merges in separable
   commits so conflicts can be understood and reverted independently.
5. Do not use a Git submodule, subtree, vendored copy of `apps/desktop`, or a second forked renderer.
6. Do not move the enterprise packages to another repository until their public interfaces, release
   cadence, ownership, and compatibility policy are stable enough to justify independent versioning.

## Security and product invariants

- Desktop and browser builds present the same Tenant policy and management behavior.
- Server-side authentication, fixed RBAC, Tenant operational state, entitlements, optimistic versions,
  and database constraints remain the final authority.
- Support Access is time-bounded, reasoned, audited, Tenant-visible, and read-only at authenticated
  middleware even if a client renders an invalid action.
- No credential plaintext, encrypted payload, Provider payload, prompt, or customer content is exposed
  by enterprise metadata or support views.
- Local mode does not require SaaS authentication and does not silently acquire Platform authority.
- A Platform operator does not receive customer Membership from provisioning or Support Access.
- A Desktop Enrollment never transfers an Admin/Support session or grants Platform authority.
- Enrollment redemption is single-use, short-lived, device-bound, audited, and revalidates current
  Membership and Tenant state before issuing a Desktop-audience session.
- Disconnecting or revoking SaaS access preserves local projects and local runtime state.
- Every destructive Tenant or Platform action preserves its server-required reason, expected version,
  dependency checks, audit action, and request identifier.
- Feature flags and capability checks control availability and presentation; they never weaken a
  server-side denial.

## Migration sequence

The extraction is incremental and must preserve behavior at each step.

### Phase A: establish the feature boundary

1. Introduce `apps/web/src/features/enterprise` and a static enterprise feature registration contract.
2. Move existing Tenant/Organization settings composition behind that boundary without changing API
   behavior or authorization semantics.
3. Register the Tenant destinations under the existing `Organization` settings group and retain the
   existing settings search and stable deep-link behavior.
4. Split the current aggregate settings panel into the documented Tenant pages and cohesive feature
   sections with focused tests.
5. Replace ad hoc enterprise cards, row borders, controls, and disclosures with the existing settings
   primitives and shared motion before package extraction.

### Phase B: extract the client runtime

1. Create `@synara/control-plane-client` and move transport, model, error, and subscription code.
2. Replace Web private-path imports with the package public API.
3. Verify local, unauthenticated, authenticated, reconnect, and partial-failure behavior before removing
   the former Web-local client.

### Phase C: extract the Tenant UI package

1. Create `@synara/enterprise-ui` from the established feature boundary.
2. Inject navigation, client, capability, and design-system dependencies through explicit interfaces.
3. Keep one shared implementation for browser and desktop renderers.
4. Add focused browser tests that compare the shared component behavior in Web and packaged-desktop
   hosts, including density, narrow-width, keyboard, and reduced-motion cases.

Implementation status (2026-07-31): the shared source boundary is complete. `@synara/enterprise-ui`
owns the full Tenant destination renderer, authentication/context presentation, capability derivation,
queries/mutations, and governance components. Web supplies only explicit runtime, design-system, and
Overview extension adapters. Focused Chromium tests cover narrow width, density, keyboard order, and
reduced motion; isolated Web routing exercises all seven destinations. Packaged macOS/Windows/Linux
Desktop acceptance remains a Phase E and release-gate responsibility, not Phase C completion evidence.

### Phase D: introduce Platform Admin

1. Scaffold `apps/admin` with separate authentication and Platform-role gates.
2. Move Platform-only provisioning, triage, Support Access approval, and cross-Tenant operations out of
   the customer settings composition.
3. Build the Admin shell from the existing Synara shell and settings primitives; extract only the
   smallest shared visual contracts needed by both hosts.
4. Add persistent Platform-authority and `support_readonly` context indicators with focused denied-write
   tests.
5. Add `apps/admin` to the deployment and release compatibility matrices without coupling it to desktop
   packaging.

Implementation status (2026-07-31): Phase D's independent authority host is complete. `apps/admin`
owns the mounted Platform Tenant overview, provisioning, versioned entitlement and four-eyes Support
Access workflows; the former Web-private parked component is removed. The shell keeps Platform role
and customer context visible, and does not mount global workflows during `support_readonly`. A fixed
SSO return marker maps only to the configured `SYNARA_PUBLIC_ADMIN_URL`, avoiding an absolute-return
open redirect. Compose and Kustomize deploy the Admin artifact separately, and the release matrix
couples it to the same Web/contracts/client/UI version. Focused unit/Chromium checks and an isolated
Owner/Admin/Security/support_readonly exercise pass. Production-like 25-row operations evidence and
Phase E Desktop Enrollment remain separate release gates.

### Phase E: add one-click Desktop Enrollment

1. Freeze the Enrollment, device identity, Desktop session audience, refresh rotation, and audit
   contracts before exposing a Deep Link.
2. Add authenticated Admin issuance/revocation routes and the atomic, narrowly unauthenticated
   redemption exchange.
3. Add the allowlisted universal/custom-link handler and OS credential-store adapter to the thin
   Desktop shell; keep Enrollment policy and Session issuance in the Control Plane.
4. Add Admin `Desktop access` rows and Desktop SaaS connection state using the existing Settings UI
   primitives.
5. Verify self-enrollment, managed-device enrollment, unverified cross-user denial/IdP handoff,
   replay, expiry, revocation, Membership drift, Tenant suspension, malformed link, unavailable Control
   Plane, and hydration rollback.
6. Add packaged macOS, Windows, and Linux link-open acceptance where each release platform supports the
   selected association mechanism.

Implementation status (2026-07-31): steps 1-5, the repository packaging declaration, a local macOS
arm64 installed-host run, and a local x64/Rosetta installed-host run are complete.
The Control Plane owns the one-time Enrollment lifecycle, Ed25519 device proof, Desktop-audience
session/rotation/replay revocation, disconnect, and audit boundary. Admin exposes self-scoped issuance,
open, Enrollment revoke, and connected-device revoke without persisting the raw handle. Electron main
strictly parses allowlisted links, keeps the credential out of renderer IPC/environment/argv, protects
it with the OS credential adapter, hydrates all SaaS authority before commit, and rolls back on failure
without touching local projects. SQLite tests and a real PostgreSQL two-connection redeem/rotation run
pass. The final local macOS arm64 package exercised LaunchServices link-open, allowlist denial,
Keychain-backed ciphertext, hydration, restart rotation, UI disconnect, trusted local-proxy CORS, and
automatic renderer authority changes; x64 under Rosetta exercised the same lifecycle. Follow-up
packaging fixed disconnected startup so a missing saved connection does not open `safeStorage`, made
native optional-dependency installation target the artifact rather than the build host, normalized the
Mach-O x64 boundary, and installed a native arm64 build without a Keychain or Intel-compatibility
prompt. The immutable reports are
`docs/reports/stage-6-desktop-macos-arm64-local-acceptance-20260731.md`,
`docs/reports/stage-6-desktop-macos-x64-rosetta-local-acceptance-20260731.md`, and
`docs/reports/stage-6-desktop-macos-arm64-local-install-fix-20260731.md`. These apps used an Apple
Development identity, were rejected by Gatekeeper, and had no stapled notarization ticket; Rosetta is
not native Intel-host evidence. Developer ID signed/notarized macOS arm64/x64 plus Windows/Linux
protocol-launch and OS credential-store acceptance remain release gates, so step 6 and the
packaged-host acceptance criteria are not yet complete.

## Acceptance criteria

This decision is implemented only when:

- `apps/desktop` contains no Stage 6 Tenant or Platform business UI;
- browser and packaged desktop exercise the same shared Tenant components;
- `apps/web` integrates enterprise UI through documented public seams rather than private cross-package
  imports;
- Tenant pages appear as ordinary destinations in the existing Settings sidebar and search rather than
  a nested enterprise navigation system;
- Tenant and Admin surfaces reuse the existing shell, tokens, settings cards, parent-owned row
  separators, focus treatment, and disclosure motion;
- the content-card seam remains the only sidebar/content seam and no enterprise route adds a parallel
  vertical border;
- the Control Plane client can be tested without mounting the Web or Admin application;
- Platform workflows are reachable only through `apps/admin` and authenticated Platform routes;
- local mode remains functional with enterprise capabilities absent;
- an authorized Admin can create and open a short-lived Enrollment that launches packaged Desktop and
  completes self/managed-device SaaS connection without another confirmation click;
- the resulting Desktop session belongs to the named subject, has Desktop audience, follows current
  Tenant Membership, and contains no Platform or Support authority;
- one Enrollment cannot produce two sessions, and expiry, revocation, replay, Membership drift, or
  Tenant suspension fails closed without changing local Desktop state;
- Desktop credentials are protected by the OS credential store and all link/secret values are redacted
  from logs, telemetry, errors, and audit payloads;
- SaaS disconnect/device revocation leaves local projects and local runtime usable;
- role-separated browser exercises prove allowed and denied Tenant/Admin paths and correlate successful
  mutations with Audit/request IDs;
- an upstream merge can update the desktop shell and core Web product without modifying enterprise
  implementation files except at intentional host seams.

## Rejected alternatives

- **Put Stage 6 UI in `apps/desktop`:** rejected because Electron is a shell, the browser would need a
  duplicate implementation, and native code would acquire unnecessary business and authorization logic.
- **Fork or copy the renderer for an Enterprise Desktop edition:** rejected because behavior, security
  fixes, tests, and upstream UI changes would diverge.
- **Keep all Platform operations in customer Settings:** rejected because cross-Tenant support and
  operator roles have a different threat model, navigation model, and deployment audience.
- **Create a separate enterprise repository now:** rejected because package contracts and release
  compatibility are still evolving, while the control plane, Web host, Admin app, and desktop artifact
  must be validated together.
- **Use UI visibility as the enterprise or RBAC boundary:** rejected because client state is not an
  authorization authority and can be stale, modified, or bypassed.
- **Copy the Admin browser cookie/session into Desktop:** rejected because it crosses audience, device,
  refresh, revocation, and Platform-authority boundaries.
- **Place an access or refresh token in the Desktop link:** rejected because URLs leak through protocol
  handlers, logs, history, diagnostics, and forwarding. Only a short-lived single-use Enrollment handle
  may cross the link boundary.
- **Let Platform Admin silently sign in as any customer user:** rejected because it destroys actor
  attribution and turns Platform administration into unrestricted impersonation. Cross-user automatic
  enrollment requires a pre-established managed-device/user authority; otherwise the subject completes
  IdP authentication.
- **Treat SaaS connection as a destructive Local/SaaS mode switch:** rejected because local-first data
  and availability must survive account disconnect, revocation, and Control Plane outages.

## Consequences

Stage 6 gains a clear customer-management surface and a separate Platform operations surface without
turning the desktop shell into a product fork. Browser and desktop behavior stay aligned, enterprise
changes are concentrated away from frequently updated upstream files, and the repository keeps one
development and compatibility boundary while the architecture is still evolving.

The migration adds workspace-package APIs and a second Web application that must be versioned and tested
with the Control Plane. Some short-term movement is unavoidable because the existing Web client,
context, route, and Tenant settings composition are large. The phased sequence deliberately establishes
public seams before extracting packages so file movement does not accidentally change authorization,
reconnect, or local-mode behavior.

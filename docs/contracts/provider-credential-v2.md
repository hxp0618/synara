# Provider Credential v2 contract

## Compatibility

This contract extends `provider-credential-v1.md`; it does not reinterpret or move existing ciphertext.
Provider and Git Credentials remain encrypted, Tenant-owned Vault resources. Migration `000033` adds scope
metadata and backfills legacy rows as follows:

| Legacy row                         | v2 scope       | AAD format | Automatic selection      |
| ---------------------------------- | -------------- | ---------- | ------------------------ |
| Provider with `organization_id`    | `organization` | v1         | disabled                 |
| Provider without `organization_id` | `tenant`       | v1         | disabled                 |
| Git with `organization_id`         | `organization` | v2         | disabled and unsupported |
| Git without `organization_id`      | `tenant`       | v2         | disabled and unsupported |

No migration decrypts or re-encrypts a Credential.

## Workspace Credential types and Bindings

Migration `000035` extends the same encrypted Vault resource with purpose-isolated Workspace Credentials:

| Purpose    | Provider/type      | Plaintext allowlist                                                                        | Binding stages                                        |
| ---------- | ------------------ | ------------------------------------------------------------------------------------------ | ----------------------------------------------------- |
| `git`      | `git/https_token`  | `host`, `username`, `token`                                                                | `git_fetch`, `git_push`                               |
| `git`      | `git/ssh_key`      | `host`, `port`, `username`, `privateKey`, optional `privateKeyPassphrase`, fixed `hostKey` | `git_fetch`, `git_push`                               |
| `registry` | `oci/basic`        | `host`, `username`, `password`                                                             | `registry_pull`, `registry_push`, `worker_image_pull` |
| `registry` | `oci/bearer_token` | `host`, `token`                                                                            | `registry_pull`, `registry_push`, `worker_image_pull` |
| `package`  | `npm/npm_token`    | `registryUrl`, `token`, optional normalized `scopes`                                       | `package_read`, `package_publish`                     |
| `package`  | `pypi/pypi_token`  | `indexUrl`, `username`, `token`                                                            | `package_read`, `package_publish`                     |

Workspace Credentials are only Organization- or Tenant-scoped. An immutable `credential_bindings` row owns the
stage and non-secret selector. Project Git selectors are the exact configured Project repository URL; Registry and
Package selectors are derived from the normalized encrypted payload. Active Binding identity is immutable and can
only transition once to disabled. Rotation creates a new Credential version but does not mutate Binding identity.

Migration `000036` retires `projects.git_credential_id` as a writable authority. Compatibility responses may derive
`gitCredentialId` from the one active `git_fetch` Binding, but all creates, updates, Claims and Worker resolution use
Bindings and Generation-fenced Grants. `worker_image_pull` belongs to an Execution Target provisioner and is never
included in an agentd Workload.

## Persisted scope

Provider Credentials use one of these immutable shapes:

| `scope`        | Owner fields      | Optional selectors                           | Eligibility                                        |
| -------------- | ----------------- | -------------------------------------------- | -------------------------------------------------- |
| `user`         | `scope_user_id`   | none                                         | active Tenant member and Session owner             |
| `organization` | `organization_id` | none                                         | exact Session Organization                         |
| `tenant`       | none              | `selector_organization_id`, `selector_model` | exact selector match when present                  |
| `platform`     | none              | none                                         | enterprise entitlement plus explicit Tenant policy |

`provider` remains immutable and is always matched exactly to the Session/Execution Provider. There is no
second Provider selector. User and Platform scope are valid only for `purpose = provider`; Git remains
Organization- or Tenant-scoped.

Every scope is still contained by `tenant_id`. A Platform-scoped row cannot be selected or resolved by
another Tenant, even within the same enterprise installation.

## Resolution

The only valid resolution order is:

```text
explicit Session provider_credential_id
user
organization
tenant
platform
```

Rules:

- An explicitly requested or already persisted Session binding is authoritative and ignores
  `auto_select_enabled`. If it is unavailable, scoped to another owner, or blocked by current policy, the
  request fails; lower scopes are not consulted.
- Automatic candidates require `purpose = provider`, exact `provider`, current availability, and
  `auto_select_enabled = true`.
- The resolver evaluates one scope at a time. Zero candidates continues to the next scope, one selects it,
  and more than one returns `credential_scope_ambiguous` with HTTP status 409.
- User membership is read under `FOR SHARE` on PostgreSQL and must still be `active`. This check occurs during
  Session selection and again in Worker plaintext resolution.
- Tenant selectors use exact Organization UUID and exact normalized Model string matching. A selector does
  not treat a missing Session Model as a wildcard.
- Platform explicit use requires enterprise installation profile, enterprise Tenant plan, and
  `platform_credentials_enabled = true`.
- Platform automatic use additionally requires `platform_credential_auto_select = true` and the individual
  Credential's `auto_select_enabled = true`.

Automatic candidate rows are read under a shared database lock. Session creation and Fork invoke the
resolver inside their idempotent transaction and persist the selected ID as the Session binding. The original
request hash remains based on caller input, so replay returns the same persisted binding even if automatic
policy later changes. Claim revalidates that binding before freezing the Execution Credential snapshot.

## Automatic-selection policy

`provider_credentials.auto_select_enabled` defaults to false for existing and new rows. It is deliberately
not part of ciphertext AAD and does not increment the encrypted payload `version`.

The authorized Control Plane service operation `SetAutoSelect`:

- requires `credentials.manage` in the active Tenant;
- rejects Git Credentials and revoked/expired Credentials;
- requires Platform auto-selection policy before enabling a Platform Credential;
- updates the flag atomically under a row lock;
- writes `credential.auto_select.changed` Audit metadata without plaintext;
- does not change ciphertext, wrapped data key, AAD version, or Credential version.

The management API exposes this operation only through the authorized service boundary:

```text
PUT /v1/tenants/{tenantID}/credentials/{credentialID}/auto-select
```

The response is sanitized Credential metadata. The browser never receives AAD or envelope fields.

## Platform policy

`provider_credential_scope_policies` has one row per Tenant:

```text
tenant_id
platform_credentials_enabled          default false
platform_credential_auto_select       default false
updated_by
created_at / updated_at
```

`platform_credential_auto_select` implies `platform_credentials_enabled`. Enabling either flag requires an
enterprise deployment profile, an enterprise Tenant plan, and an active Tenant member as updater. Deleting
or disabling the policy immediately prevents subsequent Platform selection/resolution; it does not copy or
move ciphertext.

The policy is managed through:

```text
GET /v1/tenants/{tenantID}/provider-credential-scope-policy
PUT /v1/tenants/{tenantID}/provider-credential-scope-policy
```

The Settings UI uses these APIs for scope creation, selector metadata, per-Credential automatic-selection
opt-in, and the two independent Platform policy gates.

## Scope-aware envelope

New Credentials and rotations use NUL-delimited AAD v3:

```text
synara-credential-v3
tenant_id
credential_id
purpose
provider
credential_type
scope
scope_user_id-or-empty
organization_id-or-empty
selector_organization_id-or-empty
selector_model-or-empty
version
```

`auto_select_enabled` is intentionally absent because emergency policy disablement must not require key
access or payload rotation. AAD v1 is accepted only for legacy Provider rows, and v2 only for legacy Git
rows. Unknown AAD versions fail closed and are never guessed.

A rotation:

- advances exactly one Credential version;
- writes a new payload ciphertext and independently wrapped data key;
- upgrades legacy AAD v1/v2 to v3 atomically;
- leaves Tenant, purpose, Provider, type, scope owner, and selectors unchanged.

Database triggers reject envelope mutation without a versioned rotation and reject an AAD-only upgrade.
Responses expose scope metadata and `autoSelectEnabled`, but not `aad_version`, encrypted payload, wrapped
data key, or plaintext.

## Execution and Worker fencing

The Execution generation freezes `provider_credential_id_snapshot` and
`provider_credential_version_snapshot`. Plaintext resolution additionally requires:

- the current Worker identity;
- the current Lease Token, unexpired Lease, and exact Generation;
- the frozen Credential ID and version;
- current purpose/Provider/scope eligibility;
- active User membership for User scope;
- current Platform entitlement/policy for Platform scope;
- current expiry and revocation state.

Session rebinding cannot change an in-flight generation. Rotation fences the old version. Revocation,
expiry, User suspension, or Platform policy disablement prevents new plaintext resolution immediately.

## Stable errors

| Code                                        | Meaning                                                                      |
| ------------------------------------------- | ---------------------------------------------------------------------------- |
| `credential_scope_ambiguous`                | multiple candidates match the first automatic scope                          |
| `credential_not_found`                      | explicit ID is absent or not eligible for the Session owner/scope            |
| `credential_purpose_mismatch`               | explicit binding is not a Provider Credential                                |
| `credential_provider_mismatch`              | explicit binding targets another Provider                                    |
| `credential_unavailable`                    | Credential is revoked or expired                                             |
| `platform_credential_not_entitled`          | enterprise entitlement or explicit Platform policy is absent                 |
| `platform_credential_auto_select_forbidden` | per-Credential Platform auto-select was requested without Tenant auto policy |
| `credential_aad_version_unsupported`        | stored envelope declares an unknown AAD format                               |

These errors never include candidate names, payload fields, ciphertext, KMS context, or secret-derived data.

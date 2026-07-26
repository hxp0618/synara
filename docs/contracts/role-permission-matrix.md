# Fixed role to permission matrix v1

Business services check permissions through `internal/authorization`; role strings are never used
as authorization decisions outside that mapping. The authoritative permission-to-role mapping is
`services/control-plane/internal/authorization/permissions.go`; this matrix is its documentation
projection and must be updated in the same change that edits that file.

## Tenant roles

| Capability group                      | owner  | admin  | security_admin | billing_admin | auditor   | member                     |
| ------------------------------------- | ------ | ------ | -------------- | ------------- | --------- | -------------------------- |
| Tenant read                           | yes    | yes    | yes            | yes           | yes       | yes                        |
| Tenant update                         | yes    | yes    | no             | no            | no        | no                         |
| Tenant delete                         | yes    | no     | no             | no            | no        | no                         |
| Member read                           | yes    | yes    | yes            | yes           | yes       | no                         |
| Member invite/update/remove           | yes    | yes    | no             | no            | no        | no                         |
| Organization management               | yes    | yes    | no             | no            | no        | no                         |
| Project/session/execution operations  | yes    | yes    | read-only      | no            | read-only | organization role required |
| Artifacts (`artifact.*`)              | manage | manage | read           | no            | read      | no                         |
| Credentials (`credentials.*`)         | manage | use    | manage         | no            | no        | no                         |
| Workers (`worker.*`)                  | manage | manage | read           | no            | no        | no                         |
| Audit (`audit.read`)                  | read   | read   | read           | no            | read      | no                         |
| Outbox operations (`outbox.*`)        | manage | manage | read           | no            | read      | no                         |
| Enterprise identity (`identity.*`)    | manage | read   | manage         | no            | no        | no                         |
| Login Session revocation              | manage | manage | manage         | no            | no        | no                         |
| Service accounts (`service_accounts.*`) | manage | manage | manage       | no            | no        | no                         |
| Quota (`quota.*`)                     | manage | manage | no             | manage        | read      | no                         |
| Retention (`retention.*`)             | manage | manage | manage         | no            | read      | no                         |
| Resource lifecycle (`lifecycle.*`)    | manage | manage | read           | no            | read      | no                         |
| Scheduling policy (`scheduling_policy.*`) | manage | manage | manage     | no            | read      | no                         |
| Billing (`billing.manage`)            | manage | no     | no             | manage        | no        | no                         |

In this table `manage` includes the group's read permissions; `use` means `credentials.use` only
(Session-scoped Credential resolution without read or management access).

## Organization roles

| Capability group                  | owner  | admin  | agent_operator | member     | viewer    |
| --------------------------------- | ------ | ------ | -------------- | ---------- | --------- |
| Organization read                 | yes    | yes    | yes            | yes        | yes       |
| Organization/member management    | yes    | yes    | no             | no         | no        |
| Project management                | yes    | yes    | read-only      | read-only  | read-only |
| Session create/use                | yes    | yes    | yes            | yes        | read-only |
| Execution create/cancel/interrupt | yes    | yes    | yes            | yes        | no        |
| Execution approval                | yes    | yes    | yes            | no         | no        |
| Artifacts (`artifact.*`)          | manage | manage | manage         | read/write | read      |
| Credentials (`credentials.use`)   | yes    | yes    | yes            | yes        | no        |
| Scheduling policy (`scheduling_policy.*`) | manage | manage | read   | read       | read      |

Tenant permissions may grant access across all organizations (for example Tenant Owner/Admin).
Otherwise an active Organization Membership is required and its role supplies the permission.

`billing.manage` covers listing the shared provider tariff catalog plus tenant-owned invoice import and reconciliation
operations. Appending to the shared catalog, sealing/sweeping shared-Target estimate authority, and allocating an
operator-owned account invoice to a shared Target also require the active/path Tenant to match the server-configured
platform tariff-operator Tenant; tenant permission alone never grants a global rate or shared actual-cost mutation.

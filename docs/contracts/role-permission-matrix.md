# Fixed role to permission matrix v1

Business services check permissions through `internal/authorization`; role strings are never used
as authorization decisions outside that mapping.

## Tenant roles

| Capability group | owner | admin | security_admin | billing_admin | auditor | member |
| --- | --- | --- | --- | --- | --- | --- |
| Tenant read | yes | yes | yes | yes | yes | yes |
| Tenant update | yes | yes | no | no | no | no |
| Tenant delete | yes | no | no | no | no | no |
| Member read | yes | yes | yes | yes | yes | no |
| Member invite/update/remove | yes | yes | no | no | no | no |
| Organization management | yes | yes | no | no | no | no |
| Project/session/execution operations | yes | yes | read-only | no | read-only | organization role required |
| Credentials | manage | no | manage | no | no | no |
| Workers | manage | manage | read | no | no | no |
| Audit | read | read | read | no | read | no |
| Billing | manage | no | no | manage | no | no |

## Organization roles

| Capability group | owner | admin | agent_operator | member | viewer |
| --- | --- | --- | --- | --- | --- |
| Organization read | yes | yes | yes | yes | yes |
| Organization/member management | yes | yes | no | no | no |
| Project management | yes | yes | read-only | read-only | read-only |
| Session create/use | yes | yes | yes | yes | read-only |
| Execution create/cancel | yes | yes | yes | yes | no |
| Execution approval | yes | yes | yes | no | no |

Tenant permissions may grant access across all organizations (for example Tenant Owner/Admin).
Otherwise an active Organization Membership is required and its role supplies the permission.

# `@synara/enterprise-ui`

Shared customer-Tenant UI implementation for Synara Web and the packaged Desktop Web renderer.

The public surface owns all seven Organization destinations, navigation/search metadata,
capability-aware registration, the complete Tenant destination renderer, authentication/context
presentation, lifecycle and governance components, and their focused tests.

`apps/web` injects the authenticated Control Plane runtime, shared visual primitives, navigation, and
the existing Execution Targets / Project Sessions Overview extensions through public providers. This
package does not import Web route internals, Electron, or application singletons. Desktop loads the same
Web renderer; packaged-platform acceptance and Desktop Enrollment remain separate release work.

Focused verification:

```bash
bun run --cwd packages/enterprise-ui test
bun run --cwd packages/enterprise-ui build
bun run --cwd apps/web test:browser -- src/components/settings/EnterpriseSettingsRenderer.browser.tsx
```

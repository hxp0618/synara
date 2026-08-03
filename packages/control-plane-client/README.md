# `@synara/control-plane-client`

Browser-capable typed client for the Synara Control Plane HTTP and event surfaces.

The package owns request and response models, same-origin cookie transport, stable `ControlPlaneError`
decoding with request identifiers, API methods, artifact transfer helpers, and resumable Session Event
streams. It intentionally has no React, Electron, route, or UI dependency.

Normal HTTP(S) browser hosts require no setup and always use the page origin. A custom-protocol host may
install a fallback resolver before rendering:

```ts
import { configureControlPlaneClientTransport } from "@synara/control-plane-client";

configureControlPlaneClientTransport({
  resolveHttpUrl: (path) => resolveDesktopHttpBridge(path),
});
```

The fallback never overrides HTTP(S) same-origin routing. `apps/web` owns the current Desktop bridge in
`src/controlPlaneClientTransport.ts`; the shared package does not inspect Electron globals.

Focused verification:

```bash
bun run --cwd packages/control-plane-client test
bun run --cwd packages/control-plane-client build
```

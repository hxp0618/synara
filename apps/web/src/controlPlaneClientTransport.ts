// FILE: controlPlaneClientTransport.ts
// Purpose: Inject the Web/Desktop URL bridge into the shared browser Control Plane client.
// Layer: Web host integration
// Exports: configureWebControlPlaneClientTransport

import { configureControlPlaneClientTransport } from "@synara/control-plane-client";

import { resolveWsHttpUrl } from "./lib/wsHttpUrl";

export function configureWebControlPlaneClientTransport(): void {
  configureControlPlaneClientTransport({ resolveHttpUrl: resolveWsHttpUrl });
}

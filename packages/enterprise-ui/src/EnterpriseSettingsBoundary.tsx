// FILE: EnterpriseSettingsBoundary.tsx
// Purpose: Own the host-neutral suspense seam for a registered Tenant settings destination.
// Layer: Public enterprise UI boundary
// Exports: EnterpriseSettingsBoundary and its renderer contract

import { Suspense, type ComponentType, type ReactNode } from "react";

import type { EnterpriseSettingsSectionId } from "./settingsRegistration";

export type EnterpriseSettingsDestinationRenderer = ComponentType<{
  destination: EnterpriseSettingsSectionId;
}>;

export function EnterpriseSettingsBoundary(props: {
  destination: EnterpriseSettingsSectionId;
  renderer: EnterpriseSettingsDestinationRenderer;
  loadingFallback: ReactNode;
}) {
  const Destination = props.renderer;
  return (
    <Suspense
      fallback={
        <div role="status" aria-live="polite">
          {props.loadingFallback}
        </div>
      }
    >
      <Destination destination={props.destination} />
    </Suspense>
  );
}

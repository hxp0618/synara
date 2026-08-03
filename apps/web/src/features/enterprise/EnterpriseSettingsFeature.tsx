// FILE: EnterpriseSettingsFeature.tsx
// Purpose: Provide the narrow Web-host seam for Stage 6 Tenant settings composition.
// Layer: Enterprise feature boundary
// Exports: EnterpriseSettingsFeature

import {
  EnterpriseSettingsBoundary,
  type EnterpriseSettingsSectionId,
} from "@synara/enterprise-ui";
import { lazy } from "react";

import { SettingsListRow, SettingsSection } from "~/components/settings/SettingsPanelPrimitives";
import { WebEnterpriseUiHost } from "./WebEnterpriseUiHost";

const WebEnterpriseSettingsRenderer = lazy(() =>
  import("./WebEnterpriseSettingsRenderer").then((module) => ({
    default: module.WebEnterpriseSettingsRenderer,
  })),
);

export function EnterpriseSettingsFeature(props: { destination: EnterpriseSettingsSectionId }) {
  return (
    <WebEnterpriseUiHost>
      <EnterpriseSettingsBoundary
        destination={props.destination}
        renderer={WebEnterpriseSettingsRenderer}
        loadingFallback={
          <SettingsSection title="Tenant settings">
            <SettingsListRow
              title="Loading…"
              description="Loading the authenticated Tenant administration surface."
            />
          </SettingsSection>
        }
      />
    </WebEnterpriseUiHost>
  );
}

// FILE: WebEnterpriseSettingsRenderer.tsx
// Purpose: Bind Web Control Plane runtime and overview extensions to the shared Tenant renderer.
// Layer: Enterprise host integration

import {
  EnterpriseControlPlaneRuntimeProvider,
  EnterpriseSettingsHostProvider,
  TenantOrganizationSettingsPanel,
  type EnterpriseSettingsHostComponents,
  type EnterpriseSettingsSectionId,
} from "@synara/enterprise-ui";

import { ExecutionTargetSettingsSection } from "~/components/settings/ExecutionTargetSettingsSection";
import { ProjectSessionSettingsSection } from "~/components/settings/ProjectSessionSettingsSection";
import { useControlPlane } from "~/controlPlaneContext";

const WEB_ENTERPRISE_SETTINGS_COMPONENTS = {
  ExecutionTargetsSection: ExecutionTargetSettingsSection,
  ProjectSessionsSection: ProjectSessionSettingsSection,
} satisfies EnterpriseSettingsHostComponents;

export function WebEnterpriseSettingsRenderer(props: { destination: EnterpriseSettingsSectionId }) {
  const controlPlane = useControlPlane();

  return (
    <EnterpriseControlPlaneRuntimeProvider runtime={controlPlane}>
      <EnterpriseSettingsHostProvider components={WEB_ENTERPRISE_SETTINGS_COMPONENTS}>
        <TenantOrganizationSettingsPanel destination={props.destination} />
      </EnterpriseSettingsHostProvider>
    </EnterpriseControlPlaneRuntimeProvider>
  );
}

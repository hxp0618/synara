// FILE: WebEnterpriseUiHost.tsx
// Purpose: Bind the shared enterprise package to the Web reference design-system primitives.
// Layer: Enterprise host integration
// Exports: WebEnterpriseUiHost

import {
  EnterpriseUiHostProvider,
  type EnterpriseLinkButtonProps,
  type EnterpriseUiHostComponents,
} from "@synara/enterprise-ui";
import type { ReactNode } from "react";

import { Badge } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import { Switch } from "~/components/ui/switch";
import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneInlineError,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsCard,
  SettingsListRow,
  SettingsRow,
  SettingsSection,
  SettingsSectionShell,
} from "~/components/settings/SettingsPanelPrimitives";
import { RefreshCwIcon } from "~/lib/icons";
import { openExternalLink } from "~/lib/linkChips";

function WebEnterpriseLinkButton(props: EnterpriseLinkButtonProps) {
  const { href, ...buttonProps } = props;
  return <Button {...buttonProps} render={<a href={href} />} />;
}

function downloadJsonFile(fileName: string, value: unknown) {
  const serialized = typeof value === "string" ? value : JSON.stringify(value, null, 2);
  const blob = new Blob([serialized], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = fileName;
  link.click();
  URL.revokeObjectURL(url);
}

const WEB_ENTERPRISE_UI_COMPONENTS = {
  Badge,
  Button,
  Input,
  Textarea,
  LinkButton: WebEnterpriseLinkButton,
  Switch,
  FormField: ControlPlaneFormField,
  InlineError: ControlPlaneInlineError,
  StatusPill: ControlPlaneStatusPill,
  SettingsCard,
  SettingsListRow,
  SettingsRow,
  SettingsSection,
  SettingsSectionShell,
  RefreshIcon: RefreshCwIcon,
  formGridClassName: CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  nativeSelectClassName: CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  confirmAction: (message) => window.confirm(message),
  downloadJsonFile,
  navigateToUrl: (url) => window.location.assign(url),
  openExternalUrl: openExternalLink,
} satisfies EnterpriseUiHostComponents;

export function WebEnterpriseUiHost(props: { children: ReactNode }) {
  return (
    <EnterpriseUiHostProvider components={WEB_ENTERPRISE_UI_COMPONENTS}>
      {props.children}
    </EnterpriseUiHostProvider>
  );
}

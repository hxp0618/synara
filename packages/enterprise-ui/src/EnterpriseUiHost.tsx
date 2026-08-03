// FILE: EnterpriseUiHost.tsx
// Purpose: Inject stable host design-system primitives into shared Tenant UI components.
// Layer: Public enterprise UI boundary
// Exports: EnterpriseUiHostProvider, useEnterpriseUiHost, and component contracts

import {
  createContext,
  useContext,
  type ButtonHTMLAttributes,
  type ComponentType,
  type HTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SVGProps,
  type TextareaHTMLAttributes,
} from "react";

export type EnterpriseButtonProps = Omit<ButtonHTMLAttributes<HTMLButtonElement>, "size"> & {
  size?: "xs" | "sm" | "default";
  variant?: "default" | "outline" | "ghost" | "destructive" | "destructive-outline";
};

export type EnterpriseInputProps = Omit<InputHTMLAttributes<HTMLInputElement>, "size"> & {
  size?: "sm" | "default" | "lg" | number;
  nativeInput?: boolean;
};

export type EnterpriseTextareaProps = TextareaHTMLAttributes<HTMLTextAreaElement>;

export type EnterpriseBadgeProps = HTMLAttributes<HTMLSpanElement> & {
  variant?: "default" | "warning" | "success";
};

export type EnterpriseLinkButtonProps = {
  href: string;
  children: ReactNode;
  size?: "xs" | "sm" | "default";
  variant?: "default" | "outline";
};

export type EnterpriseSwitchProps = {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  "aria-label": string;
};

export type EnterpriseSettingsListRowProps = {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  align?: "center" | "start";
};

export type EnterpriseSettingsRowProps = {
  title: ReactNode;
  description: string;
  status?: ReactNode;
  control?: ReactNode;
  children?: ReactNode;
};

export type EnterpriseUiHostComponents = {
  Badge: ComponentType<EnterpriseBadgeProps>;
  Button: ComponentType<EnterpriseButtonProps>;
  Input: ComponentType<EnterpriseInputProps>;
  Textarea: ComponentType<EnterpriseTextareaProps>;
  LinkButton: ComponentType<EnterpriseLinkButtonProps>;
  Switch: ComponentType<EnterpriseSwitchProps>;
  FormField: ComponentType<{ label: string; children: ReactNode }>;
  InlineError: ComponentType<{ error: unknown }>;
  StatusPill: ComponentType<{ value: string; active?: boolean }>;
  SettingsCard: ComponentType<{ children: ReactNode }>;
  SettingsListRow: ComponentType<EnterpriseSettingsListRowProps>;
  SettingsRow: ComponentType<EnterpriseSettingsRowProps>;
  SettingsSection: ComponentType<{ title: string; children: ReactNode }>;
  SettingsSectionShell: ComponentType<{
    title: string;
    action?: ReactNode;
    children: ReactNode;
  }>;
  RefreshIcon: ComponentType<SVGProps<SVGSVGElement>>;
  formGridClassName: string;
  nativeSelectClassName: string;
  confirmAction: (message: string) => boolean;
  downloadJsonFile: (fileName: string, value: unknown) => void;
  navigateToUrl: (url: string) => void;
  openExternalUrl: (url: string) => void;
};

const EnterpriseUiHostContext = createContext<EnterpriseUiHostComponents | null>(null);

export function EnterpriseUiHostProvider(props: {
  components: EnterpriseUiHostComponents;
  children: ReactNode;
}) {
  return (
    <EnterpriseUiHostContext.Provider value={props.components}>
      {props.children}
    </EnterpriseUiHostContext.Provider>
  );
}

export function useEnterpriseUiHost(): EnterpriseUiHostComponents {
  const host = useContext(EnterpriseUiHostContext);
  if (!host) {
    throw new Error("Enterprise UI must be rendered inside EnterpriseUiHostProvider.");
  }
  return host;
}

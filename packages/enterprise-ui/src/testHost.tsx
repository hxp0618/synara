import type { EnterpriseUiHostComponents } from "./EnterpriseUiHost";

export const TEST_ENTERPRISE_UI_HOST = {
  Badge: ({ children, variant: _variant, ...props }) => <span {...props}>{children}</span>,
  Button: ({ children, size: _size, variant: _variant, ...props }) => (
    <button {...props}>{children}</button>
  ),
  Input: ({ size: _size, nativeInput: _nativeInput, ...props }) => <input {...props} />,
  Textarea: (props) => <textarea {...props} />,
  LinkButton: ({ href, children }) => <a href={href}>{children}</a>,
  Switch: ({ checked, "aria-label": ariaLabel }) => (
    <input aria-label={ariaLabel} checked={checked} readOnly type="checkbox" />
  ),
  FormField: ({ label, children }) => (
    <label>
      {label}
      {children}
    </label>
  ),
  InlineError: ({ error }) => (error ? <p role="alert">request failed</p> : null),
  StatusPill: ({ value }) => <span>{value}</span>,
  SettingsCard: ({ children }) => <div data-test="settings-card">{children}</div>,
  SettingsListRow: ({ title, description, actions }) => (
    <div>
      <strong>{title}</strong>
      <span>{description}</span>
      {actions}
    </div>
  ),
  SettingsRow: ({ title, description, children }) => (
    <div>
      <strong>{title}</strong>
      <span>{description}</span>
      {children}
    </div>
  ),
  SettingsSection: ({ title, children }) => (
    <section>
      <h2>{title}</h2>
      {children}
    </section>
  ),
  SettingsSectionShell: ({ title, action, children }) => (
    <section>
      <h2>{title}</h2>
      {action}
      {children}
    </section>
  ),
  RefreshIcon: (props) => <svg {...props} />,
  formGridClassName: "test-form-grid",
  nativeSelectClassName: "test-native-select",
  confirmAction: () => true,
  downloadJsonFile: () => undefined,
  navigateToUrl: () => undefined,
  openExternalUrl: () => undefined,
} satisfies EnterpriseUiHostComponents;

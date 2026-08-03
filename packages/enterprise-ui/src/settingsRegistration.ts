// FILE: settingsRegistration.ts
// Purpose: Declare the Stage 6 Tenant settings destinations and resolve their authenticated visibility.
// Layer: Enterprise feature registration boundary
// Exports: static destination metadata, runtime resolver, and destination guards

export const ENTERPRISE_SETTINGS_SECTION_IDS = [
  "organization-overview",
  "organization-members",
  "organization-identity",
  "organization-credentials",
  "organization-usage",
  "organization-data",
  "organization-support",
] as const;

export type EnterpriseSettingsSectionId = (typeof ENTERPRISE_SETTINGS_SECTION_IDS)[number];

export const ENTERPRISE_SETTINGS_NAV_ITEMS = [
  {
    id: "organization-overview",
    group: "organization",
    label: "Organization overview",
    description:
      "Active Tenant and Organization context, lifecycle, Plan, region, and runtime scope.",
    icon: "buildings",
    eyebrow: "Tenant summary",
  },
  {
    id: "organization-members",
    group: "organization",
    label: "Members & roles",
    description: "Invitations, fixed RBAC, offboarding, and Organization assignments.",
    icon: "user-group",
    eyebrow: "Access lifecycle",
  },
  {
    id: "organization-identity",
    group: "organization",
    label: "Identity & access",
    description: "Domains, SSO enforcement, identity connections, SCIM, and Service Accounts.",
    icon: "shield-access",
    eyebrow: "Enterprise identity",
  },
  {
    id: "organization-credentials",
    group: "organization",
    label: "Credentials",
    description:
      "Provider Credential metadata, bindings, selection policy, rotation, and revocation.",
    icon: "key-1",
    eyebrow: "Credential governance",
  },
  {
    id: "organization-usage",
    group: "organization",
    label: "Usage & limits",
    description: "Tenant usage, Token and cost explanations, quota, and internal entitlements.",
    icon: "gauge",
    eyebrow: "Consumption controls",
  },
  {
    id: "organization-data",
    group: "organization",
    label: "Data & compliance",
    description: "Retention, residency, Legal Hold, privacy requests, and export receipts.",
    icon: "file-lock",
    eyebrow: "Data governance",
  },
  {
    id: "organization-support",
    group: "organization",
    label: "Support",
    description:
      "Tenant Support Access policy, active grants, expiry, revocation, and visible audit.",
    icon: "support",
    eyebrow: "Controlled support",
  },
] as const;

export const ENTERPRISE_SETTINGS_SEARCH_ENTRIES = [
  {
    id: "organization-overview:tenant-context",
    section: "organization-overview",
    title: "Tenant context",
    keywords:
      "Organization overview lifecycle status entitlement profile region execution targets projects",
    target: null,
  },
  {
    id: "organization-members:member-lifecycle",
    section: "organization-members",
    title: "Members & roles",
    keywords: "Invitation fixed RBAC joiner mover leaver suspend reactivate offboarding",
    target: null,
  },
  {
    id: "organization-identity:governance",
    section: "organization-identity",
    title: "Identity & access",
    keywords: "Domain verification SSO OIDC SAML SCIM Service Account group mapping",
    target: null,
  },
  {
    id: "organization-credentials:governance",
    section: "organization-credentials",
    title: "Credentials",
    keywords: "Provider BYOK binding selector rotation revocation secret metadata",
    target: null,
  },
  {
    id: "organization-usage:limits",
    section: "organization-usage",
    title: "Usage & limits",
    keywords: "Tenant usage token cost quota entitlement reporting period limits",
    target: null,
  },
  {
    id: "organization-data:compliance",
    section: "organization-data",
    title: "Data & compliance",
    keywords: "Retention residency Legal Hold privacy DSAR export deletion lifecycle",
    target: null,
  },
  {
    id: "organization-support:controlled-access",
    section: "organization-support",
    title: "Support",
    keywords: "Support Access grant expiry revoke read only Tenant audit outbox delivery",
    target: null,
  },
] as const;

export type EnterpriseSettingsCapabilities = {
  canReadMembers: boolean;
  canReadIdentity: boolean;
  canReadServiceAccounts: boolean;
  canReadCredentials: boolean;
  canReadQuota: boolean;
  canReadRetention: boolean;
  canReadSchedulingPolicy: boolean;
  canReadAudit: boolean;
  canReadOutbox: boolean;
};

export type EnterpriseSettingsRuntime = {
  availability: "detecting" | "local" | "available" | "unavailable";
  authentication: "unknown" | "unauthenticated" | "authenticated" | "error";
  hasSession: boolean;
  hasActiveTenant: boolean;
  capabilities: EnterpriseSettingsCapabilities;
};

export function isEnterpriseSettingsSection(value: string): value is EnterpriseSettingsSectionId {
  return ENTERPRISE_SETTINGS_SECTION_IDS.some((candidate) => candidate === value);
}

export function resolveEnterpriseSettingsNavItems(runtime: EnterpriseSettingsRuntime) {
  if (runtime.availability !== "available") {
    return [];
  }
  if (runtime.authentication === "unauthenticated") {
    return ENTERPRISE_SETTINGS_NAV_ITEMS.slice(0, 1);
  }
  if (runtime.authentication !== "authenticated" || !runtime.hasSession) {
    return [];
  }
  if (!runtime.hasActiveTenant) {
    return ENTERPRISE_SETTINGS_NAV_ITEMS.slice(0, 1);
  }

  const visible = new Set<EnterpriseSettingsSectionId>(["organization-overview"]);
  const capabilities = runtime.capabilities;
  if (capabilities.canReadMembers) visible.add("organization-members");
  if (capabilities.canReadIdentity || capabilities.canReadServiceAccounts) {
    visible.add("organization-identity");
  }
  if (capabilities.canReadCredentials) visible.add("organization-credentials");
  if (capabilities.canReadQuota) visible.add("organization-usage");
  if (
    capabilities.canReadRetention ||
    capabilities.canReadSchedulingPolicy ||
    capabilities.canReadAudit
  ) {
    visible.add("organization-data");
  }
  if (capabilities.canReadAudit || capabilities.canReadOutbox) {
    visible.add("organization-support");
  }
  return ENTERPRISE_SETTINGS_NAV_ITEMS.filter((item) => visible.has(item.id));
}

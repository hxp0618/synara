import type { ControlPlaneCredential, ControlPlaneCredentialPurpose } from "./controlPlaneClient";
import { normalizeControlPlaneProviderCode } from "./controlPlaneProviderCode";

export type CredentialFormKind =
  | "provider_codex"
  | "provider_claude"
  | "provider_advanced"
  | "git_https"
  | "git_ssh"
  | "registry_basic"
  | "registry_bearer"
  | "package_npm"
  | "package_pypi";

export type CredentialPayloadDraft = {
  provider: string;
  credentialType: string;
  jsonPayload: string;
  host: string;
  port: string;
  username: string;
  secret: string;
  privateKey: string;
  privateKeyPassphrase: string;
  hostKey: string;
  endpointUrl: string;
  scopes: string;
};

export type CredentialFormDescriptor = {
  purpose: ControlPlaneCredentialPurpose;
  provider: string;
  credentialType: string;
};

export const DEFAULT_CREDENTIAL_FORM_KIND: CredentialFormKind = "provider_codex";

export const CREDENTIAL_FORM_KIND_OPTIONS: ReadonlyArray<{
  kind: CredentialFormKind;
  label: string;
}> = [
  { kind: "provider_codex", label: "Provider runtime (Codex)" },
  { kind: "provider_claude", label: "Provider runtime (Claude)" },
  { kind: "provider_advanced", label: "Provider runtime (advanced JSON)" },
  { kind: "git_https", label: "Git HTTPS token" },
  { kind: "git_ssh", label: "Git SSH key" },
  { kind: "registry_basic", label: "OCI Registry username/password" },
  { kind: "registry_bearer", label: "OCI Registry bearer token" },
  { kind: "package_npm", label: "npm registry token" },
  { kind: "package_pypi", label: "PyPI index token" },
];

export function createCredentialPayloadDraft(kind: CredentialFormKind): CredentialPayloadDraft {
  return {
    provider: kind === "provider_codex" ? "codex" : kind === "provider_claude" ? "claudeagent" : "",
    credentialType: "api_key",
    jsonPayload: "",
    host: "",
    port: kind === "git_ssh" ? "22" : "",
    username:
      kind === "git_https"
        ? "x-access-token"
        : kind === "git_ssh"
          ? "git"
          : kind === "package_pypi"
            ? "__token__"
            : "",
    secret: "",
    privateKey: "",
    privateKeyPassphrase: "",
    hostKey: "",
    endpointUrl:
      kind === "package_npm"
        ? "https://registry.npmjs.org/"
        : kind === "package_pypi"
          ? "https://pypi.org/simple/"
          : "",
    scopes: "",
  };
}

export function credentialFormKindForCredential(
  credential: Pick<ControlPlaneCredential, "purpose" | "provider" | "credentialType">,
): CredentialFormKind | null {
  if (credential.purpose === "provider") {
    const provider = normalizeControlPlaneProviderCode(credential.provider);
    if (credential.credentialType === "api_key" && provider === "codex") {
      return "provider_codex";
    }
    if (credential.credentialType === "api_key" && provider === "claudeagent") {
      return "provider_claude";
    }
    return "provider_advanced";
  }
  if (credential.purpose === "git" && credential.credentialType === "https_token") {
    return "git_https";
  }
  if (credential.purpose === "git" && credential.credentialType === "ssh_key") {
    return "git_ssh";
  }
  if (
    credential.purpose === "registry" &&
    credential.provider === "oci" &&
    credential.credentialType === "basic"
  ) {
    return "registry_basic";
  }
  if (
    credential.purpose === "registry" &&
    credential.provider === "oci" &&
    credential.credentialType === "bearer_token"
  ) {
    return "registry_bearer";
  }
  if (
    credential.purpose === "package" &&
    credential.provider === "npm" &&
    credential.credentialType === "npm_token"
  ) {
    return "package_npm";
  }
  if (
    credential.purpose === "package" &&
    credential.provider === "pypi" &&
    credential.credentialType === "pypi_token"
  ) {
    return "package_pypi";
  }
  return null;
}

export function describeCredentialForm(
  kind: CredentialFormKind,
  draft: CredentialPayloadDraft,
): CredentialFormDescriptor {
  switch (kind) {
    case "provider_codex":
      return { purpose: "provider", provider: "codex", credentialType: "api_key" };
    case "provider_claude":
      return { purpose: "provider", provider: "claudeagent", credentialType: "api_key" };
    case "provider_advanced":
      return {
        purpose: "provider",
        provider: draft.provider.trim().toLowerCase(),
        credentialType: draft.credentialType.trim().toLowerCase(),
      };
    case "git_https":
      return { purpose: "git", provider: "git", credentialType: "https_token" };
    case "git_ssh":
      return { purpose: "git", provider: "git", credentialType: "ssh_key" };
    case "registry_basic":
      return { purpose: "registry", provider: "oci", credentialType: "basic" };
    case "registry_bearer":
      return { purpose: "registry", provider: "oci", credentialType: "bearer_token" };
    case "package_npm":
      return { purpose: "package", provider: "npm", credentialType: "npm_token" };
    case "package_pypi":
      return { purpose: "package", provider: "pypi", credentialType: "pypi_token" };
  }
}

export function buildCredentialPayload(
  kind: CredentialFormKind,
  draft: CredentialPayloadDraft,
): Record<string, unknown> {
  switch (kind) {
    case "provider_codex":
    case "provider_claude":
      return buildManagedProviderPayload(draft);
    case "provider_advanced":
      return parseCredentialJSONPayload(draft.jsonPayload);
    case "git_https":
      return { host: draft.host, username: draft.username, token: draft.secret };
    case "git_ssh": {
      const port = Number(draft.port);
      if (!Number.isInteger(port) || port < 1 || port > 65_535) {
        throw new Error("Git SSH port must be an integer between 1 and 65535.");
      }
      return {
        host: draft.host,
        port,
        username: draft.username,
        privateKey: draft.privateKey,
        ...(draft.privateKeyPassphrase === ""
          ? {}
          : { privateKeyPassphrase: draft.privateKeyPassphrase }),
        hostKey: draft.hostKey,
      };
    }
    case "registry_basic":
      return { host: draft.host, username: draft.username, password: draft.secret };
    case "registry_bearer":
      return { host: draft.host, token: draft.secret };
    case "package_npm": {
      const scopes = [
        ...new Set(
          draft.scopes
            .split(/[\s,]+/u)
            .map((scope) => scope.trim().toLowerCase())
            .filter(Boolean),
        ),
      ];
      return {
        registryUrl: draft.endpointUrl,
        token: draft.secret,
        ...(scopes.length === 0 ? {} : { scopes }),
      };
    }
    case "package_pypi":
      return {
        indexUrl: draft.endpointUrl,
        username: draft.username,
        token: draft.secret,
      };
  }
}

export function clearCredentialSecrets(draft: CredentialPayloadDraft): CredentialPayloadDraft {
  return {
    ...draft,
    jsonPayload: "",
    secret: "",
    privateKey: "",
    privateKeyPassphrase: "",
  };
}

export function isAdvancedProviderCredentialFormKind(kind: CredentialFormKind): boolean {
  return kind === "provider_advanced";
}

function buildManagedProviderPayload(draft: CredentialPayloadDraft): Record<string, unknown> {
  const baseUrl = draft.endpointUrl.trim();
  return {
    apiKey: draft.secret,
    ...(baseUrl === "" ? {} : { baseUrl }),
  };
}

function parseCredentialJSONPayload(value: string): Record<string, unknown> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new Error("Credential payload must be a valid JSON object.");
  }
  if (parsed === null || Array.isArray(parsed) || typeof parsed !== "object") {
    throw new Error("Credential payload must be a JSON object.");
  }
  if (Object.keys(parsed).length === 0) {
    throw new Error("Credential payload must not be empty.");
  }
  return parsed as Record<string, unknown>;
}

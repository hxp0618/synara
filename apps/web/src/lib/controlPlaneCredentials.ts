import type { ControlPlaneCredential, ControlPlaneCredentialPurpose } from "./controlPlaneClient";
import { normalizeControlPlaneProviderCode } from "./controlPlaneProviderCode";

export function listUsableControlPlaneCredentials(
  credentials: ReadonlyArray<ControlPlaneCredential>,
  options: {
    purpose: ControlPlaneCredentialPurpose;
    organizationId: string | null;
    userId?: string;
    model?: string | null;
    provider?: string;
    now?: number;
  },
): ReadonlyArray<ControlPlaneCredential> {
  const now = options.now ?? Date.now();
  return credentials.filter(
    (credential) =>
      credential.purpose === options.purpose &&
      credential.revokedAt === null &&
      (credential.expiresAt === null || Date.parse(credential.expiresAt) > now) &&
      credentialMatchesScope(credential, options) &&
      (options.provider === undefined ||
        normalizeControlPlaneProviderCode(credential.provider) ===
          normalizeControlPlaneProviderCode(options.provider)),
  );
}

function credentialMatchesScope(
  credential: ControlPlaneCredential,
  options: { organizationId: string | null; userId?: string; model?: string | null },
): boolean {
  switch (credential.scope) {
    case "user":
      return credential.scopeUserId !== null && credential.scopeUserId === options.userId;
    case "organization":
      return (
        credential.organizationId !== null && credential.organizationId === options.organizationId
      );
    case "tenant": {
      if (
        credential.selectorOrganizationId !== null &&
        credential.selectorOrganizationId !== options.organizationId
      ) {
        return false;
      }
      const model = options.model?.trim() || null;
      return credential.selectorModel === null || credential.selectorModel === model;
    }
    case "platform":
      return true;
  }
}

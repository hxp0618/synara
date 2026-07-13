import type { ControlPlaneCredential, ControlPlaneCredentialPurpose } from "./controlPlaneClient";

export function listUsableControlPlaneCredentials(
  credentials: ReadonlyArray<ControlPlaneCredential>,
  options: {
    purpose: ControlPlaneCredentialPurpose;
    organizationId: string | null;
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
      (credential.organizationId === null ||
        credential.organizationId === options.organizationId) &&
      (options.provider === undefined || credential.provider === options.provider),
  );
}
